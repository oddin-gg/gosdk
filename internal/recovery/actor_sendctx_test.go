package recovery

import (
	"context"
	"errors"
	"testing"
)

// TestActor_SendCtx pins the ctx-bounded admission primitive that
// OnSnapshotCompleteReceived relies on to make snapshot-completes
// reliable: it blocks until the inbox has room, aborts with ctx.Err()
// when the caller ctx cancels, and returns ErrManagerClosed when the
// actor is shutting down.
func TestActor_SendCtx(t *testing.T) {
	a := &recoveryActor{
		inbox:    make(chan actorEvent, 1),
		shutdown: make(chan struct{}),
	}

	// Room available -> admitted.
	if err := a.sendCtx(context.Background(), evTick{}); err != nil {
		t.Fatalf("sendCtx with room = %v, want nil", err)
	}

	// Inbox now full (cap 1). A cancelled ctx must abort instead of
	// blocking forever — the correctness-critical "don't drop, but don't
	// deadlock" behavior.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.sendCtx(ctx, evTick{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("sendCtx on full inbox + cancelled ctx = %v, want context.Canceled", err)
	}

	// Drain one; room frees, admission succeeds again.
	<-a.inbox
	if err := a.sendCtx(context.Background(), evTick{}); err != nil {
		t.Fatalf("sendCtx after drain = %v, want nil", err)
	}

	// Shutdown wins over a full inbox: admission fails with ErrManagerClosed.
	close(a.shutdown)
	if err := a.sendCtx(context.Background(), evTick{}); !errors.Is(err, ErrManagerClosed) {
		t.Fatalf("sendCtx after shutdown = %v, want ErrManagerClosed", err)
	}
}
