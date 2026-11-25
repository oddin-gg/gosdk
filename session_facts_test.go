package gosdk

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	"github.com/oddin-gg/gosdk/internal/recovery"
	"github.com/oddin-gg/gosdk/types"
)

// factLog records every call a session makes on the recovery processor, in
// order, with the arguments the per-session facts are made of.
type factLog struct {
	mu     sync.Mutex
	events []factEvent
}

type factEvent struct {
	kind        string
	session     uuid.UUID
	producer    int
	generatedAt time.Time
	taken       recovery.Stamp
	requestID   int
	subscribed  bool
	interest    types.MessageInterest
}

func (l *factLog) add(e factEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *factLog) kinds() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, 0, len(l.events))
	for _, e := range l.events {
		out = append(out, e.kind)
	}
	return out
}

func (l *factLog) of(kind string) []factEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []factEvent
	for _, e := range l.events {
		if e.kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func (l *factLog) OnFeedChannelLost(uuid.UUID)     {}
func (l *factLog) OnFeedChannelRestored(uuid.UUID) {}
func (l *factLog) OnFeedSessionGone(s uuid.UUID)   { l.add(factEvent{kind: "gone", session: s}) }
func (l *factLog) OnClientSessionOpened(s uuid.UUID, mi types.MessageInterest, at recovery.Stamp) {
	l.add(factEvent{kind: "opened", session: s, interest: mi, taken: at})
}
func (l *factLog) OnMessageProcessingStarted(s uuid.UUID, p int, taken recovery.Stamp) {
	l.add(factEvent{kind: "started", session: s, producer: p, taken: taken})
}
func (l *factLog) OnMessageProcessingEnded(s uuid.UUID, p int, ts time.Time) {
	l.add(factEvent{kind: "ended", session: s, producer: p, generatedAt: ts})
}
func (l *factLog) OnMessageProcessed(s uuid.UUID, p int, gen time.Time, taken recovery.Stamp, req int) {
	l.add(factEvent{kind: "processed", session: s, producer: p, generatedAt: gen, taken: taken, requestID: req})
}
func (l *factLog) OnSessionAlive(s uuid.UUID, p int, gen time.Time, taken recovery.Stamp, subscribed bool) {
	l.add(factEvent{kind: "session-alive", session: s, producer: p, generatedAt: gen, taken: taken, subscribed: subscribed})
}
func (l *factLog) OnAliveReceived(p int, ts types.MessageTimestamp, received recovery.Stamp, subscribed bool) {
	l.add(factEvent{kind: "system-alive", producer: p, generatedAt: ts.Created, taken: received, subscribed: subscribed})
}
func (l *factLog) OnSnapshotCompleteReceived(_ context.Context, s uuid.UUID, p, req int) error {
	l.add(factEvent{kind: "snapshot-complete", session: s, producer: p, requestID: req})
	return nil
}

// stepClock reads a wall clock and a monotonic one that tick one second and
// one millisecond per reading, so the stamp of every reading is its own and
// the order of the readings shows in both.
type stepClock struct{ n atomic.Int64 }

func (c *stepClock) Now() recovery.Stamp {
	n := c.n.Add(1)
	return recovery.NewStamp(time.Unix(1_700_000_000+n, 0), time.Duration(n)*time.Millisecond)
}

func (c *stepClock) reads() int64 { return c.n.Load() }

func factSession(l *factLog, clk recovery.Clock) *oddsFeedSessionImpl {
	return &oddsFeedSessionImpl{
		cacheManager:             &spyCacheNotifier{},
		feedMessageFactory:       &fakeMessageBuilder{builtMessage: oddsChangeStub{}},
		recoveryMessageProcessor: l,
		logger:                   discardLogger(),
		msgCh:                    make(chan sessionEnvelope, 4),
		sessionID:                uuid.New(),
		clock:                    clk,
	}
}

func oddsChangeFM(created time.Time, requestID *int) *types.FeedMessage {
	return &types.FeedMessage{
		BasicFeedMessage: types.BasicFeedMessage{Timestamp: types.MessageTimestamp{Created: created}},
		Message:          &feedXML.OddsChange{ProductID: 3, EventID: "od:match:1", RequestID: requestID},
	}
}

// The fact that a message is processed is not posted by the session when it
// hands the message over: it rides the envelope and is run by whoever
// settles the ack. It carries the session, the producer, the message's own
// timestamp, the request id of a recovery's message (0 for a live one) and
// the stamp taken when processing began.
func TestSession_ProcessedFactRidesTheEnvelopeWithIdsAndStamp(t *testing.T) {
	l := &factLog{}
	clk := &stepClock{}
	o := factSession(l, clk)
	created := time.Unix(1_699_999_000, 0)
	request := 77

	o.processFeedMessage(t.Context(), oddsChangeFM(created, &request), types.AllMessageInterest, nil, false, nil)

	if got := l.of("processed"); len(got) != 0 {
		t.Fatalf("processed was posted at hand-over, before any ack: %+v", got)
	}
	env := <-o.msgCh
	if env.settled == nil {
		t.Fatal("the envelope of a message handed to the client carries no processed fact")
	}
	if got, want := l.kinds(), []string{"started", "ended"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("calls before the ack = %v, want %v", got, want)
	}

	env.settled()

	got := l.of("processed")
	if len(got) != 1 {
		t.Fatalf("processed facts = %d, want 1", len(got))
	}
	p := got[0]
	if p.session != o.sessionID || p.producer != 3 || p.requestID != request || !p.generatedAt.Equal(created) {
		t.Fatalf("processed = %+v, want session %s producer 3 request %d generatedAt %v", p, o.sessionID, request, created)
	}
	if p.taken.Mono() != time.Millisecond {
		t.Fatalf("taken = %v, want the first reading of the clock (1ms)", p.taken.Mono())
	}
	if clk.reads() != 1 {
		t.Fatalf("clock read %d times for one message, want 1: taken is stamped once, at the start", clk.reads())
	}
}

func TestSession_ProcessedFactOfALiveMessageHasRequestIDZero(t *testing.T) {
	l := &factLog{}
	o := factSession(l, &stepClock{})
	o.processFeedMessage(t.Context(), oddsChangeFM(time.Unix(1_699_999_000, 0), nil), types.AllMessageInterest, nil, false, nil)
	(<-o.msgCh).settled()
	if got := l.of("processed"); len(got) != 1 || got[0].requestID != 0 {
		t.Fatalf("processed = %+v, want one with request id 0", got)
	}
}

// Two messages of one session are stamped in the order they were taken.
func TestSession_TakenStampsOrderWithTheMessages(t *testing.T) {
	l := &factLog{}
	o := factSession(l, &stepClock{})
	for range 2 {
		o.processFeedMessage(t.Context(), oddsChangeFM(time.Unix(1_699_999_000, 0), nil), types.AllMessageInterest, nil, false, nil)
	}
	(<-o.msgCh).settled()
	(<-o.msgCh).settled()
	got := l.of("processed")
	if len(got) != 2 {
		t.Fatalf("processed facts = %d, want 2", len(got))
	}
	if !(got[0].taken.Mono() < got[1].taken.Mono()) || !got[0].taken.Wall().Before(got[1].taken.Wall()) {
		t.Fatalf("taken stamps out of order: %+v then %+v", got[0].taken, got[1].taken)
	}
}

// What the client does not receive is not processed by it: an unparsable
// message, a snapshot_complete and an alive carry no processed fact.
func TestSession_NoProcessedFactWithoutAMessageForTheClient(t *testing.T) {
	l := &factLog{}
	o := factSession(l, &stepClock{})
	ctx := t.Context()
	subscribed := 1
	o.processFeedMessage(ctx, &types.FeedMessage{Message: &feedXML.Alive{ProductID: 3, Subscribed: &subscribed}}, types.AllMessageInterest, nil, false, nil)
	o.processFeedMessage(ctx, &types.FeedMessage{Message: &feedXML.SnapshotComplete{ProductID: 3, RequestID: 5}}, types.AllMessageInterest, nil, false, nil)
	if got := l.of("processed"); len(got) != 0 {
		t.Fatalf("processed facts = %+v, want none", got)
	}
	if len(o.msgCh) != 0 {
		t.Fatalf("an alive or a snapshot_complete reached the client")
	}
}

// An alive of a session of the client is that session's fact, with the
// stamp of the moment it was taken, and no longer goes through the alive
// channel's hook.
func TestSession_AliveOfAClientSessionIsASessionFact(t *testing.T) {
	l := &factLog{}
	o := factSession(l, &stepClock{})
	created := time.Unix(1_699_999_000, 0)
	subscribed := 1
	fm := &types.FeedMessage{
		BasicFeedMessage: types.BasicFeedMessage{Timestamp: types.MessageTimestamp{Created: created}},
		Message:          &feedXML.Alive{ProductID: 2, Subscribed: &subscribed},
	}

	o.processFeedMessage(t.Context(), fm, types.LiveOnlyMessageInterest, nil, false, nil)

	got := l.of("session-alive")
	if len(got) != 1 {
		t.Fatalf("session alive facts = %d, want 1", len(got))
	}
	a := got[0]
	if a.session != o.sessionID || a.producer != 2 || !a.generatedAt.Equal(created) || !a.subscribed {
		t.Fatalf("session alive = %+v", a)
	}
	if a.taken.Mono() != time.Millisecond {
		t.Fatalf("taken = %v, want 1ms", a.taken.Mono())
	}
	if n := len(l.of("system-alive")); n != 0 {
		t.Fatalf("a client session's alive also went to the alive channel hook (%d)", n)
	}
}

// The SDK's own alive session posts no fact of a session's: its alives are
// the alive channel's, and it does not take part in the client's processing.
// A client subscription of the alive-only interest is the same: no lane.
func TestSession_AliveOnlySessionPostsNoSessionFacts(t *testing.T) {
	l := &factLog{}
	o := factSession(l, &stepClock{})
	subscribed := 1
	created := time.Unix(1_699_999_000, 0)
	fm := &types.FeedMessage{
		BasicFeedMessage: types.BasicFeedMessage{Timestamp: types.MessageTimestamp{Created: created}},
		Message:          &feedXML.Alive{ProductID: 1, Subscribed: &subscribed},
	}

	o.processFeedMessage(t.Context(), fm, types.SystemAliveOnly, nil, false, nil)

	if got, want := l.kinds(), []string{"system-alive"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("calls = %v, want only %v", got, want)
	}
	a := l.of("system-alive")[0]
	if a.session != (uuid.UUID{}) || a.producer != 1 || !a.generatedAt.Equal(created) || !a.subscribed {
		t.Fatalf("system alive = %+v", a)
	}

	// A message that is not an alive, on the same interest, is not the
	// client's processing either: whatever reaches the pump carries no
	// processed fact.
	o.processFeedMessage(t.Context(), oddsChangeFM(created, nil), types.SystemAliveOnly, nil, false, nil)
	select {
	case env := <-o.msgCh:
		if env.settled != nil {
			env.settled()
		}
	default:
	}
	if got := l.of("processed"); len(got) != 0 {
		t.Fatalf("an alive-only session posted processed facts: %+v", got)
	}
}

// A snapshot_complete names its session.
func TestSession_SnapshotCompleteCarriesTheSessionID(t *testing.T) {
	l := &factLog{}
	o := factSession(l, &stepClock{})
	fm := &types.FeedMessage{Message: &feedXML.SnapshotComplete{ProductID: 3, RequestID: 42}}
	o.processFeedMessage(t.Context(), fm, types.PrematchOnlyMessageInterest, nil, false, nil)
	got := l.of("snapshot-complete")
	if len(got) != 1 || got[0].session != o.sessionID || got[0].producer != 3 || got[0].requestID != 42 {
		t.Fatalf("snapshot complete = %+v", got)
	}
}

// The processed fact goes out when the pump has settled the delivery, after
// the broker's ack: with the public buffer full nothing is acked and nothing
// reported; once the message is admitted the ack comes first, the fact next.
func TestPump_ProcessedFactFollowsTheAck(t *testing.T) {
	_, sess, sub := newPumpHarness(t, 1, nil)

	var mu sync.Mutex
	var order []string
	note := func(s string) func() {
		return func() { mu.Lock(); order = append(order, s); mu.Unlock() }
	}
	snapshot := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), order...)
	}
	waitFor := func(want ...string) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for len(snapshot()) < len(want) {
			if time.Now().After(deadline) {
				t.Fatalf("order = %v, want %v", snapshot(), want)
			}
			time.Sleep(time.Millisecond)
		}
		if got := snapshot(); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}

	sess.respCh <- sessionEnvelope{ack: note("ack1"), settled: note("fact1")}
	waitFor("ack1", "fact1")

	// The public buffer (capacity 1) is full: the second message is held.
	sess.respCh <- sessionEnvelope{ack: note("ack2"), settled: note("fact2")}
	time.Sleep(20 * time.Millisecond)
	if got := snapshot(); len(got) != 2 {
		t.Fatalf("a held message was acked or reported: %v", got)
	}

	<-sub.messages // the client takes one: the held message is admitted
	waitFor("ack1", "fact1", "ack2", "fact2")
	<-sub.messages
}

// A delivery whose ack has not settled when the pump stops reports nothing:
// the checkpoint made of the facts is never ahead of what the broker has let
// go.
func TestPump_NoProcessedFactWhenTheAckDoesNotSettle(t *testing.T) {
	c := &Client{logger: discardLogger()}
	sub := &Subscription{
		id:         uuid.New(),
		messages:   make(chan types.SessionMessage, 1),
		pumpStop:   make(chan struct{}),
		pumpDone:   make(chan struct{}),
		closed:     make(chan struct{}),
		client:     c,
		underlying: &pumpFakeSession{respCh: make(chan sessionEnvelope)},
	}
	release := make(chan struct{})
	var settled atomic.Int32
	result := make(chan bool, 1)
	go func() {
		result <- c.deliverToSubscription(sub, sessionEnvelope{
			ack:     func() { <-release },
			settled: func() { settled.Add(1) },
		})
	}()

	time.Sleep(20 * time.Millisecond) // the ack is in flight, the buffer took the message
	close(sub.pumpStop)
	if <-result {
		t.Fatal("deliver reported success while the ack was unsettled and the pump stopped")
	}
	close(release)
	time.Sleep(20 * time.Millisecond)
	if settled.Load() != 0 {
		t.Fatalf("a fact was posted for a delivery whose ack was abandoned: %d", settled.Load())
	}
}
