package lru

import (
	"context"
	"sync"
	"sync/atomic"
)

// Lifetime is the one lifetime owner of the caches of a client. Its context
// is the detach root of every flight; End cancels it and, in the same call,
// waits for the commits that are running. A cache stores only inside Enter
// and Leave (or Commit), so no cache changes after End has returned: a flight
// that finishes later finds the lifetime ended and stores nothing.
//
// A commit is a brief in-memory step that never waits on a flight, so End
// holds no lock while it waits for a flight. The order of locks is always the
// lifetime first, then the lock of the cache; a commit must not Enter again.
//
// The methods of a nil *Lifetime never end: standalone use and tests.
type Lifetime struct {
	ctx    context.Context
	cancel context.CancelFunc

	mu    sync.RWMutex
	ended bool
}

// NewLifetime derives a lifetime from parent: it carries the values of parent
// but not its cancellation; only End cancels it.
func NewLifetime(parent context.Context) *Lifetime {
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	return &Lifetime{ctx: ctx, cancel: cancel}
}

// Context is the detach root of the flights; nil for a nil Lifetime.
func (l *Lifetime) Context() context.Context {
	if l == nil {
		return nil
	}
	return l.ctx
}

// Err is nil until End has been called, then the error of the context.
func (l *Lifetime) Err() error {
	if l == nil {
		return nil
	}
	return l.ctx.Err()
}

// Enter opens a commit and reports false once the lifetime has ended, in
// which case the caller stores nothing and must not call Leave.
func (l *Lifetime) Enter() bool {
	if l == nil {
		return true
	}
	l.mu.RLock()
	if l.ended {
		l.mu.RUnlock()
		return false
	}
	if h := commitHook.Load(); h != nil {
		(*h)()
	}
	return true
}

// commitHook is a test-only seam: see SetCommitHook.
var commitHook atomic.Pointer[func()]

// SetCommitHook installs f to be called by every Enter that succeeds, inside
// the commit it opened, and returns the function that removes it. Tests use it
// to hold a flight inside its commit. It does nothing in production.
func SetCommitHook(f func()) (restore func()) {
	commitHook.Store(&f)
	return func() { commitHook.Store(nil) }
}

// Leave closes a commit that Enter opened.
func (l *Lifetime) Leave() {
	if l != nil {
		l.mu.RUnlock()
	}
}

// Commit runs store as one commit and reports whether it ran.
func (l *Lifetime) Commit(store func()) bool {
	if !l.Enter() {
		return false
	}
	defer l.Leave()
	store()
	return true
}

// End cancels the context of the flights and returns once the commits that
// were running have finished; every later commit is a no-op. It is safe to
// call more than once and from several goroutines.
func (l *Lifetime) End() {
	if l == nil {
		return
	}
	l.cancel()
	l.mu.Lock()
	l.ended = true
	l.mu.Unlock()
}
