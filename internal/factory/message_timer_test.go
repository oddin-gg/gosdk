package factory

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// flipCtx is a context whose timer fires between two reads of its error: the
// first Err says nothing yet, the later ones report the deadline.
type flipCtx struct {
	context.Context
	deadline time.Time
	reads    atomic.Int32
}

func (c *flipCtx) Deadline() (time.Time, bool) { return c.deadline, true }
func (c *flipCtx) Err() error {
	if c.reads.Add(1) == 1 {
		return nil
	}
	return context.DeadlineExceeded
}

// A message whose deadline has passed counted as having waited its whole time
// even when the context's timer fires while the check reads it.
func TestMessageTimerFired_TheTimerFiringDuringTheCheckStillCounts(t *testing.T) {
	ctx := &flipCtx{Context: context.Background(), deadline: time.Now().Add(-time.Millisecond)}
	if !messageTimerFired(ctx) {
		t.Fatal("a passed deadline whose timer fired during the check was not counted")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	late := &laggingCancelCtx{Context: cancelled, deadline: time.Now().Add(-time.Millisecond)}
	if messageTimerFired(late) {
		t.Fatal("a cancelled context counted as its timer having fired")
	}
}

// laggingCancelCtx has a passed deadline but ended by cancellation.
type laggingCancelCtx struct {
	context.Context
	deadline time.Time
}

func (c *laggingCancelCtx) Deadline() (time.Time, bool) { return c.deadline, true }
