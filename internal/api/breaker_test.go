package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/types"
)

const breakerSportsXML = `<?xml version="1.0"?><sports generated_at="2026-01-01T00:00:00"><sport id="od:sport:1" name="Soccer" abbreviation="SOC"/></sports>`

// fakeClock is a settable time source for the breaker.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(1_000_000, 0)} }

func (f *fakeClock) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	f.t = f.t.Add(d)
	f.mu.Unlock()
}

func fired(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestBreaker_OpensForItsTimeOnly(t *testing.T) {
	clk := newFakeClock()
	b := newBreaker(clk.now, 10*time.Second)
	if b.Open() {
		t.Fatal("a new breaker is open")
	}
	b.Trip()
	if !b.Open() {
		t.Fatal("not open after Trip")
	}
	clk.advance(9 * time.Second)
	if !b.Open() {
		t.Fatal("closed before its time ended")
	}
	clk.advance(time.Second)
	if b.Open() {
		t.Fatal("still open after its time ended")
	}
}

func TestBreaker_AFailureWhileOpenExtendsTheTime(t *testing.T) {
	clk := newFakeClock()
	b := newBreaker(clk.now, 10*time.Second)
	b.Trip()
	clk.advance(8 * time.Second)
	b.failure()
	clk.advance(8 * time.Second)
	if !b.Open() {
		t.Fatal("a second failure did not keep the breaker open for another full time")
	}
	clk.advance(2 * time.Second)
	if b.Open() {
		t.Fatal("open beyond the extended time")
	}
}

// Opened fires once per opening: a waiter that took the channel while the
// breaker was closed is woken by the next opening, a repeated failure does
// not panic on a second close, and a waiter taking it after the breaker
// closed again (by a success or by expiry) does not get a stale, fired one.
func TestBreaker_OpenedFiresOncePerOpening(t *testing.T) {
	clk := newFakeClock()
	b := newBreaker(clk.now, 10*time.Second)

	ch := b.Opened()
	if fired(ch) {
		t.Fatal("Opened fired on a closed breaker")
	}
	b.Trip()
	if !fired(ch) {
		t.Fatal("the waiter was not woken by the opening")
	}
	b.Trip() // while open: no second close
	if !fired(b.Opened()) {
		t.Fatal("Opened of an open breaker must be fired")
	}

	b.success()
	ch = b.Opened()
	if fired(ch) {
		t.Fatal("Opened is fired after a success closed the breaker")
	}
	b.Trip()
	if !fired(ch) {
		t.Fatal("second opening did not wake its waiter")
	}

	clk.advance(10 * time.Second)
	if fired(b.Opened()) {
		t.Fatal("Opened is fired after the breaker expired")
	}
	if b.Open() {
		t.Fatal("expired breaker is open")
	}
}

func TestBreaker_SuccessClosesAnOpenBreaker(t *testing.T) {
	b := newBreaker(newFakeClock().now, 10*time.Second)
	b.Trip()
	b.success()
	if b.Open() {
		t.Fatal("open after a healthy answer")
	}
	b.success() // on a closed breaker: nothing happens
	if b.Open() {
		t.Fatal("a success opened the breaker")
	}
}

func TestBreaker_ConcurrentFailuresAndSuccesses(t *testing.T) {
	b := newBreaker(time.Now, 5*time.Millisecond)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 2000 {
				switch (i + j) % 4 {
				case 0:
					b.failure()
				case 1:
					b.success()
				case 2:
					_ = b.Open()
				default:
					ch := b.Opened()
					select {
					case <-ch:
					default:
					}
				}
			}
		}()
	}
	wg.Wait()
	b.success()
	if b.Open() {
		t.Fatal("open after a final success")
	}
	if fired(b.Opened()) {
		t.Fatal("the channel of a closed breaker is fired")
	}
}

func TestBreaker_ClientStartsClosed(t *testing.T) {
	c := New(&testConfig{apiURL: "x", token: "t"})
	if c.Breaker() == nil || c.Breaker().Open() {
		t.Fatal("a new client's breaker must exist and be closed")
	}
	if c.breaker.openFor != breakerOpenFor {
		t.Fatalf("open time = %v, want %v", c.breaker.openFor, breakerOpenFor)
	}
}

// The breaker opens on a failed attempt that says the API is unavailable:
// 5xx, 429, a broken connection and a timeout after the request was sent.
func TestBreaker_OpensOnAPIFailures(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		setup   func(c *Client)
	}{
		{"500", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", http.StatusInternalServerError) }, nil},
		{"503", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", http.StatusServiceUnavailable) }, nil},
		{"429", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "slow down", http.StatusTooManyRequests) }, nil},
		{"connection closed", func(w http.ResponseWriter, r *http.Request) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		}, nil},
		{"timeout after send", func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		}, func(c *Client) { c.httpClient.Timeout = 150 * time.Millisecond }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			c := newTestClient(t, srv)
			c.maxAttempts = 1
			if tc.setup != nil {
				tc.setup(c)
			}
			if _, err := c.FetchSports(t.Context(), types.EnLocale); err == nil {
				t.Fatal("expected an error")
			}
			if !c.Breaker().Open() {
				t.Fatal("the breaker did not open")
			}
		})
	}
}

// A non-idempotent mutation is not retried after a transport error, and the
// broken connection still counts against the API.
func TestBreaker_OpensOnATransportErrorOfANonIdempotentMutation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv)
	if ok, err := c.postEmpty(t.Context(), "/x", false); err == nil || ok {
		t.Fatalf("ok=%v err=%v, want a failure", ok, err)
	}
	if !c.Breaker().Open() {
		t.Fatal("the breaker did not open")
	}
}

// Failures that say nothing about the API leave the breaker closed: a caller
// that gave up, a closed client, an answer about the request (404).
func TestBreaker_DoesNotOpenOnTheCallersOwnOutcome(t *testing.T) {
	t.Run("caller cancels a hanging request", func(t *testing.T) {
		started := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(started)
			<-r.Context().Done()
		}))
		defer srv.Close()
		c := newTestClient(t, srv)
		c.maxAttempts = 1
		ctx, cancel := context.WithCancel(t.Context())
		go func() { <-started; cancel() }()
		if _, err := c.FetchSports(ctx, types.EnLocale); err == nil {
			t.Fatal("expected an error")
		}
		if c.Breaker().Open() {
			t.Fatal("the caller's cancellation opened the breaker")
		}
	})
	t.Run("caller cancels a stalled body", func(t *testing.T) {
		started := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `<?xml version="1.0"?><sports generated_at="2026-01-01T00:00:00"><sport id="od:sp`)
			w.(http.Flusher).Flush()
			close(started)
			<-r.Context().Done()
		}))
		defer srv.Close()
		c := newTestClient(t, srv)
		c.maxAttempts = 1
		c.httpClient.Timeout = 30 * time.Second
		ctx, cancel := context.WithCancel(t.Context())
		go func() { <-started; cancel() }()
		if _, err := c.FetchSports(ctx, types.EnLocale); err == nil {
			t.Fatal("expected an error")
		}
		if c.Breaker().Open() {
			t.Fatal("the caller's cancellation of a body read opened the breaker")
		}
	})
	t.Run("caller cancels a stalled mutation body", func(t *testing.T) {
		started := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `<?xml version="1.0"?><response response_code="O`)
			w.(http.Flusher).Flush()
			close(started)
			<-r.Context().Done()
		}))
		defer srv.Close()
		c := newTestClient(t, srv)
		c.maxAttempts = 1
		c.httpClient.Timeout = 30 * time.Second
		ctx, cancel := context.WithCancel(t.Context())
		go func() { <-started; cancel() }()
		if ok, err := c.postEmpty(ctx, "/x", true); err == nil || ok {
			t.Fatalf("ok=%v err=%v, want a failure", ok, err)
		}
		if c.Breaker().Open() {
			t.Fatal("the caller's cancellation of a mutation body read opened the breaker")
		}
	})
	t.Run("closed client", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, breakerSportsXML)
		}))
		defer srv.Close()
		c := newTestClient(t, srv)
		c.Close()
		if _, err := c.FetchSports(t.Context(), types.EnLocale); !errors.Is(err, ErrClosed) {
			t.Fatalf("err = %v, want ErrClosed", err)
		}
		if c.Breaker().Open() {
			t.Fatal("a call on a closed client opened the breaker")
		}
	})
	t.Run("404", func(t *testing.T) {
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			http.Error(w, "<error/>", http.StatusNotFound)
		}))
		defer srv.Close()
		c := newTestClient(t, srv)
		if _, err := c.FetchSports(t.Context(), types.EnLocale); err == nil {
			t.Fatal("expected an error")
		}
		if hits.Load() != 1 || c.Breaker().Open() {
			t.Fatalf("hits=%d open=%v, want one request and a closed breaker", hits.Load(), c.Breaker().Open())
		}
	})
}

// A 4xx answer is not a healthy one: it does not close an open breaker.
func TestBreaker_ANotFoundAnswerDoesNotCloseIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "<error/>", http.StatusNotFound)
	}))
	defer srv.Close()
	c := newTestClient(t, srv)
	c.Breaker().Trip()
	_, _ = c.FetchSports(t.Context(), types.EnLocale)
	if !c.Breaker().Open() {
		t.Fatal("a 404 closed the breaker")
	}
}

// Any healthy answer closes the breaker, whichever call it was for.
func TestBreaker_ClosedByAHealthyAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, breakerSportsXML)
	}))
	defer srv.Close()
	c := newTestClient(t, srv)
	c.Breaker().Trip()
	if _, err := c.FetchSports(t.Context(), types.EnLocale); err != nil {
		t.Fatal(err)
	}
	if c.Breaker().Open() {
		t.Fatal("a healthy answer did not close the breaker")
	}
}

// A 2xx closes the breaker only once its body has been read in full and
// decoded: headers followed by a truncated, invalid or stalled body are the
// signs of a degraded API and open it instead.
func TestBreaker_ClosedByAHealthyAnswer_OnlyAfterTheBodyWasRead(t *testing.T) {
	const (
		complete  = "complete"
		truncated = "truncated"
		invalid   = "invalid"
		stalled   = "stalled"
		trailing  = "trailing"
	)
	var mode atomic.Value
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		switch mode.Load() {
		case truncated:
			w.Header().Set("Content-Length", "4096")
			_, _ = io.WriteString(w, `<?xml version="1.0"?><sports generated_at="2026-01-01T00:00:00"><sport id="od:sp`)
		case invalid:
			_, _ = io.WriteString(w, `<sports><sport></sports>`)
		case trailing:
			_, _ = io.WriteString(w, breakerSportsXML+breakerSportsXML)
		case stalled:
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `<?xml version="1.0"?><sports generated_at="2026-01-01T00:00:00"><sport id="od:sp`)
			w.(http.Flusher).Flush()
			select {
			case <-release:
			case <-r.Context().Done():
			}
		default:
			_, _ = io.WriteString(w, breakerSportsXML)
		}
	}))
	defer srv.Close()
	defer close(release)
	c := newTestClient(t, srv)
	c.maxAttempts = 1
	c.httpClient.Timeout = 300 * time.Millisecond

	for _, m := range []string{truncated, invalid, trailing, stalled} {
		for _, startOpen := range []bool{false, true} {
			mode.Store(m)
			c.breaker.success()
			if startOpen {
				c.Breaker().Trip()
			}
			if _, err := c.FetchSports(t.Context(), types.EnLocale); err == nil {
				t.Fatalf("%s body: expected an error", m)
			}
			if !c.Breaker().Open() {
				t.Fatalf("%s body (start open=%v): the breaker is closed", m, startOpen)
			}
		}
	}

	mode.Store(complete)
	if _, err := c.FetchSports(t.Context(), types.EnLocale); err != nil {
		t.Fatal(err)
	}
	if c.Breaker().Open() {
		t.Fatal("complete body: the breaker stayed open")
	}
}

// The same holds for a call that does not decode an entity: a POST whose 2xx
// body cannot be read, is too large, cannot be verified or says the service is
// unavailable opens a closed breaker, while a verified body, accepted or a
// decoded rejection about the resource, closes an open one.
func TestBreaker_ClosedByAHealthyAnswer_AMutationOnlyOnceItsBodyWasVerified(t *testing.T) {
	var mode atomic.Value
	mode.Store("ok")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		switch mode.Load() {
		case "truncated":
			w.Header().Set("Content-Length", "4096")
			_, _ = io.WriteString(w, `<?xml version="1.0"?><response response_code="O`)
		case "oversized":
			_, _ = io.WriteString(w, `<?xml version="1.0"?><response response_code="OK"><message>`)
			_, _ = io.WriteString(w, strings.Repeat("a", maxErrorBodyBytes+1))
			_, _ = io.WriteString(w, `</message></response>`)
		case "malformed":
			_, _ = io.WriteString(w, `<payload><a></payload>`)
		case "trailing":
			_, _ = io.WriteString(w, `<response response_code="OK"/><response response_code="OK"/>`)
		case "unavailable":
			_, _ = io.WriteString(w, `<response response_code="SERVICE_UNAVAILABLE"/>`)
		case "forbidden":
			_, _ = io.WriteString(w, `<response response_code="FORBIDDEN"/>`)
		case "not_found":
			_, _ = io.WriteString(w, `<response response_code="NOT_FOUND"/>`)
		case "conflict":
			_, _ = io.WriteString(w, `<response response_code="CONFLICT"/>`)
		case "bare_error":
			_, _ = io.WriteString(w, `<error/>`)
		case "no_code":
			_, _ = io.WriteString(w, `<response/>`)
		case "bare_error_truncated":
			_, _ = io.WriteString(w, `<error><message>x`)
		case "bare_error_unavailable":
			_, _ = io.WriteString(w, `<error response_code="SERVICE_UNAVAILABLE"><message>down</message></error>`)
		case "bare_error_trailing":
			_, _ = io.WriteString(w, `<error/><response response_code="SERVICE_UNAVAILABLE"/>`)
		default:
			_, _ = io.WriteString(w, `<?xml version="1.0"?><response response_code="OK"/>`)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv)
	c.maxAttempts = 1

	for _, m := range []string{"truncated", "oversized", "malformed", "trailing", "unavailable",
		"bare_error_truncated", "bare_error_unavailable", "bare_error_trailing"} {
		mode.Store(m)
		c.breaker.success()
		if ok, err := c.postEmpty(t.Context(), "/x", true); err == nil || ok {
			t.Fatalf("%s body: ok=%v err=%v, want a failure", m, ok, err)
		}
		if !c.Breaker().Open() {
			t.Fatalf("%s body: the breaker stayed closed", m)
		}
	}

	for _, m := range []string{"forbidden", "not_found", "conflict", "bare_error", "no_code"} {
		mode.Store(m)
		c.Breaker().Trip()
		if ok, err := c.postEmpty(t.Context(), "/x", true); err == nil || ok {
			t.Fatalf("%s envelope: ok=%v err=%v, want a rejection", m, ok, err)
		}
		if c.Breaker().Open() {
			t.Fatalf("%s envelope: a healthy answer left the breaker open", m)
		}
	}

	mode.Store("ok")
	c.Breaker().Trip()
	if ok, err := c.postEmpty(t.Context(), "/x", true); err != nil || !ok {
		t.Fatalf("complete body: ok=%v err=%v", ok, err)
	}
	if c.Breaker().Open() {
		t.Fatal("complete body: the breaker stayed open")
	}
}

// Only a healthy answer closes the breaker: a 200 whose envelope says the
// service is unavailable, or whose document is about another resource, is the
// failing API (it opens), while an envelope about the resource itself (not
// found, forbidden) is an answer of a healthy one (it closes).
func TestBreaker_ClosedByAHealthyAnswer_OnlyHealthyAnswers(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		call     func(t *testing.T, c *Client) error
		wantOpen bool
	}{
		{"envelope OK", `<producers response_code="OK"/>`, fetchProducers, false},
		{"envelope service unavailable", `<producers response_code="SERVICE_UNAVAILABLE"/>`, fetchProducers, true},
		{"envelope not found", `<producers response_code="NOT_FOUND"/>`, fetchProducers, false},
		{"envelope forbidden", `<producers response_code="FORBIDDEN"/>`, fetchProducers, false},
		{
			"identity mismatch",
			`<player_profile><player id="od:player:8" name="Q"/></player_profile>`,
			func(t *testing.T, c *Client) error {
				_, err := c.FetchPlayerProfile(t.Context(), "od:player:9", types.EnLocale)
				return err
			},
			true,
		},
		{
			"identity match",
			`<player_profile><player id="od:player:9" name="P"/></player_profile>`,
			func(t *testing.T, c *Client) error {
				_, err := c.FetchPlayerProfile(t.Context(), "od:player:9", types.EnLocale)
				return err
			},
			false,
		},
	}
	for _, tc := range cases {
		for _, startOpen := range []bool{false, true} {
			t.Run(tc.name, func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/xml")
					_, _ = io.WriteString(w, `<?xml version="1.0"?>`+tc.body)
				}))
				defer srv.Close()
				c := newTestClient(t, srv)
				if startOpen {
					c.Breaker().Trip()
				}
				_ = tc.call(t, c)
				if got := c.Breaker().Open(); got != tc.wantOpen {
					t.Fatalf("open = %v, want %v (start open=%v)", got, tc.wantOpen, startOpen)
				}
			})
		}
	}
}

func fetchProducers(t *testing.T, c *Client) error {
	_, err := c.FetchProducers(t.Context())
	return err
}
