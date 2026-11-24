package lru

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestLifetime_EndCancelsContextAndRefusesLaterCommits(t *testing.T) {
	l := NewLifetime(t.Context())
	if l.Err() != nil {
		t.Fatalf("a new lifetime reports %v", l.Err())
	}
	if !l.Commit(func() {}) {
		t.Fatal("a commit before End was refused")
	}
	l.End()
	l.End() // idempotent
	if !errorsIsCanceled(l.Err()) || l.Context().Err() == nil {
		t.Fatalf("End left the context alive: %v", l.Err())
	}
	ran := false
	if l.Commit(func() { ran = true }) || ran {
		t.Fatal("a commit after End ran")
	}
	if l.Enter() {
		t.Fatal("Enter after End succeeded")
	}
}

func errorsIsCanceled(err error) bool { return errors.Is(err, context.Canceled) }

// End returns only after the commit that is running has finished, and holds
// no lock a commit needs while it waits.
func TestLifetime_EndWaitsForARunningCommit(t *testing.T) {
	l := NewLifetime(t.Context())
	inside := make(chan struct{})
	release := make(chan struct{})
	var finished atomic.Bool
	go l.Commit(func() {
		close(inside)
		<-release
		finished.Store(true)
	})
	<-inside

	ended := make(chan struct{})
	go func() { l.End(); close(ended) }()
	select {
	case <-ended:
		t.Fatal("End returned while a commit was running")
	case <-time.After(100 * time.Millisecond):
	}
	// The context is cancelled at once, so a commit that waits on it unwinds.
	if l.Context().Err() == nil {
		t.Fatal("End did not cancel the context before waiting")
	}
	close(release)
	<-ended
	if !finished.Load() {
		t.Fatal("End returned before the commit finished")
	}
}

func TestLifetime_NilNeverEnds(t *testing.T) {
	var l *Lifetime
	if !l.Enter() {
		t.Fatal("nil Enter refused")
	}
	l.Leave()
	l.End()
	if l.Err() != nil || l.Context() != nil || !l.Commit(func() {}) {
		t.Fatal("a nil lifetime must behave as one that never ends")
	}
}
