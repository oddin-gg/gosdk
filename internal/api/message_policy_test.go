package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/internal/apipolicy"
	"github.com/oddin-gg/gosdk/types"
)

const policySportsXML = `<?xml version="1.0"?><sports generated_at="2026-01-01T00:00:00"><sport id="od:sport:1" name="Soccer" abbreviation="SOC"/></sports>`

// The message path makes ONE attempt: the same failing endpoint takes three
// attempts under the default policy.
func TestDo_MessagePolicy_SingleAttemptNoRetries(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := newTestClient(t, srv)

	// Default policy: 3 attempts (1 + 2 retries).
	if _, err := c.FetchSports(t.Context(), types.EnLocale); err == nil {
		t.Fatal("expected an error")
	}
	if got := hits.Load(); got != 3 {
		t.Fatalf("default policy attempts = %d, want 3", got)
	}

	hits.Store(0)
	start := time.Now()
	ctx := apipolicy.With(t.Context(), apipolicy.Message)
	if _, err := c.FetchSports(ctx, types.EnLocale); err == nil {
		t.Fatal("expected an error")
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("message policy attempts = %d, want 1", got)
	}
	if time.Since(start) > 400*time.Millisecond {
		t.Fatalf("message-path failure took %v; it must not sit in the retry backoff", time.Since(start))
	}
}

// A hanging API costs the message path its short budget, not the client's
// 30 s request timeout.
func TestDo_MessagePolicy_ShortTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	c := newTestClient(t, srv)
	c.httpClient.Timeout = 30 * time.Second

	ctx := apipolicy.With(t.Context(), apipolicy.Message)
	start := time.Now()
	_, err := c.FetchSports(ctx, types.EnLocale)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if elapsed := time.Since(start); elapsed < apipolicy.MessageTimeout-200*time.Millisecond || elapsed > apipolicy.MessageTimeout+1500*time.Millisecond {
		t.Fatalf("message-path call took %v, want about %v", elapsed, apipolicy.MessageTimeout)
	}
}

// MessageSkip makes no request at all.
func TestDo_MessageSkipPolicy_NoRequest(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, policySportsXML)
	}))
	defer srv.Close()
	c := newTestClient(t, srv)

	ctx := apipolicy.With(t.Context(), apipolicy.MessageSkip)
	_, err := c.FetchSports(ctx, types.EnLocale)
	if !errors.Is(err, apipolicy.ErrSkipped) {
		t.Fatalf("err = %v, want apipolicy.ErrSkipped", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("requests = %d, want none", hits.Load())
	}
}

// A request that starts after the shared budget is spent is never sent and
// says so: ErrBudgetSpent (still a deadline error for callers that test for
// one), unlike a request that was sent and timed out.
func TestDo_MessagePolicy_SpentBudgetIsRefusedWithoutSending(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, policySportsXML)
	}))
	defer srv.Close()
	c := newTestClient(t, srv)

	ctx := apipolicy.With(t.Context(), apipolicy.Message)
	time.Sleep(apipolicy.MessageTimeout + 50*time.Millisecond)
	_, err := c.FetchSports(ctx, types.EnLocale)
	if !errors.Is(err, apipolicy.ErrBudgetSpent) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want ErrBudgetSpent and a deadline error", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("requests = %d, want none", hits.Load())
	}
}

// Any 2xx advances the success counter; failures do not.
func TestClient_SuccessGeneration(t *testing.T) {
	var fail atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, policySportsXML)
	}))
	defer srv.Close()
	c := newTestClient(t, srv)
	ctx := apipolicy.With(t.Context(), apipolicy.Message)

	g0 := c.SuccessGeneration()
	fail.Store(true)
	_, _ = c.FetchSports(ctx, types.EnLocale)
	if c.SuccessGeneration() != g0 {
		t.Fatal("a failed call advanced the success counter")
	}
	fail.Store(false)
	if _, err := c.FetchSports(ctx, types.EnLocale); err != nil {
		t.Fatal(err)
	}
	if c.SuccessGeneration() == g0 {
		t.Fatal("a successful call did not advance the success counter")
	}
}

// The 2 s budget covers reading the body too: a server that sends headers
// and part of the XML and then holds the rest must not keep the message path
// waiting (the response body is decoded after do() returns, under the same
// request context).
func TestDo_MessagePolicy_BudgetCoversTheBody(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `<?xml version="1.0"?><sports generated_at="2026-01-01T00:00:00"><sport id="od:sport:1" name="Soc`)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	c := newTestClient(t, srv)
	c.httpClient.Timeout = 30 * time.Second

	ctx := apipolicy.With(t.Context(), apipolicy.Message)
	start := time.Now()
	_, err := c.FetchSports(ctx, types.EnLocale)
	if err == nil {
		t.Fatal("expected an error for a body that never completes")
	}
	if elapsed := time.Since(start); elapsed > apipolicy.MessageTimeout+1500*time.Millisecond {
		t.Fatalf("message-path fetch took %v with a stalled body, want about %v", elapsed, apipolicy.MessageTimeout)
	}
}

// Requests made under one message-path context share ONE budget: the second
// of two sequential requests gets only what the first left, and a request
// that starts after the budget is spent never goes out.
func TestDo_MessagePolicy_RequestsShareOneBudget(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case <-time.After(1500 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, policySportsXML)
	}))
	defer srv.Close()
	c := newTestClient(t, srv)
	c.httpClient.Timeout = 30 * time.Second

	ctx := apipolicy.With(t.Context(), apipolicy.Message)
	start := time.Now()
	if _, err := c.FetchSports(ctx, types.EnLocale); err != nil {
		t.Fatalf("first request: %v", err)
	}
	// 0.5 s of the budget is left: the second request is cut off there.
	if _, err := c.FetchSports(ctx, types.EnLocale); err == nil {
		t.Fatal("second request succeeded beyond the shared budget")
	}
	if elapsed := time.Since(start); elapsed > apipolicy.MessageTimeout+500*time.Millisecond {
		t.Fatalf("two requests took %v, want about the %v budget", elapsed, apipolicy.MessageTimeout)
	}
	// Budget spent: a third request makes no call at all.
	before := hits.Load()
	if _, err := c.FetchSports(ctx, types.EnLocale); err == nil {
		t.Fatal("request after the budget succeeded")
	}
	if hits.Load() != before {
		t.Fatal("a request went out after the budget was spent")
	}
}
