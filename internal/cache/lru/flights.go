package lru

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/oddin-gg/gosdk/internal/apipolicy"
)

// ErrFlightTooOld is returned to a message-path caller that stopped waiting
// because the flight it would wait on has already run for as long as a
// message waits for anything. The flight goes on and fills the cache for
// later callers.
var ErrFlightTooOld = errors.New("lru: skipped, the load has been running for longer than a message waits")

// Flights is a group of shared loads keyed by string: concurrent callers
// with the same key share one running flight, and each flight records when
// it started. The start is what lets a message stop waiting on a flight that
// is already older than its timeout (see Flight.Await): with N messages
// needing one hung endpoint, the first waits a timeout and the others wait
// no longer than what is left of it, not a timeout each.
//
// The zero value is ready to use. A flight runs fn in its own goroutine, so
// a caller that stops waiting does not stop it, and the result reaches every
// caller that waits.
type Flights struct {
	mu sync.Mutex
	m  map[string]*Flight
}

// Flight is one running or finished load of a Flights group.
type Flight struct {
	started time.Time
	done    chan struct{}
	val     any
	err     error
}

// Go returns the flight of key, starting it with fn if none is running.
func (g *Flights) Go(key string, fn func() (any, error)) *Flight {
	g.mu.Lock()
	if f, ok := g.m[key]; ok {
		g.mu.Unlock()
		return f
	}
	f := &Flight{started: time.Now(), done: make(chan struct{})}
	if g.m == nil {
		g.m = make(map[string]*Flight)
	}
	g.m[key] = f
	g.mu.Unlock()

	go func() {
		defer func() {
			g.mu.Lock()
			// A Forget may have replaced the flight of this key meanwhile.
			if g.m[key] == f {
				delete(g.m, key)
			}
			g.mu.Unlock()
			close(f.done)
		}()
		f.val, f.err = fn()
	}()
	return f
}

// Do is Go followed by an unbounded wait for the result.
func (g *Flights) Do(key string, fn func() (any, error)) (any, error) {
	f := g.Go(key, fn)
	<-f.done
	return f.val, f.err
}

// Forget detaches the running flight of key: callers arriving afterwards
// start a new flight instead of joining it. The detached flight runs to its
// end and its waiters still get its result.
func (g *Flights) Forget(key string) {
	g.mu.Lock()
	delete(g.m, key)
	g.mu.Unlock()
}

// Done is closed when the flight has ended.
func (f *Flight) Done() <-chan struct{} { return f.done }

// Result is the flight's outcome; it is only valid after Done is closed.
func (f *Flight) Result() (any, error) { return f.val, f.err }

// Started is when the flight began.
func (f *Flight) Started() time.Time { return f.started }

// Await waits for the flight. Every caller stops waiting when ctx is done.
// A caller on the message path (see apipolicy.WithMessage) also stops, with
// apipolicy.ErrSkipped, once the gate opens, and with ErrFlightTooOld once
// the flight has run for the message's timeout, however late the caller
// joined it: a flight that has not answered in that time is not worth a
// further wait from every message that needs the same key.
func (f *Flight) Await(ctx context.Context) (any, error) {
	select {
	case <-f.done:
		return f.val, f.err
	default:
	}
	var tooOld <-chan time.Time
	if d, ok := apipolicy.TimeoutOf(ctx); ok {
		left := d - time.Since(f.started)
		if left <= 0 {
			return nil, ErrFlightTooOld
		}
		t := time.NewTimer(left)
		defer t.Stop()
		tooOld = t.C
	}
	select {
	case <-GateOpened(ctx):
		return nil, apipolicy.ErrSkipped
	case <-f.done:
		return f.val, f.err
	case <-tooOld:
		return nil, ErrFlightTooOld
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
