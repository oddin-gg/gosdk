package api

// Regression tests for the review-driven correctness pass in client.go +
// error.go: idempotency-aware retry policy, 429 handling, 2xx success
// range, typed *Error surfaces, APIEvent.Err redaction, non-OK envelope
// emission, maxAttempts fallback, error-body cap, and path escaping.

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/types"
)

// eventRecorder collects APIEvents. Emission is synchronous with the
// API call, so all() is complete once the call under test returns; the
// mutex keeps it -race clean regardless.
type eventRecorder struct {
	mu     sync.Mutex
	events []APIEvent
}

func (r *eventRecorder) emit(ev APIEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *eventRecorder) all() []APIEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]APIEvent(nil), r.events...)
}

func mustURN(t *testing.T, s string) types.URN {
	t.Helper()
	u, err := types.ParseURN(s)
	if err != nil {
		t.Fatalf("ParseURN(%s): %v", s, err)
	}
	return *u
}

// hijackAndClose kills the connection without writing an HTTP response,
// producing a transport-level error on the client side.
func hijackAndClose(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	hj, ok := w.(http.Hijacker)
	if !ok {
		t.Error("hijacker not supported")
		return
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		t.Errorf("hijack: %v", err)
		return
	}
	_ = conn.Close()
}

// TestClient_TransportError_NonIdempotentNoRetry pins finding 1: replay
// POST/PUT/DELETE carry no dedupe key, so a transport failure (which may
// occur AFTER the server processed the request) must be terminal —
// exactly one attempt reaches the server and the error surfaces.
func TestClient_TransportError_NonIdempotentNoRetry(t *testing.T) {
	tests := []struct {
		name string
		call func(t *testing.T, c *Client) (bool, error)
	}{
		{"PostReplayStop", func(t *testing.T, c *Client) (bool, error) {
			return c.PostReplayStop(t.Context(), nil)
		}},
		{"PostReplayClear", func(t *testing.T, c *Client) (bool, error) {
			return c.PostReplayClear(t.Context(), nil)
		}},
		{"PutReplayEvent", func(t *testing.T, c *Client) (bool, error) {
			return c.PutReplayEvent(t.Context(), mustURN(t, "od:match:42"), nil)
		}},
		{"DeleteReplayEvent", func(t *testing.T, c *Client) (bool, error) {
			return c.DeleteReplayEvent(t.Context(), mustURN(t, "od:match:42"), nil)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var attempts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				hijackAndClose(t, w)
			}))
			defer srv.Close()

			c := newTestClient(t, srv)
			ok, err := tc.call(t, c)
			if err == nil {
				t.Fatal("expected transport error, got nil")
			}
			if ok {
				t.Error("expected ok=false on transport error")
			}
			if got := attempts.Load(); got != 1 {
				t.Fatalf("attempts = %d, want 1 (non-idempotent request must not retry)", got)
			}
		})
	}
}

// TestClient_TransportError_IdempotentPostRetries pins the flip side of
// finding 1: recovery POSTs are request_id-keyed (server dedupes), so a
// transport error IS retried and the call succeeds once the server
// recovers.
func TestClient_TransportError_IdempotentPostRetries(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) < 2 {
			hijackAndClose(t, w)
			return
		}
		_, _ = io.WriteString(w, `<empty/>`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	ok, err := c.PostRecovery(t.Context(), "live", 1234, nil, time.Time{})
	if err != nil {
		t.Fatalf("expected success after transport-error retry, got %v", err)
	}
	if !ok {
		t.Error("expected ok=true")
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2 (idempotent POST retries transport errors)", got)
	}
}

// TestClient_429RetriedUnderBothPolicies pins finding 2: 429 means the
// server rate-limited the request WITHOUT processing it, so it is
// retried even for non-idempotent replay POSTs.
func TestClient_429RetriedUnderBothPolicies(t *testing.T) {
	tests := []struct {
		name    string
		success string
		call    func(t *testing.T, c *Client) error
	}{
		{
			name:    "idempotent GET",
			success: `<producers response_code="OK"/>`,
			call: func(t *testing.T, c *Client) error {
				_, err := c.FetchProducers(t.Context())
				return err
			},
		},
		{
			name:    "non-idempotent replay POST",
			success: `<empty/>`,
			call: func(t *testing.T, c *Client) error {
				_, err := c.PostReplayStop(t.Context(), nil)
				return err
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var attempts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if attempts.Add(1) < 3 {
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = io.WriteString(w, `<response response_code="TOO_MANY_REQUESTS"><action>retry</action><message>slow down</message></response>`)
					return
				}
				_, _ = io.WriteString(w, tc.success)
			}))
			defer srv.Close()

			c := newTestClient(t, srv)
			if err := tc.call(t, c); err != nil {
				t.Fatalf("expected success after 429 retries, got %v", err)
			}
			if got := attempts.Load(); got != 3 {
				t.Fatalf("attempts = %d, want 3 (429 twice, then success)", got)
			}
		})
	}
}

// TestClient_404TerminalUnderBothPolicies pins the other half of finding
// 2: non-429 4xx stays terminal after exactly one attempt under both
// retry policies, and surfaces a typed *Error.
func TestClient_404TerminalUnderBothPolicies(t *testing.T) {
	tests := []struct {
		name string
		call func(t *testing.T, c *Client) error
	}{
		{
			name: "idempotent GET",
			call: func(t *testing.T, c *Client) error {
				_, err := c.FetchProducers(t.Context())
				return err
			},
		},
		{
			name: "non-idempotent replay POST",
			call: func(t *testing.T, c *Client) error {
				_, err := c.PostReplayStop(t.Context(), nil)
				return err
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var attempts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `<response response_code="NOT_FOUND"><action>none</action><message>missing</message></response>`)
			}))
			defer srv.Close()

			c := newTestClient(t, srv)
			err := tc.call(t, c)
			if err == nil {
				t.Fatal("expected 404 error, got nil")
			}
			var apiErr *Error
			if !errors.As(err, &apiErr) {
				t.Fatalf("errors.As(*api.Error) failed for %v", err)
			}
			if apiErr.Status != http.StatusNotFound {
				t.Errorf("Status = %d, want 404", apiErr.Status)
			}
			if got := attempts.Load(); got != 1 {
				t.Fatalf("attempts = %d, want 1 (404 is terminal)", got)
			}
		})
	}
}

// TestClient_2xxStatusesAreSuccess pins finding 3: any 2xx (202, 204) is
// success — fetch decodes the body when present, doNoBody returns true —
// and is never routed into the server-error retry branch.
func TestClient_2xxStatusesAreSuccess(t *testing.T) {
	producers := `<producers response_code="OK"><producer id="1" name="LO" description="" active="true" api_url="" producer_scopes="live"/></producers>`
	tests := []struct {
		name   string
		status int
		body   string
		call   func(t *testing.T, c *Client) error
	}{
		{
			name:   "fetch 202 decodes body",
			status: http.StatusAccepted,
			body:   producers,
			call: func(t *testing.T, c *Client) error {
				prods, err := c.FetchProducers(t.Context())
				if err == nil && len(prods) != 1 {
					t.Errorf("decoded %d producers, want 1", len(prods))
				}
				return err
			},
		},
		{
			name:   "doNoBody 202",
			status: http.StatusAccepted,
			body:   `<empty/>`,
			call: func(t *testing.T, c *Client) error {
				ok, err := c.PostReplayStop(t.Context(), nil)
				if err == nil && !ok {
					t.Error("expected ok=true")
				}
				return err
			},
		},
		{
			name:   "doNoBody 204 without body",
			status: http.StatusNoContent,
			body:   "",
			call: func(t *testing.T, c *Client) error {
				ok, err := c.PostReplayStop(t.Context(), nil)
				if err == nil && !ok {
					t.Error("expected ok=true")
				}
				return err
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var attempts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				w.WriteHeader(tc.status)
				if tc.body != "" {
					_, _ = io.WriteString(w, tc.body)
				}
			}))
			defer srv.Close()

			c := newTestClient(t, srv)
			if err := tc.call(t, c); err != nil {
				t.Fatalf("expected %d to be success, got %v", tc.status, err)
			}
			if got := attempts.Load(); got != 1 {
				t.Fatalf("attempts = %d, want 1 (2xx must not be retried)", got)
			}
		})
	}
}

// TestClient_TypedError_StructuredBody pins finding 4: a 4xx with a
// structured error envelope yields a typed *Error reachable via
// errors.As, with the server message decoded and the historical string
// format preserved.
func TestClient_TypedError_StructuredBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `<response response_code="NOT_FOUND"><action>none</action><message>boom</message></response>`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, err := c.FetchWhoAmI(t.Context())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("errors.As(*api.Error) failed for %v", err)
	}
	if apiErr.Status != http.StatusNotFound {
		t.Errorf("Status = %d, want 404", apiErr.Status)
	}
	if apiErr.Message != "boom" {
		t.Errorf("Message = %q, want %q", apiErr.Message, "boom")
	}
	// The envelope response_code must be carried on a NON-2xx error too, so
	// consumers can classify it — pre-fix Code was only set on 2xx-envelope
	// rejections and stayed empty here.
	if apiErr.Code != types.NotFoundResponseCode {
		t.Errorf("Code = %q, want %q", apiErr.Code, types.NotFoundResponseCode)
	}
	if apiErr.Method != http.MethodGet {
		t.Errorf("Method = %q, want GET", apiErr.Method)
	}
	// The human-readable string format is preserved (status-first, no code)
	// despite Code now being populated.
	const want = "api: GET /users/whoami: status 404: boom"
	if got := apiErr.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// TestClient_TypedError_BareErrorEnvelope pins the second half of the P2
// finding: a 4xx/5xx carrying the bare <error><message>…</message></error>
// shape must still surface the server message. Pre-fix the pinned
// <response> decoder rejected this root and Message stayed empty.
func TestClient_TypedError_BareErrorEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `<error><message>upstream exploded</message></error>`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, err := c.FetchWhoAmI(t.Context())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("errors.As(*api.Error) failed for %v", err)
	}
	if apiErr.Status != http.StatusInternalServerError {
		t.Errorf("Status = %d, want 500", apiErr.Status)
	}
	if apiErr.Message != "upstream exploded" {
		t.Errorf("Message = %q, want %q (bare <error> body must decode)", apiErr.Message, "upstream exploded")
	}
}

// TestClient_TypedError_NonOKEnvelope pins the response_code half of
// finding 4: a 200 whose decoded envelope carries a non-OK response_code
// yields a typed *Error with Code set.
func TestClient_TypedError_NonOKEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `<bookmaker_details response_code="FORBIDDEN" expire_at="2030-01-02T15:04:05" bookmaker_id="1" virtual_host="/vhost"/>`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, err := c.FetchWhoAmI(t.Context())
	if err == nil {
		t.Fatal("expected non-OK envelope error, got nil")
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("errors.As(*api.Error) failed for %v", err)
	}
	if apiErr.Code != types.ForbiddenResponseCode {
		t.Errorf("Code = %q, want %q", apiErr.Code, types.ForbiddenResponseCode)
	}
	if apiErr.Status != http.StatusOK {
		t.Errorf("Status = %d, want 200", apiErr.Status)
	}
	const want = "api: not acceptable response code from /users/whoami: FORBIDDEN"
	if got := apiErr.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// TestClient_APIEventErr_StripsQueryString pins finding 5: the emitted
// APIEvent.Err has the request's query string scrubbed from its message
// (matching what redactURL does for APIEvent.URL) while Unwrap keeps the
// original chain reachable for errors.Is/As.
func TestClient_APIEventErr_StripsQueryString(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hijackAndClose(t, w)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	c.maxAttempts = 1 // one attempt is enough to observe the event
	rec := &eventRecorder{}
	c.SetEventCapture(EventCapture{Emit: rec.emit})

	_, err := c.PostRecovery(t.Context(), "live", 987654, nil, time.Time{})
	if err == nil {
		t.Fatal("expected transport error, got nil")
	}

	events := rec.all()
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	ev := events[0]
	if ev.Err == nil {
		t.Fatal("APIEvent.Err = nil, want transport error")
	}
	if ev.Status != 0 {
		t.Errorf("APIEvent.Status = %d, want 0 (transport error)", ev.Status)
	}
	if msg := ev.Err.Error(); strings.Contains(msg, "request_id=") {
		t.Errorf("APIEvent.Err leaked query string: %q", msg)
	}
	if strings.Contains(ev.URL, "?") || strings.Contains(ev.URL, "request_id") {
		t.Errorf("APIEvent.URL leaked query string: %q", ev.URL)
	}
	// The raw *url.Error must NOT be reachable through Unwrap — its
	// Error() renders the full URL including the query string, so
	// retaining it in the chain would let any consumer unwrap straight
	// past the redaction. (This test previously pinned the opposite.)
	var urlErr *url.Error
	if errors.As(ev.Err, &urlErr) {
		t.Fatalf("event err chain retains *url.Error rendering %q — redaction bypassable via Unwrap", urlErr.Error())
	}
}

// TestClient_APIEventErr_RedactsTokenFromErrorBody pins the token half
// of finding 5, extended by the at-source redaction (Codex P2): a
// token echoed in a server error message is replaced with [REDACTED]
// both in the event's Err text AND in the caller-facing
// APIError.Message — the caller-facing error travels into consumer
// logs, recovery handle results, and recovery's own error logging, so
// it must never carry the credential. (This test previously pinned the
// opposite: the raw token in the caller-facing Message.)
func TestClient_APIEventErr_RedactsTokenFromErrorBody(t *testing.T) {
	const tok = "super-secret-token-0123456789abcdef"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `<response response_code="NOT_FOUND"><action>none</action><message>invalid token `+tok+`</message></response>`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("bad server url: %v", err)
	}
	c.cfg = &testConfig{apiURL: u.Host, token: tok}
	rec := &eventRecorder{}
	c.SetEventCapture(EventCapture{Emit: rec.emit})

	_, callErr := c.FetchWhoAmI(t.Context())
	if callErr == nil {
		t.Fatal("expected 404 error, got nil")
	}
	// Caller-facing error is redacted AT SOURCE — classification kept.
	var apiErr *Error
	if !errors.As(callErr, &apiErr) {
		t.Fatalf("errors.As(*api.Error) failed for %v", callErr)
	}
	if strings.Contains(apiErr.Message, tok) {
		t.Errorf("caller-facing Message carries the access token: %q", apiErr.Message)
	}
	if !strings.Contains(apiErr.Message, "[REDACTED]") {
		t.Errorf("caller-facing Message missing redaction marker: %q", apiErr.Message)
	}
	if apiErr.Status != http.StatusNotFound {
		t.Errorf("redaction disturbed classification: Status = %d, want 404", apiErr.Status)
	}

	events := rec.all()
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	ev := events[0]
	if ev.Err == nil {
		t.Fatal("APIEvent.Err = nil, want 404 error")
	}
	if msg := ev.Err.Error(); strings.Contains(msg, tok) {
		t.Errorf("APIEvent.Err leaked access token: %q", msg)
	}
	if msg := ev.Err.Error(); !strings.Contains(msg, "[REDACTED]") {
		t.Errorf("APIEvent.Err missing [REDACTED] marker: %q", msg)
	}
	// errors.As traverses to a SANITIZED *Error copy through Unwrap —
	// classification survives, secrets don't. (This test previously
	// pinned the opposite: the raw error in the chain, which let event
	// consumers unwrap their way past the redaction.)
	var evAPIErr *Error
	if !errors.As(ev.Err, &evAPIErr) {
		t.Fatalf("errors.As(*api.Error) failed for event err %v", ev.Err)
	}
	if strings.Contains(evAPIErr.Message, tok) {
		t.Errorf("unwrapped event error leaks the token: %q", evAPIErr.Message)
	}
	if !strings.Contains(evAPIErr.Message, "[REDACTED]") {
		t.Errorf("unwrapped event error missing [REDACTED] marker: %q", evAPIErr.Message)
	}
	if evAPIErr.Status != http.StatusNotFound {
		t.Errorf("unwrapped event error lost classification: Status = %d, want 404", evAPIErr.Status)
	}
}

// TestClient_NonOKEnvelope_EmitsEventErr pins finding 6: a 200 whose
// envelope carries a non-OK response_code emits an APIEvent with a
// non-nil Err (pre-fix it emitted Err=nil, indistinguishable from
// success on the observability stream).
func TestClient_NonOKEnvelope_EmitsEventErr(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `<bookmaker_details response_code="FORBIDDEN" expire_at="2030-01-02T15:04:05" bookmaker_id="1" virtual_host="/vhost"/>`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	rec := &eventRecorder{}
	c.SetEventCapture(EventCapture{Emit: rec.emit})

	if _, err := c.FetchWhoAmI(t.Context()); err == nil {
		t.Fatal("expected non-OK envelope error, got nil")
	}

	events := rec.all()
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	ev := events[0]
	if ev.Status != http.StatusOK {
		t.Errorf("APIEvent.Status = %d, want 200", ev.Status)
	}
	if ev.Err == nil {
		t.Fatal("APIEvent.Err = nil for non-OK envelope, want error")
	}
	var apiErr *Error
	if !errors.As(ev.Err, &apiErr) {
		t.Fatalf("errors.As(*api.Error) failed for event err %v", ev.Err)
	}
	if apiErr.Code != types.ForbiddenResponseCode {
		t.Errorf("Code = %q, want %q", apiErr.Code, types.ForbiddenResponseCode)
	}
}

// TestClient_MaxAttemptsNonPositive_DefaultsToThree pins finding 7:
// maxAttempts <= 0 must fall back to the default 3-attempt budget —
// backoff/v5 treats WithMaxTries(0) as unlimited, which would retry a
// persistent 5xx forever.
func TestClient_MaxAttemptsNonPositive_DefaultsToThree(t *testing.T) {
	tests := []struct {
		name        string
		maxAttempts int
	}{
		{"zero", 0},
		{"negative", -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var attempts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `<response response_code="SERVICE_UNAVAILABLE"><action>none</action><message>down</message></response>`)
			}))
			defer srv.Close()

			c := newTestClient(t, srv)
			c.maxAttempts = tc.maxAttempts
			_, err := c.FetchProducers(t.Context())
			if err == nil {
				t.Fatal("expected error after exhausting retries, got nil")
			}
			if got := attempts.Load(); got != 3 {
				t.Fatalf("attempts = %d, want 3 (default budget)", got)
			}
			if !strings.Contains(err.Error(), "gave up after 3 attempt(s)") {
				t.Errorf("error %q missing attempt-count context", err.Error())
			}
			// 5xx envelope message must survive into the surfaced error.
			if !strings.Contains(err.Error(), "down") {
				t.Errorf("error %q lost the 5xx envelope message", err.Error())
			}
		})
	}
}

// TestClient_ErrorBodyCap_OversizedBody pins finding 8: a 4xx body
// larger than maxErrorBodyBytes doesn't blow up the client — the read is
// capped, the call terminates after one attempt, and the typed error
// still carries the status and the (early, within-cap) structured
// message.
func TestClient_ErrorBodyCap_OversizedBody(t *testing.T) {
	envelope := `<response response_code="NOT_FOUND"><action>none</action><message>boom</message></response>`
	padding := strings.Repeat(" ", maxErrorBodyBytes/2)
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, envelope)
		// Total body ~1.5 MiB > maxErrorBodyBytes; writes past the
		// client's capped read may fail once it closes the body — fine.
		for i := 0; i < 3; i++ {
			if _, err := io.WriteString(w, padding); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, err := c.FetchWhoAmI(t.Context())
	if err == nil {
		t.Fatal("expected 404 error, got nil")
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("errors.As(*api.Error) failed for %v", err)
	}
	if apiErr.Status != http.StatusNotFound {
		t.Errorf("Status = %d, want 404", apiErr.Status)
	}
	if apiErr.Message != "boom" {
		t.Errorf("Message = %q, want %q (envelope within cap must decode)", apiErr.Message, "boom")
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1", got)
	}
}

// TestClient_PathEscape_DynamicMarketVariant pins finding 9: a feed-
// originated variant containing '?' and a space must be path-escaped so
// it can't be misread as a query string or misroute the request.
func TestClient_PathEscape_DynamicMarketVariant(t *testing.T) {
	const variant = "od:variant?x=1 y"
	type seenReq struct {
		path     string
		rawQuery string
		escaped  string
	}
	seenCh := make(chan seenReq, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenCh <- seenReq{path: r.URL.Path, rawQuery: r.URL.RawQuery, escaped: r.URL.EscapedPath()}
		_, _ = io.WriteString(w, `<market_descriptions response_code="OK"/>`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	if _, err := c.FetchMarketDescriptionsWithDynamicOutcomes(t.Context(), 123, variant, types.EnLocale); err != nil {
		t.Fatalf("FetchMarketDescriptionsWithDynamicOutcomes: %v", err)
	}

	seen := <-seenCh
	const wantPath = "/v1/descriptions/en/markets/123/variants/od:variant?x=1 y"
	if seen.path != wantPath {
		t.Errorf("decoded path = %q, want %q", seen.path, wantPath)
	}
	if seen.rawQuery != "" {
		t.Errorf("query string = %q, want empty ('?' must be escaped into the path)", seen.rawQuery)
	}
	if !strings.Contains(seen.escaped, "%3F") {
		t.Errorf("escaped path %q does not contain %%3F for '?'", seen.escaped)
	}
}

// TestClient_DoNoBody_ReadErrorIsUnverified is the regression for the
// unverifiable-2xx P2: a 2xx doNoBody response whose body errors
// mid-read leaves acceptance UNVERIFIED — the body could have carried a
// rejection envelope we never saw. It must return (false, error), not
// (true, nil): otherwise recovery hands out a live handle and replay
// claims acceptance for a possibly-rejected request. The event still
// fires with the error. (This test previously pinned the opposite —
// (true, nil) with the drain error only on the event.)
func TestClient_DoNoBody_ReadErrorIsUnverified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Declare more bytes than we write; the server kills the
		// connection when the handler returns short, so the client's
		// body read fails mid-stream.
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "abc")
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	rec := &eventRecorder{}
	c.SetEventCapture(EventCapture{Emit: rec.emit})

	ok, err := c.PostReplayStop(t.Context(), nil)
	if err == nil {
		t.Fatal("unreadable 2xx body must be reported as unverified, got nil error")
	}
	if ok {
		t.Fatal("expected ok=false for an unverified acceptance")
	}
	if !strings.Contains(err.Error(), "unverified") {
		t.Errorf("err = %v, want an 'acceptance unverified' description", err)
	}
	events := rec.all()
	if len(events) != 1 || events[0].Err == nil {
		t.Fatalf("want exactly 1 event carrying the error, got %d", len(events))
	}
}

// TestClient_DoNoBody_OversizedBodyIsUnverified pins the cap-overflow
// half of the same P2: a 2xx body larger than the internal cap can't be
// scanned for a rejection envelope, so acceptance is unverified and the
// call must fail rather than falsely report success.
func TestClient_DoNoBody_OversizedBodyIsUnverified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// A padded FORBIDDEN envelope larger than maxErrorBodyBytes.
		_, _ = io.WriteString(w, `<response response_code="FORBIDDEN"><message>no</message></response>`)
		pad := make([]byte, maxErrorBodyBytes+1024)
		for i := range pad {
			pad[i] = ' '
		}
		_, _ = w.Write(pad)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	ok, err := c.PostReplayStop(t.Context(), nil)
	if err == nil || ok {
		t.Fatalf("oversized 2xx body must be unverified, got ok=%v err=%v", ok, err)
	}
	if !strings.Contains(err.Error(), "exceeds") && !strings.Contains(err.Error(), "could not be verified") {
		t.Errorf("err = %v, want a cap-overflow description", err)
	}
}

// --- previously-untested endpoint happy paths ---

func TestClient_FetchWhoAmI_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/users/whoami" {
			t.Errorf("path = %s, want /v1/users/whoami", r.URL.Path)
		}
		_, _ = io.WriteString(w, `<bookmaker_details response_code="OK" expire_at="2030-01-02T15:04:05" bookmaker_id="123" virtual_host="/vhost"/>`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	resp, err := c.FetchWhoAmI(t.Context())
	if err != nil {
		t.Fatalf("FetchWhoAmI: %v", err)
	}
	if resp.BookmakerID != 123 {
		t.Errorf("BookmakerID = %d, want 123", resp.BookmakerID)
	}
	if resp.VirtualHost != "/vhost" {
		t.Errorf("VirtualHost = %q, want /vhost", resp.VirtualHost)
	}
	if resp.Code() != types.OkResponseCode {
		t.Errorf("Code() = %q, want OK", resp.Code())
	}
}

func TestClient_FetchReplayStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/replay/status" {
			t.Errorf("path = %s, want /v1/replay/status", r.URL.Path)
		}
		if got := r.URL.Query().Get("node_id"); got != "9" {
			t.Errorf("node_id = %q, want 9", got)
		}
		_, _ = io.WriteString(w, `<player_status status="playing"/>`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	node := 9
	status, err := c.FetchReplayStatus(t.Context(), &node)
	if err != nil {
		t.Fatalf("FetchReplayStatus: %v", err)
	}
	if status != "playing" {
		t.Errorf("status = %q, want playing", status)
	}
}

func TestClient_PutAndDeleteReplayEvent(t *testing.T) {
	tests := []struct {
		name   string
		method string
		call   func(t *testing.T, c *Client) (bool, error)
	}{
		{"PutReplayEvent", http.MethodPut, func(t *testing.T, c *Client) (bool, error) {
			node := 7
			return c.PutReplayEvent(t.Context(), mustURN(t, "od:match:42"), &node)
		}},
		{"DeleteReplayEvent", http.MethodDelete, func(t *testing.T, c *Client) (bool, error) {
			node := 7
			return c.DeleteReplayEvent(t.Context(), mustURN(t, "od:match:42"), &node)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != tc.method {
					t.Errorf("method = %s, want %s", r.Method, tc.method)
				}
				if r.URL.Path != "/v1/replay/events/od:match:42" {
					t.Errorf("path = %s, want /v1/replay/events/od:match:42", r.URL.Path)
				}
				if got := r.URL.Query().Get("node_id"); got != "7" {
					t.Errorf("node_id = %q, want 7", got)
				}
				_, _ = io.WriteString(w, `<empty/>`)
			}))
			defer srv.Close()

			c := newTestClient(t, srv)
			ok, err := tc.call(t, c)
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if !ok {
				t.Fatal("expected ok=true")
			}
		})
	}
}

func TestClient_FetchReplaySetContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/replay" {
			t.Errorf("path = %s, want /v1/replay", r.URL.Path)
		}
		if r.URL.RawQuery != "" {
			t.Errorf("query = %q, want empty (nil nodeID)", r.URL.RawQuery)
		}
		_, _ = io.WriteString(w, `<replay_set_content><replay_event id="od:match:1" position="1"/><replay_event id="od:match:2" ref_id="sr:match:9" position="2"/></replay_set_content>`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	events, err := c.FetchReplaySetContent(t.Context(), nil)
	if err != nil {
		t.Fatalf("FetchReplaySetContent: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d replay events, want 2", len(events))
	}
	if events[0].ID != "od:match:1" || events[1].RefID != "sr:match:9" {
		t.Errorf("unexpected replay events decoded: %+v", events)
	}
}

// TestClient_APIEvents_LocaleSnapshotPerAttempt pins snapshot ownership
// on APIEvent.Locale (Codex P3): all retry attempts of one call shared
// a single *types.Locale pointee, so a consumer mutating attempt 1's
// event Locale while the call backed off changed what later attempts'
// events (and already-delivered historical events) reported — an
// aliasing race on observability metadata. Every emitted event must
// carry its own copy.
func TestClient_APIEvents_LocaleSnapshotPerAttempt(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		hijackAndClose(t, w) // transport error every attempt → retries
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	c.maxAttempts = 2
	rec := &eventRecorder{}
	c.SetEventCapture(EventCapture{Emit: rec.emit})

	_, err := c.FetchSports(t.Context(), types.EnLocale)
	if err == nil {
		t.Fatal("expected transport failure")
	}

	events := rec.all()
	if len(events) < 2 {
		t.Fatalf("got %d events, want >= 2 (one per attempt)", len(events))
	}
	seen := map[*types.Locale]struct{}{}
	for i, ev := range events {
		if ev.Locale == nil {
			t.Fatalf("event %d has nil Locale", i)
		}
		if *ev.Locale != types.EnLocale {
			t.Fatalf("event %d Locale = %q, want %q", i, *ev.Locale, types.EnLocale)
		}
		if _, dup := seen[ev.Locale]; dup {
			t.Fatal("two events share one Locale pointee — mutation on one aliases the other")
		}
		seen[ev.Locale] = struct{}{}
	}
	// Mutating one event's pointee must not leak into any other event.
	*events[0].Locale = types.Locale("xx")
	if *events[1].Locale != types.EnLocale {
		t.Fatal("mutation of event 0's Locale reached event 1")
	}
}

// TestClient_DoNoBody_NonOK2xxEnvelopeIsError is the regression for the
// blind 2xx drain (Codex P2): recovery and replay mutations can answer
// HTTP 200 with a REJECTION envelope such as
// `<response response_code="FORBIDDEN">…</response>`. doNoBody drained
// the body without looking and returned success — recovery handed the
// caller a live handle for a rejected request. A decoded non-OK
// envelope must surface as a typed *Error; empty 202/204 bodies and
// plain OK envelopes stay successful.
func TestClient_DoNoBody_NonOK2xxEnvelopeIsError(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantOK  bool
		wantErr types.ResponseCode
	}{
		{"forbidden envelope on 200", http.StatusOK, `<response response_code="FORBIDDEN"><message>not allowed</message></response>`, false, types.ResponseCode("FORBIDDEN")},
		{"ok envelope on 200", http.StatusOK, `<response response_code="OK"/>`, true, ""},
		{"created envelope on 200", http.StatusOK, `<response response_code="CREATED"/>`, true, ""},
		// ACCEPTED is a recognized success code (Codex P2) — was rejected.
		{"accepted envelope on 200", http.StatusOK, `<response response_code="ACCEPTED"/>`, true, ""},
		{"accepted envelope on 202", http.StatusAccepted, `<response response_code="ACCEPTED"/>`, true, ""},
		{"empty 202", http.StatusAccepted, "", true, ""},
		{"empty 204", http.StatusNoContent, "", true, ""},
		{"non-envelope body on 200", http.StatusOK, `<something_else/>`, true, ""},
		// A bare <error> root on a 2xx is the API's other defined error
		// shape — it must classify as REJECTION, not fall through to
		// success (else a rejected recovery/replay request reports OK).
		{"error root on 200", http.StatusOK, `<error><message>rejected</message></error>`, false, ""},
		{"error root on 202", http.StatusAccepted, `<error><message>nope</message></error>`, false, ""},
		// A <response> root MUST carry a recognized success code — a
		// missing code is not implicit success (Codex P2).
		{"missing code on 200", http.StatusOK, `<response/>`, false, ""},
		{"unknown code on 200", http.StatusOK, `<response response_code="WAT"/>`, false, types.ResponseCode("WAT")},
		// Truncated / malformed <response> must NOT fall through as success.
		{"truncated forbidden on 200", http.StatusOK, `<response response_code="FORBIDDEN"><message>no`, false, ""},
		// Trailing document after a valid envelope → unverified.
		{"trailing document on 200", http.StatusOK, `<response response_code="OK"/><response response_code="OK"/>`, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			c := newTestClient(t, srv)
			ok, err := c.doNoBody(t.Context(), http.MethodPost, "/v1/replay/play", false)
			if tc.wantOK {
				if !ok || err != nil {
					t.Fatalf("doNoBody = (%v, %v), want (true, nil)", ok, err)
				}
				return
			}
			if ok || err == nil {
				t.Fatalf("doNoBody = (%v, %v), want rejection error", ok, err)
			}
			var apiErr *Error
			if !errors.As(err, &apiErr) {
				t.Fatalf("errors.As(*Error) failed for %v", err)
			}
			if apiErr.Code != tc.wantErr {
				t.Fatalf("Code = %q, want %q", apiErr.Code, tc.wantErr)
			}
			if apiErr.Status != tc.status {
				t.Fatalf("Status = %d, want %d", apiErr.Status, tc.status)
			}
		})
	}
}
