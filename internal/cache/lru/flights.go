package lru

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oddin-gg/gosdk/internal/apipolicy"
)

// ErrFlightTooOld is returned to a message-path caller that stopped waiting
// because the flight it would wait on has already run for as long as a
// message waits for anything. The flight goes on and fills the cache for
// later callers.
var ErrFlightTooOld = errors.New("lru: skipped, the load has been running for longer than a message waits")

// defaultFailureMemory is how long a group remembers that the load of a key
// failed with an answer of the API (see apipolicy.KeyFailure), so that the
// message path does not start or join a flight for that key again meanwhile.
// It is the time the endpoint breakers stay open.
const defaultFailureMemory = 10 * time.Second

var failureMemory atomic.Int64

// SetFailureMemory changes how long a key's failure is remembered, for tests
// that need a short time, and returns the function that restores the default.
func SetFailureMemory(d time.Duration) (restore func()) {
	failureMemory.Store(int64(d))
	return func() { failureMemory.Store(0) }
}

func failureMemoryFor() time.Duration {
	if d := time.Duration(failureMemory.Load()); d > 0 {
		return d
	}
	return defaultFailureMemory
}

// maxRememberedFailures bounds the failure memory of one group.
const maxRememberedFailures = 4096

// Flights is a group of shared loads keyed by string: concurrent callers
// with the same key share one running flight, and each flight records when
// it started. The start is what lets a message stop waiting on a flight that
// is already older than its timeout (see Flight.Await): with N messages
// needing one hung endpoint, the first waits a timeout and the others wait
// no longer than what is left of it, not a timeout each.
//
// The group also remembers, for 10 s, the keys whose flight ended
// with an answer of the API that was a failure (see apipolicy.KeyFailure):
// the message path asks Remembered before it starts or joins a flight, and a
// success of the key forgets it. Getters never read the memory.
//
// The zero value is ready to use. A flight runs fn in its own goroutine, so
// a caller that stops waiting does not stop it, and the result reaches every
// caller that waits.
type Flights struct {
	mu sync.Mutex
	m  map[string]*Flight
	// failed maps a key to the time its last flight failed.
	failed map[string]time.Time
}

// Flight is one running or finished load of a Flights group.
type Flight struct {
	started time.Time
	done    chan struct{}
	val     any
	err     error

	// attemptFailed is closed when an attempt of the flight's request has
	// failed while the flight goes on (see NoteAttemptFailed).
	attemptFailed chan struct{}
	attemptOnce   sync.Once
}

// NoteAttemptFailed records that an attempt of the flight's request failed
// and that the flight is going to retry it. The messages that wait on the
// flight stop waiting (see Await); the flight goes on. Loaders put it into
// the context of the request with apipolicy.WithAttemptFailed.
func (f *Flight) NoteAttemptFailed() {
	f.attemptOnce.Do(func() { close(f.attemptFailed) })
}

// Go returns the flight of key, starting it with fn if none is running.
func (g *Flights) Go(key string, fn func() (any, error)) *Flight {
	return g.GoWith(key, func(*Flight) (any, error) { return fn() })
}

// GoWith is Go for a load that needs its flight, to pass NoteAttemptFailed
// to the requests it makes.
func (g *Flights) GoWith(key string, fn func(*Flight) (any, error)) *Flight {
	f, _ := g.goWith(false, key, fn)
	return f
}

// GoFor is GoWith for the caller of ctx: a caller on the message path (see
// apipolicy.WithMessage) that would start a flight for a key that failed
// moments ago gets apipolicy.ErrSkipped instead, and no flight starts. The
// check and the start are one step: a key whose flight ended between a
// caller's earlier look at Remembered and this call is still not asked again.
// A caller that joins a running flight, and every caller without the mark,
// gets the flight as GoWith gives it.
func (g *Flights) GoFor(ctx context.Context, key string, fn func(*Flight) (any, error)) (*Flight, error) {
	return g.goWith(apipolicy.GateOf(ctx) != nil, key, fn)
}

func (g *Flights) goWith(skipRemembered bool, key string, fn func(*Flight) (any, error)) (*Flight, error) {
	g.mu.Lock()
	if f, ok := g.m[key]; ok {
		g.mu.Unlock()
		return f, nil
	}
	if skipRemembered && g.rememberedLocked(key) {
		g.mu.Unlock()
		return nil, apipolicy.ErrSkipped
	}
	f := &Flight{started: time.Now(), done: make(chan struct{}), attemptFailed: make(chan struct{})}
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
			g.noteOutcomeLocked(key, f.err)
			g.mu.Unlock()
			close(f.done)
		}()
		f.val, f.err = fn(f)
	}()
	return f, nil
}

// noteOutcomeLocked keeps the failure memory of key: a success forgets it, a
// failure that was an answer of the API for the key remembers it, anything
// else (a cancellation, a timeout, an outage the breaker covers) leaves it as
// it was.
func (g *Flights) noteOutcomeLocked(key string, err error) {
	switch {
	case err == nil:
		delete(g.failed, key)
	case apipolicy.KeyFailure(err):
		now := time.Now()
		if g.failed == nil {
			g.failed = make(map[string]time.Time)
		}
		if _, ok := g.failed[key]; !ok && len(g.failed) >= maxRememberedFailures {
			// Full: drop what has expired, and the oldest if that frees nothing.
			var oldest string
			var oldestAt time.Time
			for k, at := range g.failed {
				if now.Sub(at) >= failureMemoryFor() {
					delete(g.failed, k)
				} else if oldestAt.IsZero() || at.Before(oldestAt) {
					oldest, oldestAt = k, at
				}
			}
			if len(g.failed) >= maxRememberedFailures {
				delete(g.failed, oldest)
			}
		}
		g.failed[key] = now
	}
}

// Remembered reports whether the last flight of key failed with an answer of
// the API moments ago (see SetFailureMemory).
func (g *Flights) Remembered(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.rememberedLocked(key)
}

func (g *Flights) rememberedLocked(key string) bool {
	at, ok := g.failed[key]
	if !ok {
		return false
	}
	if time.Since(at) >= failureMemoryFor() {
		delete(g.failed, key)
		return false
	}
	return true
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

// ForgetPrefix is Forget for every key that starts with prefix, for a caller
// that invalidates one item whose flights are keyed per variant (locale).
func (g *Flights) ForgetPrefix(prefix string) {
	g.mu.Lock()
	for k := range g.m {
		if strings.HasPrefix(k, prefix) {
			delete(g.m, k)
		}
	}
	g.mu.Unlock()
}

// Done is closed when the flight has ended.
func (f *Flight) Done() <-chan struct{} { return f.done }

// Result is the flight's outcome; it is only valid after Done is closed.
func (f *Flight) Result() (any, error) { return f.val, f.err }

// Started is when the flight began.
func (f *Flight) Started() time.Time { return f.started }

// Await waits for the flight, which loads from endpoint ep. Every caller
// stops waiting when ctx is done. A caller on the message path (see
// apipolicy.WithMessage) also stops, with apipolicy.ErrSkipped, once the
// endpoint's gate opens or the first attempt of the flight's request has
// failed (the flight retries for whoever needs the key, a message does not
// sit through that), and with ErrFlightTooOld once the flight has run for the
// message's timeout, however late the caller joined it: a flight that has not
// answered in that time is not worth a further wait from every message that
// needs the same key. When the wait ends because the message's own time ran
// out, ep is recorded as the endpoint the message waited on (see
// apipolicy.NoteTimedOut).
func (f *Flight) Await(ctx context.Context, ep apipolicy.Endpoint) (any, error) {
	val, err := f.await(ctx, ep)
	if errors.Is(err, context.DeadlineExceeded) {
		apipolicy.NoteTimedOut(ctx, ep)
	}
	return val, err
}

func (f *Flight) await(ctx context.Context, ep apipolicy.Endpoint) (any, error) {
	select {
	case <-f.done:
		return f.val, f.err
	default:
	}
	var tooOld <-chan time.Time
	var attemptFailed <-chan struct{}
	if d, ok := apipolicy.TimeoutOf(ctx); ok {
		attemptFailed = f.attemptFailed
		left := d - time.Since(f.started)
		if left <= 0 {
			return nil, tooOldOr(ctx)
		}
		t := time.NewTimer(left)
		defer t.Stop()
		tooOld = t.C
	}
	select {
	case <-GateOpened(ctx, ep):
		return nil, apipolicy.ErrSkipped
	case <-attemptFailed:
		// The flight's own answer, if it has just arrived, is better than
		// none.
		select {
		case <-f.done:
			return f.val, f.err
		default:
		}
		return nil, apipolicy.ErrSkipped
	case <-f.done:
		return f.val, f.err
	case <-tooOld:
		return nil, tooOldOr(ctx)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// tooOldOr is ErrFlightTooOld unless the caller's own time has run out too: a
// message whose own time ran out is not a message that left a slow flight
// early, and the flight's timer may fire together with the context's. The
// context's error is the one that counts, since it tells the factory the
// message waited its whole time. The deadline is read directly because the
// context only reports it once its own timer has run, which can lag behind.
func tooOldOr(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if dl, ok := ctx.Deadline(); ok && !time.Now().Before(dl) {
		return context.DeadlineExceeded
	}
	return ErrFlightTooOld
}
