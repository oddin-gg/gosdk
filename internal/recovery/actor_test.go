package recovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/producer"
	"github.com/oddin-gg/gosdk/types"
)

// The recovery actor on its own: what its queues keep and drop, in what order
// it takes them, and how it closes. The actor here has the real producer manager
// and machine over the fixture, a clock the test moves, and the fake of the
// manager; unless a test runs it, its loop does not, and the test takes turns by
// hand.

// actorRig is an actor over the fixture that is not running yet.
type actorRig struct {
	t     *testing.T
	a     *recoveryActor
	ops   *fakeManagerOps
	pm    *producer.Manager
	clock *fakeClock
	hits  *recoveryHits
	srv   *httptest.Server
}

func newActorRig(t *testing.T, edit ...func(*minimalCfg)) *actorRig {
	t.Helper()
	return newActorRigAs(t, nil, edit...)
}

// newActorRigAs is newActorRig with the producer manager prepared by setUp before the
// actor is built, as a client prepares it before it connects.
func newActorRigAs(t *testing.T, setUp func(pm *producer.Manager, clock *fakeClock), edit ...func(*minimalCfg)) *actorRig {
	t.Helper()
	srv, hits := fixtureSrv(t)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	cfg := &minimalCfg{apiURL: u.Host, token: "tok"}
	for _, e := range edit {
		e(cfg)
	}
	pm := producer.NewManager(cfg, apiClientFor(srv, cfg), newDiscardLogger())
	if err := pm.Open(t.Context()); err != nil {
		t.Fatal(err)
	}
	active, err := pm.ActiveProducers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	producers := make([]types.Producer, 0, len(active))
	for _, p := range active {
		producers = append(producers, p)
	}
	ops := newFakeManagerOps()
	clock := newFakeClock()
	clock.wall = time.Now().Truncate(time.Millisecond)
	if setUp != nil {
		setUp(pm, clock)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a := newRecoveryActor(ctx, cfg, apiClientFor(srv, cfg), pm, ops, newDiscardLogger(), clock, producers, 0)
	return &actorRig{t: t, a: a, ops: ops, pm: pm, clock: clock, hits: hits, srv: srv}
}

// drainAll takes turns until the queues are empty.
func (r *actorRig) drainAll() {
	r.t.Helper()
	for range 1000 {
		r.a.drain()
		r.a.essentialMu.Lock()
		empty := len(r.a.essential) == 0
		r.a.essentialMu.Unlock()
		if empty && len(r.a.samples) == 0 && len(r.a.commands) == 0 {
			return
		}
	}
	r.t.Fatal("the queues did not empty")
}

// closeBeforeRunning has the close in place before the actor's loop starts, so
// the facts queued are those the close finds.
func (r *actorRig) closeBeforeRunning() {
	r.t.Helper()
	r.a.shutdownOnce.Do(func() { close(r.a.shutdown) })
	go r.a.run()
	if !r.stop() {
		r.t.Fatal("the actor did not stop")
	}
}

// handled records what the actor handles, in order.
type handled struct {
	mu    sync.Mutex
	facts []fact
}

func (h *handled) add(f fact) {
	h.mu.Lock()
	h.facts = append(h.facts, f)
	h.mu.Unlock()
}

func (h *handled) all() []fact {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]fact(nil), h.facts...)
}

func (r *actorRig) record() *handled {
	h := &handled{}
	r.a.setBeforeHandle(h.add)
	return h
}

// run runs the actor's loop, which the test stops with stop.
func (r *actorRig) run() {
	r.t.Helper()
	go r.a.run()
	r.t.Cleanup(func() { r.stop() })
}

func (r *actorRig) stop() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return r.a.stopBounded(ctx)
}

func (r *actorRig) stamp() Stamp { return r.clock.Now() }

// start opens one session of interest and starts the feed, taking the facts by hand.
func (r *actorRig) startWith(interest types.MessageInterest, sessions ...int) {
	r.t.Helper()
	for _, n := range sessions {
		r.a.handle(factOpened{session: sid(n), interest: interest, at: r.stamp()})
		r.a.handle(factChannelReopened{session: sid(n)})
	}
	r.a.handle(factStart{})
}

// postAlive posts an alive of the producer generated now.
func (r *actorRig) postAlive(producerID int, subscribed bool) {
	now := r.stamp()
	r.a.onAlive(producerID, now.Wall(), now, subscribed)
}

func kinds(facts []fact) []string {
	out := make([]string, len(facts))
	for i, f := range facts {
		out[i] = strings.TrimPrefix(fmt.Sprintf("%T", f), "recovery.")
	}
	return out
}

// ---- the queues

// The essential facts are never dropped, however many are posted: the queue has
// no capacity of its own.
func TestActor_EssentialFactsAreNeverDropped(t *testing.T) {
	r := newActorRig(t)
	h := r.record()
	const n = sampleCapacity + commandCapacity + 5000
	for i := range n {
		if !r.a.postEssential(factChannelReopened{session: sid(i % 7)}) {
			t.Fatalf("essential fact %d refused", i)
		}
	}
	r.a.drain()
	if got := len(h.all()); got != n {
		t.Fatalf("handled %d essential facts, posted %d", got, n)
	}
}

// The samples are lossy: a full queue drops one and counts it, and an essential
// fact posted meanwhile is not touched.
func TestActor_SamplesAreLossyAndEssentialFactsAreNot(t *testing.T) {
	r := newActorRig(t)
	h := r.record()
	r.a.handle(factStart{})
	h.mu.Lock()
	h.facts = nil
	h.mu.Unlock()
	for i := 0; i < sampleCapacity+10; i++ {
		r.a.postSample(factProcessed{session: sid(1), producer: liveID, generatedAt: r.clock.Now().Wall(), taken: r.stamp()})
	}
	if got := r.a.counters.samplesDropped.Load(); got != 10 {
		t.Fatalf("samples dropped = %d, want 10", got)
	}
	if !r.a.postEssential(factChannelLost{session: sid(1)}) {
		t.Fatal("an essential fact was refused beside a full sample queue")
	}
	r.drainAll()
	facts := kinds(h.all())
	if len(facts) != sampleCapacity+1 {
		t.Fatalf("handled %d facts, want the essential one and %d samples", len(facts), sampleCapacity)
	}
	if facts[0] != "factChannelLost" {
		t.Fatalf("first handled = %s, want the essential fact before every sample", facts[0])
	}
}

// A sample posted after an essential fact is not handled before it: what a session
// finished on a new channel cannot move a checkpoint before the loss of the old
// channel is known.
func TestActor_SamplePostedAfterAnEssentialFactIsNotHandledBeforeIt(t *testing.T) {
	r := newActorRig(t)
	h := r.record()
	r.a.postSample(factProcessed{session: sid(1), producer: liveID, taken: r.stamp()})
	r.a.postEssential(factChannelLost{session: sid(1)})
	r.a.postSample(factProcessed{session: sid(1), producer: liveID, taken: r.stamp()})
	r.a.drain()
	got := kinds(h.all())
	want := []string{"factChannelLost", "factProcessed", "factProcessed"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("handled %v, want %v", got, want)
	}
}

// The essential queue is looked at after a sample is taken, not only before: a
// poster that put an essential fact in and then a sample, after the actor looked,
// still has the essential fact handled first.
func TestActor_SampleTakenAfterAnEssentialFactWasPostedIsHandledAfterIt(t *testing.T) {
	r := newActorRig(t)
	h := r.record()
	posted := false
	r.a.beforeSample = func() {
		// the actor has looked at the essential queue and found it empty; the
		// fact and the sample arrive now, in that order
		if !posted {
			posted = true
			r.a.postEssential(factChannelLost{session: sid(1)})
			r.a.postSample(factProcessed{session: sid(1), producer: liveID, taken: r.stamp()})
		}
	}
	r.a.drain()
	got := kinds(h.all())
	want := []string{"factChannelLost", "factProcessed"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("handled %v, want %v", got, want)
	}
}

// The commands wait behind what is essential, too, and a full queue of them
// refuses a request, answered at once.
func TestActor_CommandsAreBoundedAndYieldToEssentialFacts(t *testing.T) {
	r := newActorRig(t)
	h := r.record()
	cmds := make([]*recoverEventCommand, 0, commandCapacity)
	for i := range commandCapacity {
		c := &recoverEventCommand{producerID: liveID, eventID: matchURN(i + 1), reply: make(chan recoverEventReply, 1)}
		if err := r.a.postCommand(t.Context(), c); err != nil {
			t.Fatalf("command %d refused: %v", i, err)
		}
		cmds = append(cmds, c)
	}
	extra := &recoverEventCommand{producerID: liveID, eventID: matchURN(99999), reply: make(chan recoverEventReply, 1)}
	if err := r.a.postCommand(t.Context(), extra); !errors.Is(err, errCommandsFull) {
		t.Fatalf("a request beyond the capacity: %v, want errCommandsFull", err)
	}
	if got := r.a.counters.commandsRefused.Load(); got != 1 {
		t.Fatalf("requests refused = %d, want 1", got)
	}
	r.a.postEssential(factChannelLost{session: sid(1)})
	r.drainAll()
	got := kinds(h.all())
	if got[0] != "factChannelLost" {
		t.Fatalf("handled first %s, want the essential fact", got[0])
	}
	for _, c := range cmds {
		select {
		case <-c.reply:
		default:
			t.Fatal("a command was left unanswered after the turns")
		}
	}
}

// A stream of event recovery requests holds up no turn: the actor takes a bounded
// number before it looks at the clock and the rest again.
func TestActor_StreamOfEventRecoveryRequestsHoldsUpNoTurn(t *testing.T) {
	// the feed has not started: each request is refused at once, so no request goes
	// out and no answer comes back to interrupt the turn
	r := newActorRig(t)
	for range commandsPerTurn + 100 {
		c := &recoverEventCommand{producerID: liveID, eventID: matchURN(1), reply: make(chan recoverEventReply, 1)}
		if err := r.a.postCommand(t.Context(), c); err != nil {
			t.Fatal(err)
		}
	}
	r.a.drain()
	if got := len(r.a.commands); got != 100 {
		t.Fatalf("%d requests left after a turn, want 100 of the %d", got, commandsPerTurn+100)
	}
	select {
	case <-r.a.wake:
	default:
		t.Fatal("a turn that left requests behind did not nudge the actor again")
	}
	r.a.drain()
	if got := len(r.a.commands); got != 0 {
		t.Fatalf("%d requests left after the second turn", got)
	}
}

// ---- the alives

// The alives of a producer wait in one slot, however many arrive, and an
// unsubscribed one in it still opens its gap: the machine is given the last
// subscribed alive before it, the unsubscribed one, and the latest.
func TestActor_AlivesInARowTakeOneSlotAndAnUnsubscribedOneInItStillOpensTheGap(t *testing.T) {
	r := newActorRig(t)
	r.startWith(types.AllMessageInterest, 1)
	var queued int
	r.postAlive(liveID, true)
	r.clock.advance(time.Second)
	subscribedBefore := r.clock.Now().Wall()
	r.postAlive(liveID, true)
	r.clock.advance(time.Second)
	r.postAlive(liveID, false)
	for range 100 {
		r.clock.advance(100 * time.Millisecond)
		r.postAlive(liveID, true)
		// an alive of a session is a sample of its own, and takes no slot
		now := r.stamp()
		r.a.postSample(factSessionAlive{session: sid(1), producer: liveID, generatedAt: now.Wall(), taken: now, subscribed: true})
	}
	r.postAlive(preID, true)
	r.a.essentialMu.Lock()
	for _, f := range r.a.essential {
		if _, ok := f.(*factAlives); ok {
			queued++
		}
	}
	r.a.essentialMu.Unlock()
	if queued != 2 {
		t.Fatalf("alive facts queued = %d, want one per producer (2)", queued)
	}
	r.a.drain()
	tr := r.a.machine.byID[liveID]
	if tr.gap == nil {
		t.Fatal("the unsubscribed alive in the slot opened no gap")
	}
	if !tr.gap.from.Equal(subscribedBefore) {
		t.Fatalf("gap from %v, want the last subscribed alive before the unsubscribed one, %v", tr.gap.from, subscribedBefore)
	}
	if r.a.machine.byID[preID].hasAlive != true {
		t.Fatal("the other producer's alive was not taken")
	}
}

// An alive is never dropped for want of room: the sample queue full changes
// nothing for it.
func TestActor_AliveIsNeverDroppedForWantOfRoom(t *testing.T) {
	r := newActorRig(t)
	r.startWith(types.AllMessageInterest, 1)
	for range sampleCapacity + 50 {
		r.a.postSample(factProcessed{session: sid(1), producer: liveID, taken: r.stamp()})
	}
	r.postAlive(liveID, true)
	r.a.drain()
	if !r.a.machine.byID[liveID].hasAlive {
		t.Fatal("an alive was dropped beside a full sample queue")
	}
}

func TestAliveSlot_KeepsWhatTheMachineNeedsInOrder(t *testing.T) {
	at := func(n int, subscribed bool) aliveSeen {
		return aliveSeen{generatedAt: time.Unix(int64(n), 0), subscribed: subscribed}
	}
	cases := []struct {
		name   string
		alives []aliveSeen
		want   []int
	}{
		{"only subscribed: the latest", []aliveSeen{at(1, true), at(2, true), at(3, true)}, []int{3}},
		{"an unsubscribed one first", []aliveSeen{at(1, false)}, []int{1}},
		{"subscribed, then unsubscribed", []aliveSeen{at(1, true), at(2, true), at(3, false)}, []int{2, 3}},
		{"with alives after it", []aliveSeen{at(1, true), at(2, false), at(3, true), at(4, true)}, []int{1, 2, 4}},
		{"two unsubscribed", []aliveSeen{at(1, false), at(2, false)}, []int{1, 2}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var s aliveSlot
			for _, a := range tc.alives {
				s.add(a)
			}
			seen := s.inOrder()
			got := make([]int, 0, len(seen))
			for _, a := range seen {
				got = append(got, int(a.generatedAt.Unix()))
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("in order = %v, want %v", got, tc.want)
			}
		})
	}
}

// ---- stray snapshot_completes

// A flood of snapshot_completes of requests this feed never asked for queues
// nothing, is counted, and is told once at info, never at warn.
func TestActor_FloodOfStraySnapshotCompletesQueuesNothingAndLogsNothingAtWarn(t *testing.T) {
	r := newActorRig(t)
	sink := &lockedBuffer{}
	r.a.logger = debugLogger(sink)
	for i := range 1000 {
		if err := r.a.onSnapshotComplete(sid(1), liveID, 1000+i); err != nil {
			t.Fatal(err)
		}
	}
	if got := r.a.counters.strayCompletions.Load(); got != 1000 {
		t.Fatalf("strays counted = %d, want 1000", got)
	}
	r.a.essentialMu.Lock()
	queued := len(r.a.essential)
	r.a.essentialMu.Unlock()
	if queued != 0 {
		t.Fatalf("%d facts queued for strays", queued)
	}
	out := sink.String()
	if strings.Contains(out, "level=WARN") || strings.Contains(out, "level=ERROR") {
		t.Fatalf("strays logged above info:\n%s", out)
	}
	if got := strings.Count(out, "level=INFO"); got != 1 {
		t.Fatalf("info lines = %d, want 1 told once\n%s", got, out)
	}
}

// A snapshot_complete of a request the actor asked for is queued, once per
// delivery, and one of an id it has forgotten is a stray; the ids in flight are
// remembered up to the cap.
func TestActor_AskedForRequestsAreRememberedUpToTheCap(t *testing.T) {
	r := newActorRig(t)
	r.a.issuedKept = 3
	for id := 1; id <= 5; id++ {
		r.a.remember(id)
	}
	r.a.remember(5) // twice changes nothing
	for id, want := range map[int]bool{1: false, 2: false, 3: true, 4: true, 5: true, 6: false} {
		if got := r.a.wasIssued(id); got != want {
			t.Errorf("wasIssued(%d) = %v, want %v", id, got, want)
		}
	}
	if err := r.a.onSnapshotComplete(sid(1), liveID, 4); err != nil {
		t.Fatal(err)
	}
	r.a.essentialMu.Lock()
	queued := len(r.a.essential)
	r.a.essentialMu.Unlock()
	if queued != 1 {
		t.Fatalf("facts queued for a request asked for = %d, want 1", queued)
	}
}

// ---- the facts carry their stamps and marks

// A session's fact is handled with the stamp it was posted with, not a reading
// of the clock taken when it is handled.
func TestActor_SessionFactIsHandledWithTheStampItWasPostedWith(t *testing.T) {
	r := newActorRig(t)
	r.startWith(types.AllMessageInterest, 1)
	r.postAlive(liveID, true)
	r.a.drain()
	taken := r.stamp()
	r.a.postSample(factProcessed{session: sid(1), producer: liveID, generatedAt: taken.Wall(), taken: taken})
	r.clock.advance(time.Hour)
	r.a.drain()
	l := r.a.machine.session(sid(1)).lane(liveID)
	if l.lastAt != taken.Mono() {
		t.Fatalf("the lane took the message at %v, want the stamp posted with it, %v", l.lastAt, taken.Mono())
	}
	if !l.checkpoint.Equal(taken.Wall()) {
		t.Fatalf("checkpoint %v, want %v", l.checkpoint, taken.Wall())
	}
}

// An alive of the alive channel is handled with the stamp it was received with.
func TestActor_AliveIsHandledWithTheStampItWasReceivedWith(t *testing.T) {
	r := newActorRig(t)
	r.startWith(types.AllMessageInterest, 1)
	received := r.stamp()
	r.a.onAlive(liveID, received.Wall(), received, true)
	r.clock.advance(time.Hour)
	r.a.drain()
	if got := r.a.machine.byID[liveID].lastAliveAt; got != received.Mono() {
		t.Fatalf("alive taken at %v, want the stamp it was received with, %v", got, received.Mono())
	}
}

// A message stamped ahead of its producer's clock moves a checkpoint no further
// than the producer's now; so does an alive of the session.
func TestActor_CheckpointIsTakenFromTheMarkOfAMessageStampedAhead(t *testing.T) {
	r := newActorRig(t)
	r.startWith(types.AllMessageInterest, 1)
	r.postAlive(liveID, true)
	r.a.drain()
	now := r.stamp()
	ahead := now.Wall().Add(10 * time.Minute)
	r.a.postSample(factProcessed{session: sid(1), producer: liveID, generatedAt: ahead, taken: now})
	r.a.postSample(factSessionAlive{session: sid(1), producer: liveID, generatedAt: ahead, taken: now, subscribed: true})
	r.a.drain()
	if got := r.a.machine.session(sid(1)).lane(liveID).checkpoint; !got.Equal(now.Wall()) {
		t.Fatalf("checkpoint %v, want the producer's now %v", got, now.Wall())
	}
}

// An alive from the producer's far future moves the safe point to the SDK's now.
func TestActor_AliveFromTheFutureMovesTheSafePointToTheSDKsNow(t *testing.T) {
	r := newActorRig(t)
	r.startWith(types.AllMessageInterest, 1)
	now := r.stamp()
	r.a.onAlive(liveID, now.Wall().Add(time.Hour), now, true)
	r.a.drain()
	if got := r.a.machine.byID[liveID].safePoint; !got.Equal(now.Wall()) {
		t.Fatalf("safe point %v, want the SDK's now %v", got, now.Wall())
	}
}

// ---- a fact that fails

// A fact that fails, even with a panic, is counted and the actor goes on with the
// next, and the points it moved are published all the same.
func TestActor_FactThatFailsIsCountedAndTheActorGoesOn(t *testing.T) {
	r := newActorRig(t)
	r.startWith(types.AllMessageInterest, 1)
	r.postAlive(liveID, true)
	r.a.drain()
	var failedOnce atomic.Bool
	r.a.setBeforeHandle(func(f fact) {
		if _, ok := f.(factChannelLost); ok && failedOnce.CompareAndSwap(false, true) {
			panic("a fact that cannot be handled")
		}
	})
	r.a.postEssential(factChannelLost{session: sid(1)})
	r.a.postEssential(factChannelReopened{session: sid(1)})
	r.a.drain()
	if got := r.a.counters.factsFailed.Load(); got != 1 {
		t.Fatalf("facts failed = %d, want 1", got)
	}
	if r.a.machine.session(sid(1)).lost {
		t.Fatal("the fact after the failed one was not handled")
	}
}

// The resume point is published as the facts move it: the client's start once the
// feed opens, then what the sessions finish as the first recovery's gap closes.
func TestActor_ResumePointIsPublishedAsTheFactsMoveIt(t *testing.T) {
	var seed time.Time
	r := newActorRigAs(t, func(pm *producer.Manager, clock *fakeClock) {
		seed = clock.Now().Wall().Add(-10 * time.Minute)
		if err := pm.SetProducerRecoveryFromTimestamp(t.Context(), liveID, seed); err != nil {
			t.Fatal(err)
		}
	})
	r.startWith(types.AllMessageInterest, 1)
	if got := r.timestampForRecovery(liveID); !got.Equal(seed) {
		t.Fatalf("resume point after the start = %v, want the client's start %v", got, seed)
	}
	r.postAlive(liveID, true)
	r.clock.advance(time.Minute)
	taken := r.stamp()
	r.postAlive(liveID, true)
	r.a.postSample(factProcessed{session: sid(1), producer: liveID, generatedAt: taken.Wall(), taken: taken})
	r.drainAll()
	// the gap of the first recovery is still open: the point does not move past it
	if got := r.timestampForRecovery(liveID); !got.Equal(seed) {
		t.Fatalf("resume point moved to %v with the first recovery open, want %v", got, seed)
	}
}

func (r *actorRig) timestampForRecovery(producerID int) time.Time {
	r.t.Helper()
	p, err := r.pm.GetProducer(context.Background(), producerID)
	if err != nil {
		r.t.Fatal(err)
	}
	return p.TimestampForRecovery()
}

// ---- the loop

// The actor looks at the clock on its own, whatever else happens: a tick
// reaches the manager (the handles are collected) and the machine is told which
// sessions have a message in processing.
func TestActor_LoopTicksOnItsOwnAndTellsTheMachineWhichSessionsAreStuck(t *testing.T) {
	r := newActorRig(t)
	r.a.tickEvery = 5 * time.Millisecond
	r.startWith(types.AllMessageInterest, 1)
	r.ops.setProcessing(map[uuid.UUID]time.Duration{sid(1): 3 * time.Second})
	r.run()
	eventually(t, "three ticks", func() bool {
		r.ops.mu.Lock()
		defer r.ops.mu.Unlock()
		return r.ops.gcs >= 3
	})
	if !r.stop() {
		t.Fatal("the actor did not stop")
	}
	s := r.a.machine.session(sid(1))
	if !s.processing || s.processingSince != 3*time.Second {
		t.Fatalf("the machine was told processing=%v since %v, want true since 3s", s.processing, s.processingSince)
	}
}

// A tick that finds a producer silent for longer than the maximum inactivity takes
// the producer down through the manager.
func TestActor_TickTakesASilentProducerDownThroughTheProducerManager(t *testing.T) {
	r := newActorRig(t)
	r.startWith(types.AllMessageInterest, 1)
	r.postAlive(liveID, true)
	r.a.drain()
	r.clock.advance(time.Minute)
	r.a.handle(factTick{})
	if down, _ := r.pm.IsProducerDown(context.Background(), liveID); !down {
		t.Fatal("the producer manager does not flag a silent producer down")
	}
	var got []types.ProducerStatus
	for _, m := range r.ops.emitted() {
		if m.ProducerStatus != nil && m.ProducerStatus.Producer().ID() == liveID {
			got = append(got, m.ProducerStatus)
		}
	}
	if len(got) != 1 || !got[0].IsDown() || got[0].ProducerStatusReason() != types.AliveIntervalViolationProducerStatusReason {
		t.Fatalf("statuses = %v, want one: down for the alive interval", got)
	}
	if cur := r.a.currentStatus(liveID); cur == nil || !cur.IsDown() {
		t.Fatalf("current status = %v, want the one told", cur)
	}
}

// ---- the end

func TestActor_StopBoundedReturnsOnItsDeadlineForAWedgedActorAndJoinsALiveOne(t *testing.T) {
	wedged := newActorRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if wedged.a.stopBounded(ctx) {
		t.Fatal("stopBounded reported completion on a never-running actor")
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("stopBounded blocked %v despite the ctx bound", el)
	}

	live := newActorRig(t)
	live.run()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if !live.a.stopBounded(ctx2) {
		t.Fatal("stopBounded reported a timeout on a live actor")
	}
	if !live.a.stopBounded(ctx2) { // idempotent
		t.Fatal("a second stopBounded failed")
	}
}

// What is essential and queued when the close comes is handled before the actor
// ends: its delivery was acked on the strength of the admission.
func TestActor_EssentialFactsQueuedWhenTheCloseComesAreHandledBeforeTheActorEnds(t *testing.T) {
	for range 10 {
		r := newActorRig(t)
		h := r.record()
		r.startWith(types.AllMessageInterest, 1)
		r.a.postEssential(factChannelLost{session: sid(1)})
		r.a.postEssential(factChannelReopened{session: sid(1)})
		go r.a.run()
		if !r.stop() {
			t.Fatal("the actor did not stop")
		}
		got := kinds(h.all())
		n := len(got)
		if n < 2 || got[n-2] != "factChannelLost" || got[n-1] != "factChannelReopened" {
			t.Fatalf("handled %v, want the queued facts handled before the end", got)
		}
	}
}

// Nothing posted after the close is taken: posting is refused, so a snapshot_complete
// stays unacked for the broker to redeliver, and a request is answered as closed.
func TestActor_FactsPostedAfterTheCloseAreRefused(t *testing.T) {
	r := newActorRig(t)
	r.run()
	r.stop()
	if r.a.postEssential(factChannelLost{session: sid(1)}) {
		t.Fatal("an essential fact was taken after the close")
	}
	if err := r.a.onSnapshotComplete(sid(1), liveID, 1); !errors.Is(err, ErrManagerClosed) {
		t.Fatalf("snapshot_complete after the close = %v, want ErrManagerClosed", err)
	}
	c := &recoverEventCommand{producerID: liveID, eventID: matchURN(1), reply: make(chan recoverEventReply, 1)}
	if err := r.a.postCommand(t.Context(), c); !errors.Is(err, ErrManagerClosed) {
		t.Fatalf("a request after the close = %v, want ErrManagerClosed", err)
	}
	r.a.postSample(factProcessed{session: sid(1), producer: liveID})
	if len(r.a.samples) != 0 {
		t.Fatal("a sample was queued after the close")
	}
}

// With no time left to handle what is queued, the resume points go back to each
// producer's last subscribed alive.
func TestActor_FactsLeftUnhandledWhenTheTimeToFinishRunsOutTakeThePointsBack(t *testing.T) {
	r := newActorRig(t)
	r.startWith(types.AllMessageInterest, 1)
	r.postAlive(liveID, true)
	r.a.drain()
	r.clock.advance(10 * time.Second)
	lastSubscribed := r.stamp().Wall()
	r.postAlive(liveID, true)
	taken := r.stamp()
	r.a.postSample(factProcessed{session: sid(1), producer: liveID, generatedAt: taken.Wall(), taken: taken})
	r.a.drain()
	before := r.timestampForRecovery(liveID)

	r.a.finishWait = 0
	r.a.postEssential(factChannelLost{session: sid(1)}) // left unhandled: its time is up
	r.closeBeforeRunning()
	if got := r.a.counters.unhandledAtClose.Load(); got != 1 {
		t.Fatalf("unhandled-at-close = %d, want 1", got)
	}
	got := r.timestampForRecovery(liveID)
	if got.After(before) || !got.Equal(lastSubscribed) && !got.Before(lastSubscribed) {
		t.Fatalf("resume point after the close = %v, want back at or before the last subscribed alive %v (was %v)", got, lastSubscribed, before)
	}
}

// A fact queued when the close comes that opens a gap (an unsubscribed alive)
// takes the point back before the actor ends.
func TestActor_GapQueuedWhenTheCloseComesTakesThePointBack(t *testing.T) {
	r := newActorRig(t)
	r.startWith(types.AllMessageInterest, 1)
	r.postAlive(liveID, true)
	r.a.drain()
	r.clock.advance(10 * time.Second)
	subscribed := r.stamp().Wall()
	r.postAlive(liveID, true)
	r.a.drain()
	r.clock.advance(10 * time.Second)
	r.postAlive(liveID, false) // queued when the close comes
	go r.a.run()
	if !r.stop() {
		t.Fatal("the actor did not stop")
	}
	if got := r.timestampForRecovery(liveID); got.After(subscribed) {
		t.Fatalf("resume point %v, want back at the last subscribed alive %v", got, subscribed)
	}
}

// A session's close queued when the actor closes moves no point forward: the sessions close
// one by one at shutdown, and the point the client persists must not depend on
// the order. The session that is behind leaves, and the point stays where it was.
func TestActor_SessionsCloseQueuedWhenTheActorClosesMovesThePointNoFurther(t *testing.T) {
	r := newActorRig(t)
	r.startWith(types.AllMessageInterest, 1, 2)
	r.postAlive(liveID, true)
	r.a.drain()
	first := r.a.machine.byID[liveID].active.requestID
	r.a.handle(factSnapshotComplete{session: sid(1), producer: liveID, requestID: first})
	r.a.handle(factSnapshotComplete{session: sid(2), producer: liveID, requestID: first})
	if r.a.machine.byID[liveID].active != nil {
		t.Fatal("the first recovery did not complete")
	}
	r.clock.advance(10 * time.Second)
	r.postAlive(liveID, true)
	behind := r.stamp()
	r.a.postSample(factProcessed{session: sid(1), producer: liveID, generatedAt: behind.Wall(), taken: behind})
	r.clock.advance(10 * time.Second)
	r.postAlive(liveID, true)
	ahead := r.stamp()
	r.a.postSample(factProcessed{session: sid(2), producer: liveID, generatedAt: ahead.Wall(), taken: ahead})
	r.drainAll()
	if got := r.timestampForRecovery(liveID); !got.Equal(behind.Wall()) {
		t.Fatalf("resume point before the close = %v, want the session that is behind, %v", got, behind.Wall())
	}
	r.a.postEssential(factClosed{session: sid(1)})
	r.a.postEssential(factClosed{session: sid(2)})
	r.closeBeforeRunning()
	if got := r.timestampForRecovery(liveID); !got.Equal(behind.Wall()) {
		t.Fatalf("closing the sessions at the close moved the point to %v, want it where it was, %v", got, behind.Wall())
	}
}

// A request waiting in the queue when the actor closes is answered as closed, and
// an event recovery in flight fails as closed: its handle classifies under the
// closed-client sentinel.
func TestActor_CloseAnswersWaitingRequestsAndFailsEventRecoveriesInFlight(t *testing.T) {
	r := newActorRig(t)
	r.startWith(types.AllMessageInterest, 1)
	r.postAlive(liveID, true)
	r.a.drain()
	inFlight := &recoverEventCommand{producerID: liveID, eventID: matchURN(1), reply: make(chan recoverEventReply, 1)}
	if err := r.a.postCommand(t.Context(), inFlight); err != nil {
		t.Fatal(err)
	}
	r.a.drain()
	rep := <-inFlight.reply
	if rep.err != nil || rep.handle == nil {
		t.Fatalf("reply = %+v", rep)
	}

	waiting := &recoverEventCommand{producerID: liveID, eventID: matchURN(2), reply: make(chan recoverEventReply, 1)}
	if err := r.a.postCommand(t.Context(), waiting); err != nil {
		t.Fatal(err)
	}
	r.closeBeforeRunning()
	w := <-waiting.reply
	if !errors.Is(w.err, ErrManagerClosed) || w.handle != nil {
		t.Fatalf("a waiting request = %+v, want ErrManagerClosed", w)
	}
	select {
	case <-rep.handle.Done():
	default:
		t.Fatal("the event recovery in flight did not end with the actor")
	}
	if res := rep.handle.Result(); res.Status != types.RecoveryStatusFailed || !errors.Is(res.Err, ErrManagerClosed) {
		t.Fatalf("result = %v, %v, want failed as closed", res.Status, res.Err)
	}
}

// An event recovery deferred for a channel does not make the close wait.
func TestActor_DeferredEventRecoveryDoesNotMakeTheCloseWait(t *testing.T) {
	r := newActorRig(t)
	r.startWith(types.AllMessageInterest, 1)
	r.a.handle(factOpened{session: sid(2), interest: types.LiveOnlyMessageInterest, at: r.stamp()}) // queue not bound
	c := &recoverEventCommand{producerID: liveID, eventID: matchURN(1), reply: make(chan recoverEventReply, 1)}
	if err := r.a.postCommand(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	r.a.drain()
	rep := <-c.reply
	if rep.err != nil {
		t.Fatal(rep.err)
	}
	go r.a.run()
	start := time.Now()
	if !r.stop() {
		t.Fatal("the actor did not stop")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("closing took %v with a deferred event recovery", time.Since(start))
	}
	if res := rep.handle.Result(); res.Status != types.RecoveryStatusFailed || !errors.Is(res.Err, ErrManagerClosed) {
		t.Fatalf("deferred event recovery ended %v, %v, want failed as closed", res.Status, res.Err)
	}
	if got := r.hits.eventRecover.Load(); got != 0 {
		t.Fatalf("a deferred request went out: %d", got)
	}
}

// ---- the requests

// An event recovery request is rooted at the actor's lifetime, not at the caller's
// context: callers cancel their context the moment they hold the handle.
func TestManager_EventRecoveryRequestIsNotAbortedByTheCallersCancel(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseIt := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseIt()
	var requestCtxErr atomic.Pointer[error]

	r := newRigWith(t, func(w http.ResponseWriter, req *http.Request) bool {
		if strings.Contains(req.URL.Path, "/odds/events/") {
			close(entered)
			<-release
			e := req.Context().Err()
			requestCtxErr.Store(&e)
			_, _ = io.WriteString(w, okBody)
			return true
		}
		return false
	})
	r.open()
	s1 := uuid.New()
	r.openSession(s1, types.AllMessageInterest)

	callerCtx, cancelCaller := context.WithCancel(context.Background())
	h, err := r.mgr.InitiateEventOddsRecoveryHandle(callerCtx, liveID, matchURN(1))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the request never reached the server")
	}
	cancelCaller()
	releaseIt()
	eventually(t, "the request to finish", func() bool { return requestCtxErr.Load() != nil })
	if err := *requestCtxErr.Load(); err != nil {
		t.Fatalf("the caller's cancel aborted the request: %v", err)
	}
	if h.IsTerminal() {
		t.Fatalf("the recovery ended on the caller's cancel: %v", h.Snapshot())
	}
}

// Closing the manager cancels a request in flight at once, and says nothing at
// warn or above: an ordinary close during a recovery is nothing to act on.
func TestManager_CloseAbortsARequestInFlightAndLogsNothingAboveInfo(t *testing.T) {
	entered := make(chan struct{}, 1)
	aborted := make(chan struct{}, 1)
	r := newRigWith(t, func(w http.ResponseWriter, req *http.Request) bool {
		if strings.Contains(req.URL.Path, "/recovery/initiate_request") {
			entered <- struct{}{}
			<-req.Context().Done()
			aborted <- struct{}{}
			return true
		}
		return false
	})
	sink := &lockedBuffer{}
	r.mgr.logger = debugLogger(sink)
	r.open()
	s1 := uuid.New()
	r.openSession(s1, types.AllMessageInterest)
	r.alive(liveID)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the snapshot request never reached the server")
	}
	start := time.Now()
	if !r.mgr.CloseCtx(t.Context()) {
		t.Fatal("close did not complete")
	}
	select {
	case <-aborted:
	case <-time.After(2 * time.Second):
		t.Fatal("close did not cancel the request in flight")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("close took %v", time.Since(start))
	}
	for line := range strings.SplitSeq(sink.String(), "\n") {
		if strings.Contains(line, "level=WARN") || strings.Contains(line, "level=ERROR") {
			t.Fatalf("close logged above info: %s", line)
		}
	}
}

// A request that a new one for the same producer supersedes is cancelled, not
// retried beside it.
func TestManager_SnapshotRequestSupersededByTheNextIsCancelled(t *testing.T) {
	var calls atomic.Int32
	firstAborted := make(chan struct{}, 1)
	r := newRigWith(t, func(w http.ResponseWriter, req *http.Request) bool {
		if strings.Contains(req.URL.Path, "/recovery/initiate_request") {
			if calls.Add(1) == 1 {
				<-req.Context().Done()
				firstAborted <- struct{}{}
				return true
			}
		}
		return false
	})
	r.open()
	s1 := uuid.New()
	r.openSession(s1, types.AllMessageInterest)
	r.alive(liveID)
	eventually(t, "the first request to be in flight", func() bool { return calls.Load() == 1 })
	r.sync()
	r.mgr.OnFeedChannelLost(s1)
	r.mgr.OnFeedChannelRestored(s1)
	r.alive(liveID)
	select {
	case <-firstAborted:
	case <-time.After(3 * time.Second):
		t.Fatal("the superseded request was not cancelled")
	}
	eventually(t, "the second request", func() bool { return calls.Load() >= 2 })
}

// ---- what the client's settings become

func TestSettingsOf_DeadlineForASnapshotCompleteIsNeverAboveTheMaximumRecoveryTime(t *testing.T) {
	cfg := &minimalCfg{maxRecovery: time.Minute, snapshotDeadline: time.Hour}
	if got := settingsOf(cfg, 0).snapshotCompleteTimeout; got != time.Minute {
		t.Fatalf("snapshot-complete timeout = %v, want the maximum recovery time, 1m", got)
	}
	cfg = &minimalCfg{maxRecovery: time.Hour, snapshotDeadline: time.Minute}
	s := settingsOf(cfg, 90*time.Minute)
	if s.snapshotCompleteTimeout != time.Minute || s.maxRecoveryTime != time.Hour || s.initialSnapshot != 90*time.Minute || s.maxInactivity != 20*time.Second {
		t.Fatalf("settings = %+v", s)
	}
	node := 4
	cfg = &minimalCfg{nodeID: &node}
	if got := settingsOf(cfg, 0).nodeID; got == nil || *got != 4 {
		t.Fatalf("node id = %v, want 4", got)
	}
}

// WithInitialSnapshotTime narrows a first recovery with no recovery start to a
// window back from now; none asks for the full history.
func TestManager_InitialSnapshotTimeNarrowsTheFirstRecovery(t *testing.T) {
	r := newRig(t)
	r.mgr.initialSnapshotTime = time.Hour
	r.open()
	s1 := uuid.New()
	r.openSession(s1, types.AllMessageInterest)
	before := r.clock.Now().Wall()
	r.alive(liveID)
	got := r.waitSnapshots(1)[0]
	want := before.Add(-time.Hour).UnixMilli()
	if d := got.afterMillis - want; d < -2000 || d > 2000 {
		t.Fatalf("first recovery after %d, want about now less an hour (%d)", got.afterMillis, want)
	}
}

// A request for an event recovery after the manager is closed does not hang, and
// one for a producer the catalog does not have costs the actor nothing.
func TestManager_EventRecoveryOfAnInvalidProducerCostsNoCommand(t *testing.T) {
	r := newRig(t)
	r.open()
	for id := 1000; id < 1020; id++ {
		if _, err := r.mgr.InitiateEventOddsRecoveryHandle(t.Context(), id, matchURN(1)); !errors.Is(err, producer.ErrProducerNotFound) {
			t.Fatalf("producer %d: err = %v, want producer.ErrProducerNotFound", id, err)
		}
	}
	if got := len(r.mgr.actor().commands); got != 0 {
		t.Fatalf("%d commands queued for producers the catalog does not have", got)
	}
	if got := r.mgr.actor().counters.factsHandledTotal.Load(); got > 3 {
		t.Fatalf("the actor handled %d facts for producers the catalog does not have", got)
	}
	if got := r.hits.eventRecover.Load(); got != 0 {
		t.Fatalf("%d requests sent for producers the catalog does not have", got)
	}
}

func TestManager_EventRecoveryCallRacingTheCloseDoesNotHang(t *testing.T) {
	for range 20 {
		r := newRig(t)
		r.open()
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = r.mgr.InitiateEventOddsRecoveryHandle(context.Background(), liveID, matchURN(1))
		}()
		r.mgr.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("a request racing the close never returned")
		}
	}
}

// The flag that a session processes the producer late is told on its own while
// the producer's state is otherwise unchanged, and ProducerStatus reports it.
func TestManager_DelayedFlagIsToldAndClearsOnTheTicks(t *testing.T) {
	r := newRig(t)
	r.open()
	s1 := uuid.New()
	r.openSession(s1, types.AllMessageInterest)
	r.alive(liveID)
	first := r.waitSnapshots(1)[0]
	r.snapshotComplete(s1, liveID, first.requestID)
	eventually(t, "the producer up", func() bool { s, ok := r.status(liveID); return ok && !s.IsDown() })
	if s, _ := r.status(liveID); s.IsDelayed() {
		t.Fatal("a producer that just came up is delayed")
	}

	// the session takes a message 25 s after the producer generated it
	r.clock.advance(5 * time.Second)
	r.alive(liveID)
	now := r.clock.Now()
	r.mgr.OnMessageProcessed(s1, liveID, now.Wall().Add(-25*time.Second), now, 0)
	r.sync()
	r.tick()
	eventually(t, "the delayed flag", func() bool { s, ok := r.status(liveID); return ok && s.IsDelayed() })
	if s, _ := r.status(liveID); !s.IsDown() || s.ProducerStatusReason() != types.ProcessingQueueDelayViolationProducerStatusReason {
		t.Fatalf("status while late = down %v reason %v", s.IsDown(), s.ProducerStatusReason())
	}

	r.clock.advance(time.Second)
	r.alive(liveID)
	now = r.clock.Now()
	r.mgr.OnMessageProcessed(s1, liveID, now.Wall(), now, 0)
	r.sync()
	r.tick()
	eventually(t, "the flag clearing", func() bool { s, ok := r.status(liveID); return ok && !s.IsDelayed() && !s.IsDown() })
}

// newRigWith is newRig over a fixture that lets a test answer some requests itself:
// the handler returns true for the ones it served.
func newRigWith(t *testing.T, handle func(http.ResponseWriter, *http.Request) bool) *rig {
	t.Helper()
	r := newRig(t)
	r.hits.override.Store(&handle)
	return r
}

// ---- an alive is never handled before an essential fact posted before it

// blockRecoveryPosts makes the fixture hold every recovery request until its context ends.
func blockRecoveryPosts(r *actorRig) {
	h := func(w http.ResponseWriter, req *http.Request) bool {
		if strings.Contains(req.URL.Path, "/recovery/initiate_request") {
			<-req.Context().Done()
			return true
		}
		return false
	}
	r.hits.override.Store(&h)
}

// An unsubscribed alive received after the API accepted the request gives the recovery
// up (the producer took it and then said it is not subscribed: it lost it), whether
// the actor handles the facts as they are posted or has them wait in its queue: an
// alive does not join a slot queued before the acceptance.
func TestActor_UnsubscribedAliveAfterTheAcceptanceIsHandledAfterItWhateverTheActorIsDoing(t *testing.T) {
	run := func(stalled bool) (abandoned int, inFlight bool) {
		r := newActorRig(t)
		blockRecoveryPosts(r)
		r.startWith(types.AllMessageInterest, 1)
		r.postAlive(liveID, true)
		r.a.drain()
		id := r.a.machine.byID[liveID].active.requestID
		r.clock.advance(time.Second)
		r.postAlive(liveID, true)
		if !stalled {
			r.a.drain()
		}
		r.clock.advance(time.Second)
		r.a.postEssential(factAnswered{requestID: id}) // the API accepted
		if !stalled {
			r.a.drain()
		}
		r.clock.advance(time.Second)
		r.postAlive(liveID, false) // received after the acceptance
		r.a.drain()
		return r.a.machine.counters.abandoned, r.a.machine.byID[liveID].active != nil
	}
	ab, act := run(false)
	ab2, act2 := run(true)
	if ab != 1 {
		t.Fatalf("facts handled as they come: abandoned = %d, want the recovery given up", ab)
	}
	if ab != ab2 || act != act2 {
		t.Fatalf("the same facts give another outcome when they wait: abandoned %d/%d, in flight %v/%v", ab, ab2, act, act2)
	}
}

// Alives that arrive one after the other share a fact; an essential fact between
// them ends the sharing.
func TestActor_AlivesShareAFactOnlyWhileNothingEssentialIsPostedBetweenThem(t *testing.T) {
	r := newActorRig(t)
	count := func() int {
		r.a.essentialMu.Lock()
		defer r.a.essentialMu.Unlock()
		n := 0
		for _, f := range r.a.essential {
			if _, ok := f.(*factAlives); ok {
				n++
			}
		}
		return n
	}
	r.postAlive(liveID, true)
	r.postAlive(liveID, true)
	r.postAlive(liveID, false)
	if got := count(); got != 1 {
		t.Fatalf("alives in a row: %d facts, want 1", got)
	}
	r.a.postEssential(factChannelLost{session: sid(1)})
	r.postAlive(liveID, true)
	r.postAlive(liveID, true)
	if got := count(); got != 2 {
		t.Fatalf("alives after an essential fact: %d facts, want 2", got)
	}
	r.postAlive(preID, true)
	r.postAlive(liveID, true)
	if got := count(); got != 4 {
		t.Fatalf("an alive of another producer in between: %d facts, want 4", got)
	}
}

// ---- a close with a request in flight

// closeWhileTheActorIsBusy closes the manager with the actor held inside a fact, after
// the requests in flight have been aborted by the close and their goroutines have
// finished: an answer they posted is in the actor's queue before the actor sees
// the close.
func closeWhileTheActorIsBusy(t *testing.T, r *rig, aborted <-chan struct{}) {
	t.Helper()
	a := r.mgr.actor()
	busy := make(chan struct{})
	release := make(chan struct{})
	a.setBeforeHandle(func(f fact) {
		if _, ok := f.(factBarrier); ok {
			close(busy)
			<-release
		}
	})
	a.postEssential(factBarrier{})
	<-busy
	closed := make(chan struct{})
	go func() {
		r.mgr.CloseCtx(t.Context())
		close(closed)
	}()
	select {
	case <-aborted:
	case <-time.After(5 * time.Second):
		t.Fatal("close did not cancel the request in flight")
	}
	a.detached.Wait() // the goroutines of the requests have posted what they were going to
	close(release)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("close did not return")
	}
}

func noLogAboveInfo(t *testing.T, sink *lockedBuffer) {
	t.Helper()
	for line := range strings.SplitSeq(sink.String(), "\n") {
		if strings.Contains(line, "level=WARN") || strings.Contains(line, "level=ERROR") {
			t.Fatalf("a clean close logged above info: %s", line)
		}
	}
}

// An event recovery whose request the close aborted ends as closed, not as a refusal
// by the API, however busy the actor is when the aborted request answers.
func TestManager_CloseWhileTheActorIsBusyEndsAnEventRecoveryAsClosed(t *testing.T) {
	entered := make(chan struct{}, 1)
	aborted := make(chan struct{})
	r := newRigWith(t, func(w http.ResponseWriter, req *http.Request) bool {
		if strings.Contains(req.URL.Path, "/odds/events/") {
			entered <- struct{}{}
			<-req.Context().Done()
			close(aborted)
			return true
		}
		return false
	})
	sink := &lockedBuffer{}
	r.mgr.logger = debugLogger(sink)
	r.open()
	s1 := uuid.New()
	r.openSession(s1, types.AllMessageInterest)
	h, err := r.mgr.InitiateEventOddsRecoveryHandle(t.Context(), liveID, matchURN(1))
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	closeWhileTheActorIsBusy(t, r, aborted)
	res := h.Result()
	if res.Status != types.RecoveryStatusFailed || !errors.Is(res.Err, ErrManagerClosed) {
		t.Fatalf("result = %v, %v, want failed as closed", res.Status, res.Err)
	}
	noLogAboveInfo(t, sink)
}

// A snapshot recovery whose request the close aborted is no failure: nothing above info
// is logged, no backoff is announced and the recovery info is not touched.
func TestManager_CloseWhileTheActorIsBusyLogsNothingAboveInfoForASnapshotRequest(t *testing.T) {
	entered := make(chan struct{}, 1)
	aborted := make(chan struct{})
	r := newRigWith(t, func(w http.ResponseWriter, req *http.Request) bool {
		if strings.Contains(req.URL.Path, "/recovery/initiate_request") {
			entered <- struct{}{}
			<-req.Context().Done()
			close(aborted)
			return true
		}
		return false
	})
	sink := &lockedBuffer{}
	r.mgr.logger = debugLogger(sink)
	r.open()
	s1 := uuid.New()
	r.openSession(s1, types.AllMessageInterest)
	r.alive(liveID)
	<-entered
	closeWhileTheActorIsBusy(t, r, aborted)
	noLogAboveInfo(t, sink)
	p, _ := r.pm.GetProducer(t.Context(), liveID)
	if info := p.RecoveryInfo(); info != nil {
		t.Fatalf("recovery info = request %d successful %v after a clean close, want none", (*info).RequestID(), (*info).Successful())
	}
}

// ---- the end, in detail

// A start the client set and the actor did not get to before it closed takes the point
// back to it, as handling it would have.
func TestActor_StartTheClientSetAndLeftUnhandledAtTheCloseTakesThePointBackToIt(t *testing.T) {
	run := func(finishWait time.Duration) (point, to time.Time) {
		r := newActorRig(t)
		blockRecoveryPosts(r)
		r.startWith(types.AllMessageInterest, 1)
		r.postAlive(liveID, true)
		r.a.drain()
		first := r.a.machine.byID[liveID].active.requestID
		r.a.handle(factSnapshotComplete{session: sid(1), producer: liveID, requestID: first})
		r.clock.advance(10 * time.Second)
		r.postAlive(liveID, true)
		taken := r.stamp()
		r.a.postSample(factProcessed{session: sid(1), producer: liveID, generatedAt: taken.Wall(), taken: taken})
		r.drainAll()
		to = r.stamp().Wall().Add(-10 * time.Minute)
		r.a.finishWait = finishWait
		r.a.postEssential(factRewind{producer: liveID, to: to})
		r.closeBeforeRunning()
		return r.timestampForRecovery(liveID), to
	}
	handled, to := run(2 * time.Second)
	if !handled.Equal(to) {
		t.Fatalf("handled, the point = %v, want the start %v", handled, to)
	}
	unhandled, to := run(0)
	if !unhandled.Equal(to) {
		t.Fatalf("left unhandled, the point = %v, want the start %v as when it is handled", unhandled, to)
	}
}

// The close's own deadline bounds what the actor spends on the facts it holds: with it
// past, nothing is handled and the points are taken back.
func TestActor_CloseDeadlineBoundsWhatTheActorHandlesAsItCloses(t *testing.T) {
	r := newActorRig(t)
	h := r.record()
	r.startWith(types.AllMessageInterest, 1)
	n := len(h.all())
	r.a.postEssential(factChannelLost{session: sid(1)})
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	r.a.stopBounded(ctx) // sets the close, with its deadline already past
	go r.a.run()
	select {
	case <-r.a.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the actor did not stop")
	}
	if got := len(h.all()); got != n {
		t.Fatalf("the actor handled %d facts past the deadline of the close", got-n)
	}
	if got := r.a.counters.unhandledAtClose.Load(); got != 1 {
		t.Fatalf("unhandled-at-close = %d, want 1", got)
	}
}

// blockingTransport answers every request, but only once released, whatever its
// context says: a request that cannot be cancelled.
type blockingTransport struct {
	release chan struct{}
	entered chan struct{}
}

func (b *blockingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-b.release
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"application/xml"}},
		Body:       io.NopCloser(strings.NewReader(okBody)),
		Request:    req,
	}, nil
}

// The close joins the goroutines of the requests, bounded by its deadline: it reports
// that it did not complete while one is still running, and that it did once it has ended.
func TestActor_StopBoundedJoinsTheRequestsInFlightWithinItsDeadline(t *testing.T) {
	r := newActorRig(t)
	bt := &blockingTransport{release: make(chan struct{}), entered: make(chan struct{}, 1)}
	released := false
	release := func() {
		if !released {
			released = true
			close(bt.release)
		}
	}
	defer release()
	cfg := &minimalCfg{apiURL: "api.test", token: "tok"}
	blocked := api.New(cfg)
	blocked.SetHTTPClient(&http.Client{Transport: bt, Timeout: time.Minute})
	r.a.api = blocked
	r.startWith(types.AllMessageInterest, 1)
	r.postAlive(liveID, true)
	r.a.drain() // the request goes out and waits in the transport
	select {
	case <-bt.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the request never reached the transport")
	}
	go r.a.run()

	short, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if r.a.stopBounded(short) {
		t.Fatal("stopBounded reported completion with a request still running")
	}
	release()
	long, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if !r.a.stopBounded(long) {
		t.Fatal("stopBounded did not complete once the request had ended")
	}
}

// A fact handled while the actor closes asks for nothing: no request goes out once the
// actor is closing.
func TestActor_NoRequestGoesOutWhileTheActorIsClosing(t *testing.T) {
	r := newActorRig(t)
	r.startWith(types.AllMessageInterest, 1)
	r.postAlive(liveID, true) // the alive that would ask for the first recovery, left for the close
	r.closeBeforeRunning()
	r.a.detached.Wait()
	if got := r.hits.recover.Load(); got != 0 {
		t.Fatalf("%d recovery requests went out while the actor closed", got)
	}
	// nothing was even started: a request that was would have left its cancel behind
	if got := len(r.a.snapshotCancels); got != 0 {
		t.Fatalf("%d requests were started while the actor closed", got)
	}
}

// What a REST worker tells the machine: the API's error, or a refusal when it said no
// without one, or nothing when it accepted.
func TestAnswerOf_ASuccessIsAcceptedAndAFailureWithOrWithoutAnErrorIsNot(t *testing.T) {
	boom := errors.New("503")
	for _, tc := range []struct {
		success bool
		err     error
		want    error
	}{
		{true, nil, nil},
		{false, boom, boom},
		{true, boom, boom},
		{false, nil, errNotAccepted},
	} {
		if got := answerOf(tc.success, tc.err); !errors.Is(got, tc.want) && (got != nil || tc.want != nil) {
			t.Errorf("answerOf(%v, %v) = %v, want %v", tc.success, tc.err, got, tc.want)
		}
	}
}

// A request that the close cancelled, or one a newer request superseded, has no answer
// for the machine.
func TestActor_AnswerOfACancelledRequestIsNotPosted(t *testing.T) {
	r := newActorRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	r.a.answer(ctx, 1, true, nil)
	cancel()
	r.a.answer(ctx, 2, false, context.Canceled)
	r.a.essentialMu.Lock()
	defer r.a.essentialMu.Unlock()
	if len(r.a.essential) != 1 {
		t.Fatalf("%d answers queued, want the one of the request that was not cancelled", len(r.a.essential))
	}
	if f, ok := r.a.essential[0].(factAnswered); !ok || f.requestID != 1 || f.err != nil {
		t.Fatalf("queued %+v", r.a.essential[0])
	}
}

// ---- what wakes the actor

// A sample, a command and a turn that left work behind each wake the actor, without
// the clock's tick: the loop here ticks once an hour.
func TestActor_ASampleOrACommandWakesTheActorAndALeftOverQueueIsTakenWithoutATick(t *testing.T) {
	r := newActorRig(t)
	r.a.tickEvery = time.Hour
	h := r.record()
	r.run()
	r.a.postSample(factProcessed{session: sid(1), producer: liveID, taken: r.stamp()})
	eventually(t, "a sample to be handled", func() bool { return len(h.all()) == 1 })
	c := &recoverEventCommand{producerID: liveID, eventID: matchURN(1), reply: make(chan recoverEventReply, 1)}
	if err := r.a.postCommand(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.reply:
	case <-time.After(5 * time.Second):
		t.Fatal("a command was not taken without a tick")
	}
	r.stop()

	// more samples than a turn takes, queued before the loop runs: the turn that
	// leaves some behind wakes the actor for the rest
	again := newActorRig(t)
	again.a.tickEvery = time.Hour
	ha := again.record()
	total := samplesPerTurn*2 + 500
	for range total {
		again.a.postSample(factProcessed{session: sid(1), producer: liveID, taken: again.stamp()})
	}
	again.run()
	eventually(t, "every sample to be handled", func() bool { return len(ha.all()) == total })
}

// A command is handled after the essential facts posted before it, even those that
// arrive after the actor looked.
func TestActor_CommandTakenAfterAnEssentialFactWasPostedIsHandledAfterIt(t *testing.T) {
	r := newActorRig(t)
	h := r.record()
	posted := false
	r.a.beforeCommand = func() {
		if !posted {
			posted = true
			r.a.postEssential(factChannelLost{session: sid(1)})
			c := &recoverEventCommand{producerID: liveID, eventID: matchURN(1), reply: make(chan recoverEventReply, 1)}
			_ = r.a.postCommand(t.Context(), c)
		}
	}
	c := &recoverEventCommand{producerID: liveID, eventID: matchURN(2), reply: make(chan recoverEventReply, 1)}
	if err := r.a.postCommand(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	r.drainAll()
	got := kinds(h.all())
	want := []string{"factChannelLost", "*recovery.recoverEventCommand", "*recovery.recoverEventCommand"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("handled %v, want %v", got, want)
	}
}
