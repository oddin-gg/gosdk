package recovery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

// The machine's tests run it on a clock the test moves, with the producers of
// a small catalog and everything it does captured: what it asks of the REST
// workers, what it publishes and what it tells of the producers' status.

const (
	preProducer  = 1 // prematch
	liveProducer = 2 // live
	testNodeID   = 7
	// statefulWindow is the stateful recovery window of the test producers.
	statefulWindow = 3 * 24 * 60
)

// sid is the id of the test session n.
func sid(n int) uuid.UUID { return uuid.UUID{15: byte(n)} }

// fakeProducer is a catalog producer with the few fields the machine reads.
type fakeProducer struct {
	types.Producer
	id     int
	name   string
	scopes []types.ProducerScope
	window int
}

func (p fakeProducer) ID() int                               { return p.id }
func (p fakeProducer) Name() string                          { return p.name }
func (p fakeProducer) ProducerScopes() []types.ProducerScope { return p.scopes }
func (p fakeProducer) StatefulRecoveryWindowInMinutes() int  { return p.window }

type fakeCatalog struct {
	list     []types.Producer
	disabled map[int]bool
	from     map[int]time.Time
}

func newFakeCatalog(window int) *fakeCatalog {
	return &fakeCatalog{
		list: []types.Producer{
			fakeProducer{id: preProducer, name: "pre", scopes: []types.ProducerScope{types.PrematchProducerScope}, window: window},
			fakeProducer{id: liveProducer, name: "live", scopes: []types.ProducerScope{types.LiveProducerScope}, window: window},
		},
		disabled: map[int]bool{},
		from:     map[int]time.Time{},
	}
}

func (c *fakeCatalog) producers() []types.Producer { return slices.Clone(c.list) }
func (c *fakeCatalog) producer(id int) (types.Producer, bool) {
	for _, p := range c.list {
		if p.ID() == id {
			return p, true
		}
	}
	return nil, false
}
func (c *fakeCatalog) enabled(id int) bool           { return !c.disabled[id] }
func (c *fakeCatalog) recoveryFrom(id int) time.Time { return c.from[id] }

// fakeOutbox captures what the machine does.
type fakeOutbox struct {
	calls []snapshotRequest
	// statuses is every status change, public or not.
	statuses []statusChange
	// infos and points are every recovery info and resume point published, in order.
	infos  []publishedInfo
	points []publishedPoint
	down   map[int]bool
	// events are the event recovery requests, and outcomes how each ended, in order.
	events []eventRequest
	// ended is the id of every snapshot request that left flight, in order.
	ended    []int
	outcomes []eventOutcome
}

func newFakeOutbox() *fakeOutbox {
	return &fakeOutbox{down: map[int]bool{}}
}

func (o *fakeOutbox) request(call snapshotRequest) { o.calls = append(o.calls, call) }
func (o *fakeOutbox) requestEnded(id int)          { o.ended = append(o.ended, id) }
func (o *fakeOutbox) recoveryInfo(id int, info types.RecoveryInfo) {
	o.infos = append(o.infos, publishedInfo{producerID: id, info: info})
}
func (o *fakeOutbox) resumePoint(id int, point time.Time) {
	o.points = append(o.points, publishedPoint{producerID: id, point: point})
}
func (o *fakeOutbox) eventRequest(call eventRequest)  { o.events = append(o.events, call) }
func (o *fakeOutbox) eventEnded(outcome eventOutcome) { o.outcomes = append(o.outcomes, outcome) }
func (o *fakeOutbox) status(change statusChange) {
	o.statuses = append(o.statuses, change)
	o.down[change.producerID] = change.down
}

type publishedInfo struct {
	producerID int
	info       types.RecoveryInfo
}

type publishedPoint struct {
	producerID int
	point      time.Time
}

// lastInfo is the last recovery info published for the producer.
func (o *fakeOutbox) lastInfo(p int) (types.RecoveryInfo, bool) {
	for i := len(o.infos) - 1; i >= 0; i-- {
		if o.infos[i].producerID == p {
			return o.infos[i].info, true
		}
	}
	return nil, false
}

// lastPoint is the last resume point published for the producer.
func (o *fakeOutbox) lastPoint(p int) (time.Time, bool) {
	for i := len(o.points) - 1; i >= 0; i-- {
		if o.points[i].producerID == p {
			return o.points[i].point, true
		}
	}
	return time.Time{}, false
}

// pointsOf are the resume points published for the producer, in order.
func (o *fakeOutbox) pointsOf(p int) []time.Time {
	var out []time.Time
	for _, pp := range o.points {
		if pp.producerID == p {
			out = append(out, pp.point)
		}
	}
	return out
}

// logSink keeps the machine's log lines as "LEVEL message".
type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (s *logSink) Enabled(context.Context, slog.Level) bool { return true }
func (s *logSink) Handle(_ context.Context, r slog.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines = append(s.lines, r.Level.String()+" "+r.Message)
	return nil
}
func (s *logSink) WithAttrs([]slog.Attr) slog.Handler { return s }
func (s *logSink) WithGroup(string) slog.Handler      { return s }

func (s *logSink) take() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := slices.Clone(s.lines)
	s.lines = nil
	return out
}

// feed is the machine with its clock, catalog and captured outputs.
type feed struct {
	t     *testing.T
	clock *fakeClock
	cat   *fakeCatalog
	out   *fakeOutbox
	logs  *logSink
	// start is the wall clock when the feed was made.
	start time.Time
	// monoCount is how many seconds monoSeconds has moved.
	monoCount int
	m         *machine
	// opened are the sessions open, for the alives in their queues.
	opened []int
	ids    int
}

func defaultSettings() recoverySettings {
	node := testNodeID
	return recoverySettings{
		maxInactivity:           20 * time.Second,
		maxRecoveryTime:         360 * time.Minute,
		snapshotCompleteTimeout: 5 * time.Minute,
		nodeID:                  &node,
	}
}

func newFeed(t *testing.T) *feed {
	t.Helper()
	return newFeedWith(t, defaultSettings(), statefulWindow)
}

func newFeedWith(t *testing.T, settings recoverySettings, window int) *feed {
	t.Helper()
	f := &feed{t: t, clock: newFakeClock(), cat: newFakeCatalog(window), out: newFakeOutbox(), logs: &logSink{}}
	f.start = f.clock.Now().wall
	f.m = newMachine(f.cat, settings, f.out, f.clock, log.New(slog.New(f.logs)), func() int {
		f.ids++
		return f.ids
	})
	return f
}

func (f *feed) now() time.Time { return f.clock.Now().wall }
func (f *feed) stamp() Stamp   { return f.clock.Now() }

// startFeed opens the feed and has the transport report its connection up.
func (f *feed) startFeed() {
	f.m.start()
	f.m.connectionUp()
}

// open opens session n, its reading of the clock taken now.
func (f *feed) open(n int, interest types.MessageInterest) {
	f.openAt(n, interest, f.stamp())
}

// openAt opens session n with the reading of the clock the caller took before
// binding the session's queue.
func (f *feed) openAt(n int, interest types.MessageInterest, at Stamp) {
	f.opened = append(f.opened, n)
	f.m.sessionOpened(sid(n), interest, at)
}

func (f *feed) close(n int) {
	f.opened = slices.DeleteFunc(f.opened, func(o int) bool { return o == n })
	f.m.sessionClosed(sid(n))
}

// alive is a subscribed alive generated now, as the alive channel sees it.
func (f *feed) alive(p int) { f.m.alive(p, f.now(), f.now(), f.stamp(), true) }

func (f *feed) unsubscribed(p int) { f.m.alive(p, f.now(), f.now(), f.stamp(), false) }

// live is a live message generated agedBy ago that session n took now.
func (f *feed) live(n, p int, agedBy time.Duration) {
	at := f.now().Add(-agedBy)
	f.m.processed(sid(n), p, at, at, f.stamp(), 0)
}

// processed is a message generated at at, not stamped ahead of its producer, of
// the recovery requestID (0 for a live message).
func (f *feed) processed(n, p int, at time.Time, requestID int) {
	f.m.processed(sid(n), p, at, at, f.stamp(), requestID)
}

func (f *feed) sessionAlive(n, p int) {
	f.m.sessionAlive(sid(n), p, f.now(), f.now(), f.stamp(), true)
}

func (f *feed) snapshotComplete(n, p, requestID int) { f.m.snapshotComplete(sid(n), p, requestID) }

func (f *feed) resume(p int) (time.Time, bool) {
	f.m.publishResumePoints()
	return f.out.lastPoint(p)
}

// snapshots are the producer's snapshot requests so far.
func (f *feed) snapshots(p int) []snapshotRequest {
	return slices.DeleteFunc(slices.Clone(f.out.calls), func(c snapshotRequest) bool { return c.producerID != p })
}

func (f *feed) lastSnapshot(p int) snapshotRequest {
	f.t.Helper()
	s := f.snapshots(p)
	if len(s) == 0 {
		f.t.Fatalf("no recovery of producer %d was asked for", p)
	}
	return s[len(s)-1]
}

func (f *feed) accept(c snapshotRequest) { f.m.answered(c.requestID, nil) }

func (f *feed) refuse(c snapshotRequest) { f.m.answered(c.requestID, errors.New("503")) }

// complete accepts the recovery and has these sessions see its snapshot_complete.
func (f *feed) complete(c snapshotRequest, sessions ...int) {
	f.accept(c)
	for _, n := range sessions {
		f.snapshotComplete(n, c.producerID, c.requestID)
	}
}

// bothUp brings both producers up by their first recovery, seen by these sessions.
func (f *feed) bothUp(sessions ...int) {
	f.alive(preProducer)
	f.alive(liveProducer)
	f.complete(f.lastSnapshot(preProducer), sessions...)
	f.complete(f.lastSnapshot(liveProducer), sessions...)
}

// upWith is one session of interest, started, with the producers' first
// recoveries complete.
func upWith(t *testing.T, interest types.MessageInterest) *feed {
	t.Helper()
	f := newFeed(t)
	f.open(1, interest)
	f.startFeed()
	f.bothUp(1)
	return f
}

// isDown reports whether the producer is down as the machine last told it:
// every producer starts down.
func (f *feed) isDown(p int) bool {
	down, told := f.out.down[p]
	return !told || down
}

func (f *feed) statusesOf(p int) []statusChange {
	return slices.DeleteFunc(slices.Clone(f.out.statuses), func(c statusChange) bool { return c.producerID != p })
}

// lastStatus is the producer's last status change; it fails the test when there is none.
func (f *feed) lastStatus(p int) statusChange {
	f.t.Helper()
	s := f.statusesOf(p)
	if len(s) == 0 {
		f.t.Fatalf("no status change of producer %d", p)
	}
	return s[len(s)-1]
}

func (f *feed) publicStatuses() []statusChange {
	return slices.DeleteFunc(slices.Clone(f.out.statuses), func(c statusChange) bool { return !c.public })
}

// causes are the causes of a producer's status changes, in order.
func causes(changes []statusChange) []statusCause {
	out := make([]statusCause, len(changes))
	for i, c := range changes {
		out[i] = c.cause
	}
	return out
}

// checkpoint is the session's checkpoint for the producer; not ok when it does
// not receive it.
func (f *feed) checkpoint(n, p int) (time.Time, bool) {
	s := f.m.session(sid(n))
	if s == nil {
		return time.Time{}, false
	}
	l := s.lane(p)
	if l == nil {
		return time.Time{}, false
	}
	return l.checkpoint, true
}

// inFlight is the request id of the producer's recovery in flight, 0 for none.
func (f *feed) inFlight(p int) int {
	if a := f.m.byID[p].active; a != nil {
		return a.requestID
	}
	return 0
}

// recoveryInfo is the last published outcome of the producer's request.
func (f *feed) recoveryInfo(p int) types.RecoveryInfo {
	f.t.Helper()
	info, ok := f.out.lastInfo(p)
	if !ok {
		f.t.Fatalf("no recovery info of producer %d", p)
	}
	return info
}

// ---- assertions

func expect[T comparable](t *testing.T, got, want T, what string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: got %v, want %v", what, got, want)
	}
}

func expectTime(t *testing.T, got, want time.Time, what string) {
	t.Helper()
	if !got.Equal(want) {
		t.Fatalf("%s: got %s, want %s", what, fmtTime(got), fmtTime(want))
	}
}

func fmtTime(at time.Time) string {
	if at.IsZero() {
		return "none (a full snapshot)"
	}
	return at.UTC().Format(time.RFC3339Nano)
}

func expectDown(t *testing.T, f *feed, p int, want bool, what string) {
	t.Helper()
	expect(t, f.isDown(p), want, what+": producer "+fmt.Sprint(p)+" down")
}

func expectCalls(t *testing.T, f *feed, p, want int, what string) {
	t.Helper()
	expect(t, len(f.snapshots(p)), want, what+": recoveries asked for")
}

func expectLine(t *testing.T, lines []string, want string) {
	t.Helper()
	if !slices.Contains(lines, want) {
		t.Fatalf("log lacks %q, has:\n%s", want, strings.Join(lines, "\n"))
	}
}
