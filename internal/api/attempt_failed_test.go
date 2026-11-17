package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/oddin-gg/gosdk/internal/apipolicy"
	"github.com/oddin-gg/gosdk/types"
)

// partialBody answers with status and a body that ends before its declared
// length, so the client's read of it fails.
func partialBody(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, "<error><message>cut")
	}
}

// A non-2xx answer whose body is then cut off is an unreadable body like a
// truncated 2xx one: it opens the endpoint's breaker. A complete one of the
// same status does not.
func TestBreaker_ANon2xxBodyThatIsCutOffOpensIt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		want    bool
	}{
		{"500 cut off", partialBody(http.StatusInternalServerError), true},
		{"404 cut off", partialBody(http.StatusNotFound), true},
		{"429 cut off", partialBody(http.StatusTooManyRequests), true},
		{"500 complete", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "<error/>", http.StatusInternalServerError)
		}, false},
		{"404 complete", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "<error/>", http.StatusNotFound) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			c := newTestClient(t, srv)
			c.maxAttempts = 1
			if _, err := c.FetchSports(t.Context(), types.EnLocale); err == nil {
				t.Fatal("expected an error")
			}
			if got := c.Breaker().Open(apipolicy.EndpointStatic); got != tc.want {
				t.Fatalf("open = %v, want %v", got, tc.want)
			}
		})
	}
}

// A body cut short because the caller gave up says nothing about the API.
func TestBreaker_ANon2xxBodyCutByTheCallersCancellationDoesNotOpenIt(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "<error>")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer srv.Close()
	c := newTestClient(t, srv)
	c.maxAttempts = 1
	ctx, cancel := contextWithCancel(t)
	go func() { <-started; cancel() }()
	if _, err := c.FetchSports(ctx, types.EnLocale); err == nil {
		t.Fatal("expected an error")
	}
	if c.Breaker().Open(apipolicy.EndpointStatic) {
		t.Fatal("the caller's cancellation opened the breaker")
	}
}

// The request's context is told of each failed attempt that is followed by a
// retry, and of nothing else: not of an answer about the request (404, 400),
// and not of the terminal failure of a mutation that must not be repeated.
func TestAttemptFailed_IsReportedForRetriedFailuresOnly(t *testing.T) {
	status := func(code int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { http.Error(w, "<error/>", code) }
	}
	hijack := func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}
	cases := []struct {
		name       string
		handler    http.HandlerFunc
		idempotent bool
		mutation   bool
		want       int32
	}{
		{"GET transport error", hijack, true, false, 1},
		{"GET 429", status(http.StatusTooManyRequests), true, false, 1},
		{"GET 500", status(http.StatusInternalServerError), true, false, 1},
		{"GET 404", status(http.StatusNotFound), true, false, 0},
		{"GET 400", status(http.StatusBadRequest), true, false, 0},
		// A non-idempotent mutation is not retried after a 5xx or a transport
		// error: no retry follows, so no failed attempt is reported.
		{"non-idempotent POST 502", status(http.StatusBadGateway), false, true, 0},
		{"non-idempotent POST transport error", hijack, false, true, 0},
		// A 429 is retried under both policies.
		{"non-idempotent POST 429", status(http.StatusTooManyRequests), false, true, 1},
		{"idempotent POST 502", status(http.StatusBadGateway), true, true, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			c := newTestClient(t, srv)
			c.maxAttempts = 1
			var fired atomic.Int32
			ctx := apipolicy.WithAttemptFailed(t.Context(), func() { fired.Add(1) })
			var err error
			if tc.mutation {
				_, err = c.postEmpty(ctx, "/x", tc.idempotent)
			} else {
				_, err = c.FetchSports(ctx, types.EnLocale)
			}
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := fired.Load(); got != tc.want {
				t.Fatalf("failed attempts reported = %d, want %d", got, tc.want)
			}
		})
	}
}

func contextWithCancel(t *testing.T) (context.Context, context.CancelFunc) {
	return context.WithCancel(t.Context())
}
