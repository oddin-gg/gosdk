package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/oddin-gg/gosdk/internal/apipolicy"
	"github.com/oddin-gg/gosdk/types"
)

// Which failures say that the API answered for the key and failed.
func TestKeyFailure_Classification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"404", &Error{Status: http.StatusNotFound}, false},
		{"200 with NOT_FOUND", &Error{Status: http.StatusOK, Code: types.NotFoundResponseCode}, false},
		{"500", &Error{Status: http.StatusInternalServerError}, true},
		{"429", &Error{Status: http.StatusTooManyRequests}, true},
		{"wrapped 500", fmt.Errorf("gave up: %w", &Error{Status: http.StatusInternalServerError}), true},
		{"bad response", badResponse(fmt.Errorf("truncated")), true},
		{"wrapped bad response", fmt.Errorf("fetch: %w", badResponse(fmt.Errorf("truncated"))), true},
		{"canceled", context.Canceled, false},
		{"deadline", context.DeadlineExceeded, false},
		{"plain", fmt.Errorf("connection refused"), false},
	}
	for _, tc := range cases {
		if got := apipolicy.KeyFailure(tc.err); got != tc.want {
			t.Errorf("%s: KeyFailure = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A non-2xx answer whose envelope says SERVICE_UNAVAILABLE opens the
// endpoint's breaker like the same envelope on a 2xx does; a plain 500 does not.
func TestBreaker_ANon2xxServiceUnavailableEnvelopeOpensIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"service unavailable envelope", `<response response_code="SERVICE_UNAVAILABLE"/>`, true},
		{"plain 500", `<error/>`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newBodyServer(tc.body, http.StatusInternalServerError)
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

func newBodyServer(body string, status int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
}
