package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// hijackClose forcibly closes the client connection without writing a
// response — the client sees a transport-level error (no HTTP status).
func hijackClose(w http.ResponseWriter) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		panic("test server does not support hijacking")
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		panic(err)
	}
	_ = conn.Close()
}

// TestDo_TransportError_NonIdempotentNotRetried is the regression for
// finding #1 of the api review: a transport error does NOT prove the
// server never processed the request (client timeout / reset can fire
// after the request was fully sent), so dedupe-key-less replay calls
// must not retry — that risked duplicate replay side effects.
func TestDo_TransportError_NonIdempotentNotRetried(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		hijackClose(w)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	ok, err := c.PostReplayStop(t.Context(), nil)
	if err == nil || ok {
		t.Fatalf("PostReplayStop = (%v, %v), want transport error", ok, err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("server hits = %d, want 1 (non-idempotent transport error must not retry)", got)
	}
}

// TestDo_TransportError_IdempotentRetried pins the counterpart: an
// idempotent, request_id-keyed recovery POST retries through a
// transport blip and succeeds.
func TestDo_TransportError_IdempotentRetried(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			hijackClose(w)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<?xml version="1.0"?><response response_code="OK"/>`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	ok, err := c.PostRecovery(t.Context(), "live", 42, nil, time.Time{})
	if err != nil || !ok {
		t.Fatalf("PostRecovery = (%v, %v), want success after retry", ok, err)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("server hits = %d, want 2", got)
	}
}

// TestDo_429_RetriedUnderBothPolicies pins finding #9: 429 means the
// server rate-limited WITHOUT processing, so it is retryable even for
// non-idempotent replay calls (other 4xx stay terminal).
func TestDo_429_RetriedUnderBothPolicies(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<?xml version="1.0"?><response response_code="OK"/>`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	ok, err := c.PostReplayStop(t.Context(), nil) // non-idempotent
	if err != nil || !ok {
		t.Fatalf("PostReplayStop = (%v, %v), want success after 429 retries", ok, err)
	}
	if got := hits.Load(); got != 3 {
		t.Fatalf("server hits = %d, want 3", got)
	}
}

// TestDo_Terminal404_TypedError pins finding #2: non-2xx failures carry
// a typed *Error reachable via errors.As, with the HTTP status and the
// server's structured message — and exactly one attempt (4xx terminal).
func TestDo_Terminal404_TypedError(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `<?xml version="1.0"?><response response_code="NOT_FOUND"><action>a</action><message>boom</message></response>`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, err := c.FetchWhoAmI(t.Context())
	if err == nil {
		t.Fatal("expected error")
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("errors.As(*api.Error) failed for %v", err)
	}
	if apiErr.Status != http.StatusNotFound || apiErr.Message != "boom" {
		t.Fatalf("apiErr = %+v, want Status=404 Message=boom", apiErr)
	}
	if !strings.Contains(err.Error(), "status 404: boom") {
		t.Fatalf("rendered error %q lost the legacy format", err.Error())
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("server hits = %d, want 1 (404 is terminal)", got)
	}
}

// TestDo_202_IsSuccess pins finding #5: any 2xx is success — 202 must
// not fall into the retryable server-error branch.
func TestDo_202_IsSuccess(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	ok, err := c.PostRecovery(t.Context(), "live", 7, nil, time.Time{})
	if err != nil || !ok {
		t.Fatalf("PostRecovery on 202 = (%v, %v), want success", ok, err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("server hits = %d, want 1", got)
	}
}

// TestAPIEvent_ErrRedacted pins finding #3: the emitted event's Err must
// not leak the query string that redactURL strips from the URL field
// (*url.Error renders the full URL), while errors.Is still traverses to
// the original cause via Unwrap.
func TestAPIEvent_ErrRedacted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hijackClose(w)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	events := make(chan APIEvent, 16)
	c.SetEventCapture(EventCapture{
		Emit:      func(ev APIEvent) { events <- ev },
		BodyLimit: 1 << 10,
	})

	_, _ = c.PostRecovery(t.Context(), "live", 4242, nil, time.Time{})

	select {
	case ev := <-events:
		if ev.Err == nil {
			t.Fatal("event Err is nil for a transport failure")
		}
		if strings.Contains(ev.Err.Error(), "request_id=") {
			t.Fatalf("event Err leaks the query string: %q", ev.Err.Error())
		}
		if !strings.Contains(ev.Err.Error(), "/recovery/initiate_request") {
			t.Fatalf("event Err lost the path context: %q", ev.Err.Error())
		}
	default:
		t.Fatal("no APIEvent emitted")
	}
}

// TestDo_NonOKEnvelope_EmitsError pins finding #4: a 200 whose decoded
// envelope carries a non-OK response_code previously emitted an APIEvent
// with Err=nil — indistinguishable from success — while the caller got
// an error.
func TestDo_NonOKEnvelope_EmitsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<?xml version="1.0"?>
<producers response_code="FORBIDDEN"/>`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	events := make(chan APIEvent, 16)
	c.SetEventCapture(EventCapture{
		Emit:      func(ev APIEvent) { events <- ev },
		BodyLimit: 1 << 10,
	})

	_, err := c.FetchProducers(t.Context())
	if err == nil {
		t.Fatal("expected non-OK envelope error")
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) || string(apiErr.Code) != "FORBIDDEN" {
		t.Fatalf("err = %v, want *api.Error with Code=FORBIDDEN", err)
	}
	select {
	case ev := <-events:
		if ev.Err == nil {
			t.Fatal("APIEvent.Err is nil for a non-OK envelope — looks like success on the event stream")
		}
	default:
		t.Fatal("no APIEvent emitted")
	}
}

// TestDo_ZeroMaxAttempts_UsesDefault pins finding #6's guard: a
// non-positive attempt budget must fall back to the default (3), not
// become backoff/v5's "unlimited".
func TestDo_ZeroMaxAttempts_UsesDefault(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	c.maxAttempts = 0
	_, err := c.FetchWhoAmI(t.Context())
	if err == nil {
		t.Fatal("expected error after retry exhaustion")
	}
	if got := hits.Load(); got != int32(defaultMaxAttempts) {
		t.Fatalf("server hits = %d, want %d (zero budget must mean default, not unlimited)", got, defaultMaxAttempts)
	}
}

// TestFetchDynamicVariant_PathEscaped pins finding #10: variant strings
// come from feed messages, so reserved characters must be path-escaped
// instead of misrouting the request or leaking into the query.
func TestFetchDynamicVariant_PathEscaped(t *testing.T) {
	var gotPath, gotQuery atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath.Store(r.URL.Path) // net/http decodes escapes here
		gotQuery.Store(r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<?xml version="1.0"?><market_descriptions response_code="OK"/>`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	variant := "od:dynamic_outcomes:1?x=1"
	if _, err := c.FetchMarketDescriptionsWithDynamicOutcomes(context.Background(), 5, variant, "en"); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if q := gotQuery.Load().(string); q != "" {
		t.Fatalf("query = %q, want empty (variant must not leak into the query string)", q)
	}
	if p := gotPath.Load().(string); !strings.HasSuffix(p, "/variants/od:dynamic_outcomes:1?x=1") {
		t.Fatalf("decoded path = %q, want the full variant as one segment", p)
	}
}

// TestAPIEvent_ErrChain_PreservesCtxSentinels pins the sanitized-chain
// contract's useful half: context classification survives sanitization
// so event consumers can errors.Is on cancellation, while nothing
// secret-bearing is retained.
func TestAPIEvent_ErrChain_PreservesCtxSentinels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hijackClose(w)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	events := make(chan APIEvent, 16)
	c.SetEventCapture(EventCapture{Emit: func(ev APIEvent) { events <- ev }, BodyLimit: 1 << 10})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.FetchWhoAmI(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("caller err = %v, want context.Canceled", err)
	}
	select {
	case ev := <-events:
		if ev.Err == nil {
			t.Fatal("event Err nil for cancelled call")
		}
		if !errors.Is(ev.Err, context.Canceled) {
			t.Fatalf("event err lost ctx classification: %v", ev.Err)
		}
	default:
		t.Fatal("no APIEvent emitted")
	}
}
