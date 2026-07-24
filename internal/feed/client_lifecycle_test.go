package feed

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	log "github.com/oddin-gg/gosdk/internal/log"
)

// newClientForLifecycleTest constructs a *Client with the minimum
// scaffolding needed to exercise close / event / signal helpers
// without dialing a real broker.
func newClientForLifecycleTest() *Client {
	return &Client{
		logger:      log.New(slog.New(slog.NewTextHandler(io.Discard, nil))),
		connectedCh: make(chan struct{}),
		closed:      make(chan struct{}),
	}
}

// TestClient_Close_NeverOpened_Idempotent verifies Close can be called
// repeatedly on a Client that never reached Open. The runShutdown
// fast-paths because conn is nil and wg is zero.
func TestClient_Close_NeverOpened_Idempotent(t *testing.T) {
	c := newClientForLifecycleTest()

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	if err := c.Close(ctx); err != nil {
		t.Fatalf("Close 1: %v", err)
	}
	if err := c.Close(ctx); err != nil {
		t.Fatalf("Close 2: %v", err)
	}

	// closed channel must be closed.
	select {
	case <-c.closed:
		// expected
	case <-time.After(time.Second):
		t.Fatal("c.closed not closed after Close")
	}
}

// TestClient_Close_RespectsCallerCtx verifies the caller's ctx caps
// the wait — but if the shutdown completes in time we observe nil.
func TestClient_Close_RespectsCallerCtx_Completion(t *testing.T) {
	c := newClientForLifecycleTest()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := c.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestClient_SignalConnected_RotatesChannel verifies signalConnected
// closes the current channel AND installs a fresh one — so subsequent
// connection() callers wait on the new channel rather than
// continuously waking on the closed one.
func TestClient_SignalConnected_RotatesChannel(t *testing.T) {
	c := newClientForLifecycleTest()

	pre := c.snapshotConnectedCh()
	c.signalConnected()

	// Old channel is closed.
	select {
	case <-pre:
		// expected
	case <-time.After(100 * time.Millisecond):
		t.Fatal("pre-signal channel was not closed")
	}

	// Fresh channel is open and distinct from the previous one.
	post := c.snapshotConnectedCh()
	if pre == post {
		t.Errorf("signalConnected did not rotate the channel")
	}
	select {
	case <-post:
		t.Fatal("post-signal channel reads ready immediately; should be open")
	default:
		// expected
	}
}

// TestClient_Connection_BeforeAnyDial_BlocksUntilCtxCancel verifies
// connection(ctx) on a never-dialed Client blocks waiting for a
// connection signal, and returns ctx.Err() on cancellation.
func TestClient_Connection_BeforeAnyDial_BlocksUntilCtxCancel(t *testing.T) {
	c := newClientForLifecycleTest()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	conn, err := c.connection(ctx)
	if conn != nil {
		t.Errorf("conn = %v, want nil pre-dial", conn)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
}

// TestClient_Connection_OnAlreadyClosed verifies connection(ctx)
// returns ErrAlreadyClosed when the client's `closed` channel is
// already closed (Close has run).
func TestClient_Connection_OnAlreadyClosed(t *testing.T) {
	c := newClientForLifecycleTest()
	close(c.closed)

	conn, err := c.connection(t.Context())
	if conn != nil {
		t.Errorf("conn = %v, want nil after Close", conn)
	}
	if !errors.Is(err, ErrAlreadyClosed) {
		t.Errorf("err = %v, want ErrAlreadyClosed", err)
	}
}

// TestClient_SetEventEmitter_NilSafe verifies the SetEventEmitter ↔
// emit pair: setting nil makes emit a no-op.
func TestClient_SetEventEmitter_NilSafe(t *testing.T) {
	c := newClientForLifecycleTest()

	c.SetEventEmitter(nil)
	// Should not panic / do anything.
	c.emit(EventConnected, nil)
}

// TestClient_SetEventEmitter_InstallReplaceClear exercises the install /
// replace / nil-replace transitions of SetEventEmitter, asserting the
// active emitter is the one set most recently.
func TestClient_SetEventEmitter_InstallReplaceClear(t *testing.T) {
	c := newClientForLifecycleTest()

	var aFired, bFired atomic.Int32
	a := func(Event) { aFired.Add(1) }
	b := func(Event) { bFired.Add(1) }

	c.SetEventEmitter(a)
	c.emit(EventConnected, nil)
	if aFired.Load() != 1 || bFired.Load() != 0 {
		t.Errorf("after install A: a=%d b=%d", aFired.Load(), bFired.Load())
	}

	c.SetEventEmitter(b)
	c.emit(EventDisconnected, errors.New("x"))
	if aFired.Load() != 1 || bFired.Load() != 1 {
		t.Errorf("after replace with B: a=%d b=%d", aFired.Load(), bFired.Load())
	}

	c.SetEventEmitter(nil)
	c.emit(EventReconnecting, nil)
	if aFired.Load() != 1 || bFired.Load() != 1 {
		t.Errorf("after nil-replace: a=%d b=%d", aFired.Load(), bFired.Load())
	}
}
