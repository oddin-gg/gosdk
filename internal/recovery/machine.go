package recovery

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

// machine is every producer's recovery state and the rules that change it: the
// state machine the recovery actor runs. Each method is one fact, handled
// to the end before the next. What the machine decides goes out through the
// outbox, and the outcome of a request comes back as another fact. Nothing
// here blocks, locks, reads the time on its own or calls the client.
//
// Gaps. A gap is what a session may be missing of a producer's messages, and
// where it starts. A session's queue loses what it held when the connection or
// its channel goes: that gap starts at the session's checkpoint, the running
// maximum of the timestamps of the live messages and subscribed alives it has
// finished, or the time of the request whose snapshot_complete it has seen. A
// producer that stops sending (an alive that says the feed is unsubscribed, or
// no alive for too long) leaves a gap in every session from its last
// subscribed alive, which says everything before it was sent; the queues
// still hold whatever they had. A recovery starts from the oldest open gap.
// Snapshot messages never move a checkpoint, and a gap keeps its start until a
// recovery that covers it completes, so asking again starts from the same
// point as the first time. The oldest of the open gaps' starts and the
// sessions' checkpoints is the producer's resume point: where a client that
// restarts resumes from, published through publishResumePoints.
//
// Recoveries. A producer has at most one recovery in flight; whatever happens
// meanwhile joins it. A recovery covers the gaps open when it was asked for. It
// completes once every session that receives the producer has seen its
// snapshot_complete; a gap opened later asks for one more. A loss that takes
// the recovery's own snapshot with it (the connection, or the channel of a
// session that has not seen the snapshot_complete yet) gives it up, and a new
// one is asked for. So does a gap of the producer's, which may have lost the
// request: an alive still unsubscribed after the API accepted it, or no alive
// for too long; the next alive asks again. A recovery is asked for only while
// an alive says the producer is there, the transport has reported the
// connection up, and every session that receives the producer has a bound
// queue. One that fails, or times out (no snapshot_complete within the
// snapshot-complete timeout once nothing more of it comes, or within the
// maximum recovery time at all), is asked for again with backoff, at most
// recoveryReissues times in a row; after that the producer stays down and the
// request is repeated at a slow, steady pace until it succeeds or an alive
// arrives after a gap.
//
// Time. Every duration the machine measures between two local events
// (a producer's inactivity, a request's deadlines, the backoffs) runs on the
// monotonic side of the clock, so a step of the wall clock neither fires a
// deadline nor holds one back. What the machine publishes and what it compares
// with a producer's own timestamps (the clock offset, the cap at the
// producer's now) is wall time. A step of the wall clock after an offset was
// measured would read as an age, so the offset is moved by the step where it is
// read: see clockOffset.at.
//
// Event recoveries. The client may ask for one event's messages again, odds
// only or stateful; any number run beside a producer's recovery, and each is
// its own request. One asked for while another of the same event and kind is
// in flight is that one; a producer has at most maxPendingEventRecoveries in
// flight, and one more is refused. Like a recovery, an event recovery waits for
// the snapshot_complete of every session that receives the producer when it
// was asked for, and completes once each has seen it; a session that closes
// stops being waited for. It does not wait for the producer to be up, and a
// producer going down does not end it: only the loss of a queue does, the
// connection or a session's channel, since that takes its snapshot_complete
// with it. It fails when the API refuses it and times out at the maximum
// recovery time, or when nothing of it is heard for the snapshot-complete
// timeout after the API accepted it. One asked for while a session that
// receives the producer has no queue bound waits for it, since its snapshot
// could go to either queue or to none, and is asked for once the queue is
// back. Whatever is in flight when the feed closes fails, and one asked for once
// the feed is closing is refused. A producer the catalog has disabled is no reason
// to refuse one.
//
// The safety net. A session whose live messages of a producer stay older than the
// stale limit, by the producer's clock, for the stale window is too far behind:
// the broker's queue for it holds a backlog that only grows. The net first asks
// for a recovery of every producer the session receives that can be asked for
// now, from the session's checkpoint, and once the API has accepted them all it
// replaces the session's channel, which drops the queue and what it held. The
// recoveries are then asked for again, as for a lost queue, since part of what
// they sent went with the old queue. The net measures against the offset of the
// producer's clock taken on the alive channel, and is off while that is stale. It
// is paused for a producer while a recovery of it is in flight or a gap is open,
// while the session's reset is pending or under way, and for messages taken before
// the last recovery ended: a snapshot ahead of a live message makes it old on every
// session. A session is reset at most safetyNetResets times per safetyNetCooldown,
// with a backoff that doubles between resets; once spent it is lagging, which is a
// warning only and takes no producer down by itself (a session that processes a
// producer later than the maximum inactivity keeps it down for that delay all the
// same). A request of the net's that the API does not accept, or a producer that is
// disabled before its request is accepted, cancels the reset, and the net waits
// before it tries again: 1, 2 or 4 minutes, by the resets already made within the
// cool-down, as between resets. So does a snapshot_complete of the session's own for one
// of the recoveries it waits for: the session has what it sent. The
// snapshot_completes of a session whose reset is under way are ignored, since they
// come from the queue being replaced, and the reset gives every lane of the session a
// new gap, so that no recovery asked before it can close it.
//
// A session is not reset when its checkpoint of any producer the catalog has enabled
// lies before where that producer's recovery would start, clamped to its stateful
// recovery window, or within resetWindowMargin of that start (at most half the
// window): what the reset drops of it no recovery repeats. The margin covers the
// time from the last acceptance to the recovery asked into the new queue, during
// which the window moves on; it is a margin, not a proof. The session is lagging
// instead, with a warning of its own for the episode, and nothing is asked. The same
// check is made when the last acceptance arrives; a reset that fails it is cancelled. Once the feed is closing the resets waiting for the API
// are cancelled, and an acceptance handled after that makes none.
//
// The stale window counts only time spent stale: one that began before the last
// recovery of the producer ended starts again, a recovery that took no sample of the
// session included.
//
// The zero time.Time stands for "none" where a time is optional, and for "a
// full snapshot" where it is a gap's start or a request's.
//
// Not safe for concurrent use: the recovery actor's goroutine is the only one
// that calls it.
type machine struct {
	catalog  producerCatalog
	settings recoverySettings
	out      outbox
	clock    Clock
	log      *log.Logger
	// nextID gives a request id that is not in use by a request in flight.
	nextID func() int
	// seq is the sequence number of the last request sent: what orders requests,
	// since two can be sent at one reading of the clock.
	seq      uint64
	counters machineCounters

	// tracks is every producer the machine knows, in the order it learned them.
	tracks []*track
	byID   map[int]*track
	// sessions are the sessions of the client, in the order they opened.
	sessions []*session

	started bool
	// closing says the feed is closing: from then on a resume point is
	// published only when it goes back, so sessions closing one by one at
	// shutdown cannot carry it past what one of them had left.
	closing bool
	// closed says the feed has closed: nothing is asked for after it.
	closed bool
	// startedAt is when the feed opened, on the monotonic clock.
	startedAt time.Duration
	// connected says the transport has reported its connection up with every
	// session's channel bound: not before its first up, so the first recovery
	// cannot go out to queues that do not exist yet.
	connected bool
	// gapSeq is the number of the last gap opened; a recovery covers the gaps
	// up to the number at its start.
	gapSeq uint64
	// resetSeq is the number of the last reset the safety net set out to make.
	resetSeq uint64

	// eventRecoveries are the event recoveries in flight, by request id.
	eventRecoveries map[int]*eventActive
	// deferred are the event recoveries asked for while a session that receives
	// the producer awaited its channel, in the order they were asked for. Each
	// has its request id already, which no other request takes.
	deferred []deferredEvent
}

// machineCounters count what happened, for tests and logs.
type machineCounters struct {
	requested          int
	reissued           int
	completed          int
	failed             int
	timedOut           int
	abandoned          int
	unknownCompletions int
	unknownProducers   int
	eventRequested     int
	eventRefused       int
	eventExpired       int
	eventAbandoned     int
	// resets are the resets the transport made; resetRequestsFailed the resets
	// the net gave up because a recovery it asked for was refused, failed or
	// became unnecessary; lagging how often a session began to lag.
	resets              int
	resetRequestsFailed int
	lagging             int
}

func newMachine(
	catalog producerCatalog,
	settings recoverySettings,
	out outbox,
	clock Clock,
	logger *log.Logger,
	nextID func() int,
) *machine {
	m := &machine{
		catalog:  catalog,
		settings: settings,
		out:      out,
		clock:    clock,
		log:      logger,
		nextID:   nextID,
		byID:     make(map[int]*track),

		eventRecoveries: make(map[int]*eventActive),
	}
	for _, producer := range catalog.producers() {
		m.addTrack(producer)
	}
	return m
}

// ---- the lifecycle

// sessionOpened registers a session whose queue is bound, before or after
// start. A session whose interest has a lane gets one for every producer it
// covers. openedAt is a reading of the clock taken by the caller before the
// session's queue is bound, so no message in the queue is older than the
// lane's checkpoint.
func (m *machine) sessionOpened(id uuid.UUID, interest types.MessageInterest, openedAt Stamp) {
	m.openSession(id, interest, openedAt, true)
}

// sessionOpening registers a session whose queue is not bound yet, as
// sessionOpened does: nothing is asked for its producers, and no event
// recovery of theirs is sent, until channelReopened says the queue is bound. A
// recovery asked for before would send its snapshot to a queue that does not
// exist, and its snapshot_complete with it.
func (m *machine) sessionOpening(id uuid.UUID, interest types.MessageInterest, openedAt Stamp) {
	m.openSession(id, interest, openedAt, false)
}

func (m *machine) openSession(id uuid.UUID, interest types.MessageInterest, openedAt Stamp, bound bool) {
	if m.session(id) != nil {
		m.log.Warnf("Session %s opened twice; the second is ignored", id)
		return
	}
	s := &session{id: id, interest: interest, lost: !bound}
	m.sessions = append(m.sessions, s)
	if !hasLane(interest) {
		// a client's alive-only session receives no producer's messages: no
		// lane, so its channel's loss and its pace take no producer down, and
		// no recovery waits for its snapshot_complete
		return
	}
	for _, t := range m.tracks {
		if interest.IsProducerInScope(t.producer) {
			m.addLane(s, t, openedAt)
		}
	}
	for _, l := range s.lanes {
		if l.gap != nil {
			m.maybeRequest(m.byID[l.producer], m.mono())
		}
	}
}

// addLane gives the session a lane of the producer.
//
// Before start, a lane has nothing yet: start gives it its gap. After it, the
// lane of a producer that has not completed a first recovery gets the same gap
// the lanes got at start, from the client's seed, so what it never received is
// asked for. A lane of a producer that has recovered starts at the producer's
// now, with no gap: it has nothing from before it, and misses nothing a
// recovery could bring; its checkpoint is that reading of the producer's clock
// at openedAt, the reading taken before the session's queue was bound.
func (m *machine) addLane(s *session, t *track, openedAt Stamp) {
	l := &lane{producer: t.id}
	s.lanes = append(s.lanes, l)
	if !m.started {
		return
	}
	if !t.everRecovered {
		l.gap = m.openGap(nil, m.seed(t), false)
		l.checkpoint = l.gap.from
		// a producer that is up with a recovery to make is down until it is made
		m.markDown(t, causeSessionOpened)
		return
	}
	l.checkpoint = t.producerNow(openedAt)
}

// sessionClosed removes a session: it leaves the checkpoints and the
// completions at once, so a recovery that waited only for it completes. Once
// the feed is closing, which is when the sessions close, it leaves the
// checkpoints only: a recovery it was waiting for has not completed, and no
// producer is up for it.
func (m *machine) sessionClosed(id uuid.UUID) {
	s := m.removeSession(id)
	if s == nil || m.closing {
		return
	}
	mono := m.mono()
	for _, l := range s.lanes {
		t := m.byID[l.producer]
		a := t.active
		if a != nil && a.awaited[id] {
			delete(a.awaited, id)
			seenAny := len(a.seen) > 0
			delete(a.seen, id)
			// a snapshot_complete stands for the API's acceptance; with none
			// seen, and none left to wait for, the API's answer decides
			if a.allSeen() && (a.accepted || seenAny) {
				m.complete(t, a, mono)
			}
		} else if a == nil {
			m.upIfNothingMissing(t)
		}
	}
	m.releaseDeferred()
	for _, e := range m.sortedEvents() {
		if !e.awaited[id] {
			continue
		}
		delete(e.awaited, id)
		delete(e.seen, id)
		// as for a recovery: with no snapshot_complete seen, and none left to wait
		// for, the API's answer decides (a snapshot_complete seen has accepted it)
		if e.allSeen() && e.accepted {
			m.eventCompleted(e)
		}
	}
}

// start opens the feed: every session misses everything before now, from the
// recovery point the client set, or from nothing, a full snapshot. Each producer
// asks at its first alive once the transport has reported its first
// connectionUp.
//
// A gap a session's lane got before (the connection down or its channel lost,
// told before the start) is replaced, not joined: nothing was received before
// the start, so there is nothing more to miss than the seed says, and joining
// it at the checkpoint the lane started from would ask for a full snapshot.
func (m *machine) start() {
	if m.started {
		return
	}
	m.started = true
	m.startedAt = m.mono()
	for _, s := range m.sessions {
		for _, l := range s.lanes {
			l.gap = m.openGap(nil, m.seed(m.byID[l.producer]), false)
			l.hasLoss = false
			// the start it got (an initial interval counted back from now, say)
			// is where a later loss starts too, while the session has processed
			// nothing to move it
			l.checkpoint = l.gap.from
		}
	}
}

// close is the end of the feed: the event recoveries in flight have failed, and
// so have the ones that waited for a channel. Nothing is asked for after it.
func (m *machine) close() {
	m.closed = true
	for _, e := range m.sortedEvents() {
		m.endEvent(e, types.RecoveryStatusFailed, errFeedClosing)
	}
	for _, d := range m.deferred {
		m.failDeferred(d, errFeedClosing)
	}
	m.deferred = nil
}

// beginClosing says the feed has begun to close: from now on the published
// resume points only go back. A session that closes takes its checkpoint out of
// the point; at shutdown that would move the point past its unprocessed
// messages, and the point the client persists would depend on the order the
// sessions close in. A gap opened meanwhile still takes the point back.
//
// The resets that wait for the API are cancelled: an acceptance still queued is
// handled as the recovery's, and drops nothing, since a reset then would take what
// the shutdown is about to settle.
func (m *machine) beginClosing() {
	m.closing = true
	mono := m.mono()
	for _, s := range m.sessions {
		if s.pending != nil {
			m.cancelReset(s, mono)
		}
	}
}

// unhandledAtClose says the actor closed with facts still queued that it did not
// handle: each published resume point goes back to the producer's last subscribed
// alive, and to the earliest time among the facts left that could have taken it back
// further (backTo, by producer): the start the client set, and the mark of an alive,
// whichever alive it is, since an unsubscribed alive opens its gap from the safe point
// there is, a subscribed alive left before it moves that safe point, and with none the
// gap starts from the alive's own mark. Nothing else left can take a point back: a
// session opened among them starts from the point there is, and a lost queue from a
// checkpoint. The rule gives the point the facts would, or an earlier one: it is never
// later than handling them would have made it, and where it cannot tell it goes
// earlier, as every point only goes back once the feed is closing. A producer with no
// point published, because the feed never started or no session received it, keeps the
// client's start.
func (m *machine) unhandledAtClose(backTo map[int]time.Time) {
	for _, t := range m.tracks {
		if t.published == nil {
			continue
		}
		floor := *t.published
		if !t.safePoint.IsZero() {
			floor = earlierOf(floor, t.safePoint)
		}
		if at, ok := backTo[t.id]; ok {
			floor = earlierOf(floor, at)
		}
		if floor.Equal(*t.published) {
			continue
		}
		m.log.Warnf("The recovery actor closed with facts it could not handle: producer %d resumes from %s",
			t.id, fmtResume(floor))
		t.published = &floor
		m.out.resumePoint(t.id, floor)
	}
}

// fmtResume is a resume point for the log.
func fmtResume(point time.Time) string {
	if point.IsZero() {
		return "a full snapshot"
	}
	return point.UTC().Format(time.RFC3339Nano)
}

// rewind is the client setting the producer's recovery start after the feed has
// started; the zero time never reaches it, it does nothing.
//
// Until the producer has completed a first recovery its start is the seed, and a value
// the client sets replaces it: the lanes that got the seed gap start from the new
// value, unless a loss of their queue opened the gap from an earlier checkpoint, which
// a start never moves, and so does every lane opened later. A value the seed already
// has does nothing. The recovery that is in flight, if any, keeps what it asked for,
// and does not close the new start: one more is asked for from it.
//
// Once the producer has recovered, the point the client sets only lowers the next
// recovery's start: it opens no request by itself, it is a gap that does not trigger
// one, folded into the start of the next request and into the published resume point,
// and closed only when a recovery that covers it completes, so a failed request keeps
// it. A point later than the one the producer is at is ignored, with a warning, and
// the point it is at, which the client set already or a loss left, does nothing.
//
// Before the start the client's value is the seed, which the actor reads as the feed
// opens.
func (m *machine) rewind(producerID int, to time.Time) {
	if !m.started {
		return
	}
	t := m.known(producerID)
	if t == nil {
		return
	}
	if !t.everRecovered {
		m.reseed(t, to)
		return
	}
	current := m.currentPoint(t)
	if to.After(current) {
		m.log.Warnf("The recovery timestamp of producer %d, %s, is later than the point it is at, %s: ignored",
			t.id, to.UTC().Format(time.RFC3339Nano), current.UTC().Format(time.RFC3339Nano))
		return
	}
	if to.Equal(current) {
		// nothing to lower: a rewind to the point it is at would outlive the recovery
		// that covers it, and hold the resume point there
		return
	}
	t.rewind = m.openGap(t.rewind, to, false)
}

// reseed replaces the seed of a producer that has not completed a first recovery: what
// its lanes got from the old one (the gap, and the checkpoint that no message moved)
// becomes the new one.
func (m *machine) reseed(t *track, to time.Time) {
	if m.seed(t).Equal(to) {
		return
	}
	t.seeded = &to
	for _, s := range m.sessions {
		l := s.lane(t.id)
		if l == nil || l.gap == nil {
			continue
		}
		from := to
		if l.hasLoss {
			// the queue was lost at a checkpoint: what it held is missing whatever the start
			from = earlierOf(to, l.lossFrom)
		}
		if l.checkpoint.Equal(l.gap.from) {
			// no message moved it past the seed
			l.checkpoint = from
		}
		// a new number, so a recovery already asked for does not close it: it
		// covered what the old seed said
		m.gapSeq++
		l.gap.from, l.gap.seq = from, m.gapSeq
	}
}

// currentPoint is where the producer is for a rewind to be before: its resume
// point, else with no session to have one the last subscribed alive, else the
// client's start.
func (m *machine) currentPoint(t *track) time.Time {
	if point, ok := m.resumePoint(t); ok {
		return point
	}
	if !t.safePoint.IsZero() {
		return t.safePoint
	}
	return m.seed(t)
}

// ---- the transport

// connectionDown says the connection is gone, and every queue with what it
// held: each session misses everything after its checkpoint, and the
// recoveries in flight lose their snapshots.
func (m *machine) connectionDown() {
	m.connected = false
	mono := m.mono()
	for _, t := range m.tracks {
		if t.active != nil {
			m.giveUp(t, mono, "its snapshot went with the connection")
		}
	}
	for _, s := range m.sessions {
		s.pending = nil
		for _, l := range s.lanes {
			m.loseLane(l)
		}
	}
	m.giveUpEvents(nil)
	for _, t := range m.tracks {
		if m.receivers(t) > 0 {
			m.markDown(t, causeConnectionLost)
		}
	}
}

// connectionUp says the connection is up with every channel open, the first
// time or back after a loss: recoveries go out now to producers whose last
// alive is recent, else at their next alives.
func (m *machine) connectionUp() {
	m.connected = true
	mono := m.mono()
	for _, t := range m.tracks {
		m.maybeRequest(t, mono)
		m.upIfNothingMissing(t)
	}
}

// channelLost says a session's channel was lost: its queue lost what it held. A
// recovery that waited for this session's snapshot_complete loses it too.
// Nothing is asked for the session's producers until channelReopened says a new
// queue is bound: what the feed sends before then reaches no queue,
// snapshot_complete included.
//
// Only a session's own channel is a fact: the SDK's internal alive channel is
// no session, and losing it alone opens no gap.
func (m *machine) channelLost(id uuid.UUID) {
	s := m.session(id)
	if s == nil {
		return
	}
	s.pending = nil
	s.lost = true
	m.queueLost(s, causeChannelLost, m.mono())
}

// channelReopened says a new channel replaced the one lost: its queue is bound,
// and what the session misses is asked for now, or at the next alives.
func (m *machine) channelReopened(id uuid.UUID) {
	s := m.session(id)
	if s == nil || !s.lost {
		return
	}
	s.lost = false
	mono := m.mono()
	for _, l := range s.lanes {
		m.maybeRequest(m.byID[l.producer], mono)
	}
	m.releaseDeferred()
}

// resetDone says the reset of the session's channel numbered number ended: replaced
// when the channel was closed and a new one is bound. The session's queue lost what
// it held, as with a lost channel: the recoveries it waited for were asked for
// before the reset, so part of what they sent went with the old queue, and they are
// given up and asked for again.
//
// A reset with the transport stays one through a lost connection or channel:
// replacing the channel late can still drop what the new one holds, so its report
// counts all the same. The report of a reset that is not the one underway, or of
// none, changes nothing. Only a reset made counts against the cap and the backoff;
// one the transport could not make (replaced is false, nothing was dropped) leaves
// the cap alone, and the backoff as long as after the last reset made, but the
// snapshot_completes ignored meanwhile are gone, and recovered by asking again.
func (m *machine) resetDone(id uuid.UUID, number uint64, replaced bool) {
	s := m.session(id)
	if s == nil || s.underway == 0 || s.underway != number {
		return
	}
	s.underway = 0
	mono := m.mono()
	m.pruneResets(s, mono)
	if replaced {
		s.resets = append(s.resets, mono)
		s.nextResetAt = mono + backoff(firstResetBackoff, len(s.resets))
		m.counters.resets++
		if s.lagging {
			s.lagging = false
			m.log.Infof("Session %s caught up", id)
		}
		m.queueLost(s, causeSafetyNetReset, mono)
	} else {
		m.log.Warnf("The safety net's reset of session %s was not made", id)
		s.nextResetAt = mono + backoff(firstResetBackoff, max(1, len(s.resets)))
		m.notSeen(s, mono)
	}
	m.releaseDeferred()
}

// pruneResets forgets the session's resets that are older than the cool-down at mono:
// the cap and the backoff count the resets of the last safetyNetCooldown only.
func (m *machine) pruneResets(s *session, mono time.Duration) {
	for len(s.resets) > 0 && mono-s.resets[0] >= safetyNetCooldown {
		s.resets = s.resets[1:]
	}
}

// loseLane opens the lane's gap from its checkpoint, and remembers where the loss
// began: a start the client sets later replaces the seed, never this.
func (m *machine) loseLane(l *lane) {
	l.above = false
	l.gap = m.openGap(l.gap, l.checkpoint, false)
	if l.hasLoss {
		l.lossFrom = earlierOf(l.lossFrom, l.checkpoint)
	} else {
		l.hasLoss, l.lossFrom = true, l.checkpoint
	}
}

// queueLost says the session's queue lost what it held: it misses everything
// after its checkpoints, and a recovery that waited for its snapshot_complete
// will not see it.
func (m *machine) queueLost(s *session, cause statusCause, mono time.Duration) {
	for _, l := range s.lanes {
		m.loseLane(l)
	}
	for _, l := range s.lanes {
		m.markDown(m.byID[l.producer], cause)
	}
	m.notSeen(s, mono)
}

// notSeen says the session will not see the snapshot_completes it has not seen
// yet: the recoveries and event recoveries waiting for them are given up, and asked for again as far
// as anything is missing.
func (m *machine) notSeen(s *session, mono time.Duration) {
	m.giveUpEvents(s)
	for _, l := range s.lanes {
		t := m.byID[l.producer]
		if a := t.active; a != nil && a.awaited[s.id] && !a.seen[s.id] {
			m.giveUp(t, mono, "its snapshot went with session "+s.id.String()+"'s queue")
		}
		m.maybeRequest(t, mono)
	}
}

// ---- the feed

// alive is an alive from the SDK's own alive channel: the producer is there. It
// measures the producer's clock offset and alive interval; subscribed, it is the
// point everything before was sent; unsubscribed, the producer has stopped
// sending since the last one that was. One without a positive timestamp is
// ignored: as the producer's clock it would put the producer's now at or before
// the epoch, and every recovery and resume point capped at it with it.
//
// markAt is what the safe point is taken from: generatedAt, or the producer's now
// at receipt when the alive is stamped ahead of it. The offset is measured from
// generatedAt as it is: it is the producer's clock. received is when the SDK
// received it: the wall reading is compared with the producer's clock and
// published, the monotonic one measures the alive interval and orders it against
// the recovery's acceptance.
func (m *machine) alive(producerID int, generatedAt, markAt time.Time, received Stamp, subscribed bool) {
	t := m.known(producerID)
	if t == nil || !m.catalog.enabled(t.id) {
		return
	}
	if !validStamp(generatedAt) {
		return
	}
	mono := m.mono()
	// the alive after the one that gave a recovery up: it may be asked for again
	t.awaitsAlive = false
	if t.hasAlive {
		// a second at least, and the maximum inactivity at most, unless that is less
		measured := min(max(received.mono-t.lastAliveAt, time.Second), max(time.Second, m.settings.maxInactivity))
		t.aliveInterval += (measured - t.aliveInterval) / 4
	}
	t.hasAlive = true
	t.lastAliveAt = received.mono
	t.lastAliveMark = markAt
	t.offset = measureOffset(generatedAt, received)
	if t.inactive {
		// an alive after a gap re-arms the cap at once
		t.inactive = false
		t.rearm()
	}
	if subscribed {
		t.safePoint = laterOf(t.safePoint, markAt)
	} else {
		m.openProducerGap(t, markAt)
		if a := t.active; a != nil && a.accepted && received.mono >= a.acceptedAt {
			// still unsubscribed after the producer took the request: it lost it,
			// restarting say, and will send neither the snapshot nor its
			// snapshot_complete
			m.giveUp(t, mono, "the producer says unsubscribed since it accepted it")
			t.awaitsAlive = true
		}
		m.markDown(t, causeUnsubscribed)
	}
	m.maybeRequest(t, mono)
	if subscribed && t.down && m.started && m.connected && !m.closing && m.receivers(t) == 0 {
		// no session has a lane of this producer, so there is nothing to
		// recover: a fresh alive is all it takes to be up, and a gap opened
		// while no lane received it is of no use to a session that opens later,
		// which starts at the producer's now. Not while the feed closes: nothing
		// is up for a session that has closed at shutdown.
		t.gap = nil
		m.bringUp(t, causeRecoveryCompleted)
	}
}

// processed says a session has finished a message of the producer. A live one
// moves the session's checkpoint; a snapshot one, from a recovery, does not. One
// without a positive timestamp moves none of that, as an alive stamped so,
// which sessionAlive ignores, moves none: it was delivered, and a snapshot one
// still puts off the deadline for its snapshot_complete.
//
// markAt is what the session's checkpoint is taken from: generatedAt, or the
// producer's now at receipt when the message is stamped ahead of it. taken is
// when the session began processing it. requestID is the recovery's request id a
// snapshot message carries, 0 for a live one.
func (m *machine) processed(id uuid.UUID, producerID int, generatedAt, markAt time.Time, taken Stamp, requestID int) {
	s, t, l := m.laneOf(id, producerID)
	if l == nil {
		return
	}
	l.lastAt, l.taken = taken.mono, true
	if !validStamp(generatedAt) {
		// not a time: as the sample it would read as an age from the epoch. A
		// snapshot message is still heard: the request id says it is on its way,
		// whatever it is stamped
		if requestID != 0 {
			m.heard(t, id, generatedAt, requestID)
		}
		return
	}
	m.heard(t, id, generatedAt, requestID)
	if requestID == 0 {
		l.checkpoint = laterOf(l.checkpoint, markAt)
		m.sample(s, l, t, generatedAt, taken)
	}
}

// sessionAlive says a session has finished an alive from its own queue:
// everything the producer sent before it is done, so a subscribed one moves the
// session's checkpoint. One without a positive timestamp is ignored, as alive
// ignores it. The checkpoint is taken from markAt, as processed takes it.
//
// held says a message of the session that was handed to the client has not
// settled yet: its delivery is not acked, and a restart from a point past it
// would skip it. The alive is heard all the same, for liveness and for the age of
// the session, but it moves no checkpoint; the next alive, or the message when it
// settles, does.
func (m *machine) sessionAlive(id uuid.UUID, producerID int, generatedAt, markAt time.Time, taken Stamp, subscribed, held bool) {
	if !validStamp(generatedAt) {
		return
	}
	s, t, l := m.laneOf(id, producerID)
	if l == nil {
		return
	}
	l.lastAt, l.taken = taken.mono, true
	m.heard(t, id, generatedAt, 0)
	if subscribed && !held {
		l.checkpoint = laterOf(l.checkpoint, markAt)
	}
	m.sample(s, l, t, generatedAt, taken)
}

// sample records how old the session's last live message or alive was: its taken
// time against its timestamp by the producer's clock. It is unknown when the offset
// is not fresh. It is also the safety net's sample: the session is above the stale
// limit while its samples are older than it, and acts once they have been for the
// stale window. The net is paused while a recovery of the producer is in flight or
// a gap is open (a snapshot ahead of a live message makes it old on every session),
// while the session's reset is pending or under way, and for what was taken before
// the last recovery of the producer ended, whenever it is handled.
func (m *machine) sample(s *session, l *lane, t *track, generatedAt time.Time, taken Stamp) {
	mono := m.mono()
	if !m.offsetFresh(t, mono) {
		l.hasAge = false
		l.above = false
		return
	}
	l.age = taken.wall.Sub(generatedAt) - t.offsetAt(taken)
	l.hasAge = true
	if m.settings.staleLimit <= 0 {
		return
	}
	if t.active != nil || m.hasGaps(t) || s.pending != nil || s.underway != 0 || taken.mono <= t.pausedUntil {
		l.above = false
		return
	}
	if l.age <= m.settings.staleLimit {
		l.above = false
		if s.lagging && !slices.ContainsFunc(s.lanes, func(other *lane) bool { return other.above }) {
			s.lagging = false
			m.log.Infof("Session %s caught up", s.id)
		}
		return
	}
	if !l.above || l.aboveSince <= t.pausedUntil {
		// a window that began before the last recovery of the producer ended
		// counts its time: it starts again
		l.above = true
		l.aboveSince = taken.mono
	} else if taken.mono-l.aboveSince >= m.settings.staleWindow {
		m.trigger(s, t, l.age, mono)
	}
}

// snapshotComplete says a session has seen a snapshot_complete; an id the
// machine has not in flight is counted, and ignored.
func (m *machine) snapshotComplete(id uuid.UUID, producerID, requestID int) {
	t := m.known(producerID)
	s := m.session(id)
	if t == nil || (s != nil && s.underway != 0) {
		// from the queue the reset replaces: what it completes is asked for again
		// once the reset is done
		return
	}
	if a := t.active; a != nil && a.requestID == requestID {
		if a.awaited[id] {
			a.seen[id] = true
			if s != nil && s.pending != nil && s.pending.producers[producerID] {
				// the session has what the recovery sent: a reset now would drop it
				m.cancelReset(s, m.mono())
			}
			if !a.accepted {
				// a snapshot_complete says the API took the request, whatever it answers later
				m.accepted(t, a, m.mono())
			}
			if t.active == a && a.allSeen() {
				m.complete(t, a, m.mono())
			}
		}
		return
	}
	if e := m.eventRecoveries[requestID]; e != nil && e.producerID == producerID {
		if e.awaited[id] {
			// a session's second snapshot_complete is no sign of life for the
			// sessions still awaited
			first := !e.seen[id]
			e.seen[id] = true
			// a snapshot_complete says the API took the request, whatever it answers later
			e.accepted = true
			switch {
			case e.allSeen():
				m.eventCompleted(e)
			case first:
				e.heardAt = m.mono()
			}
		}
		return
	}
	m.counters.unknownCompletions++
	m.log.Debugf("Snapshot complete of producer %d for request %d, which is not in flight", producerID, requestID)
}

// ---- the REST workers

// answered is a request's outcome: accepted when err is nil.
func (m *machine) answered(requestID int, err error) {
	m.answeredAt(requestID, err, m.mono())
}

// answeredAt is answered for an answer that arrived at arrived, on the monotonic clock,
// and was handled later: an alive received after the API accepted a request is judged
// against when the answer arrived, not when it was handled, however far behind the
// actor was.
func (m *machine) answeredAt(requestID int, err error, arrived time.Duration) {
	mono := m.mono()
	if e := m.eventRecoveries[requestID]; e != nil {
		m.eventAnswered(e, err, mono)
		return
	}
	for _, t := range m.tracks {
		a := t.active
		if a == nil || a.requestID != requestID {
			continue
		}
		switch {
		case a.accepted:
			// a snapshot_complete came first and said the API took it: the answer changes nothing
			m.log.Debugf("Request %d answered after a snapshot complete of it", requestID)
		case err == nil:
			m.accepted(t, a, min(arrived, mono))
		default:
			m.failed(t, a, mono, "the API did not accept it: "+err.Error())
		}
		return
	}
	m.log.Debugf("Request %d answered after it was given up", requestID)
}

// ---- recoveries

// maybeRequest asks for a recovery when the producer misses something and
// nothing holds the request back.
func (m *machine) maybeRequest(t *track, mono time.Duration) {
	if m.hasGaps(t) && m.canRequest(t, mono) {
		m.request(t, mono)
	}
}

// canRequest reports whether a recovery may go out now: none in flight, a
// session to receive it, the connection up, every channel that receives it
// bound, an alive that says the producer is there (after the one that gave the
// last recovery up), and its backoff over.
func (m *machine) canRequest(t *track, mono time.Duration) bool {
	return m.started &&
		m.connected &&
		t.active == nil &&
		m.catalog.enabled(t.id) &&
		m.receivers(t) > 0 &&
		m.aliveRecent(t, mono) &&
		!t.awaitsAlive &&
		!m.awaitsChannel(t) &&
		mono >= t.retryAt
}

func (m *machine) request(t *track, mono time.Duration) {
	now := m.clock.Now()
	gapStart := m.oldestGap(t)
	after := m.after(t, gapStart)
	requestID := m.newRequestID()
	a := &active{
		requestID:   requestID,
		after:       after,
		issuedWall:  now.wall,
		issuedMono:  mono,
		seq:         m.nextSeq(),
		requestedAt: t.producerNow(now),
		covers:      m.gapSeq,
		awaited:     m.awaited(t),
		seen:        make(map[uuid.UUID]bool),
		heardAt:     mono,
	}
	a.causes = append(a.causes, t.cause)
	t.active = a
	m.counters.requested++
	if t.failures > 0 {
		m.counters.reissued++
	}
	m.log.Infof("Recovery %d of producer %d %s", requestID, t.id, fromWhere(after))
	if after.After(gapStart) {
		m.clamped(t, requestID, gapStart, after)
	}
	m.out.request(snapshotRequest{
		producerID: t.id,
		producer:   t.producer.Name(),
		requestID:  requestID,
		after:      after,
	})
}

func fromWhere(after time.Time) string {
	if after.IsZero() {
		return "as a full snapshot"
	}
	return "from " + after.UTC().Format(time.RFC3339Nano)
}

// clamped logs a recovery asked for from after, after the producer's stateful
// recovery window, not from gapStart, where its oldest gap began: what the
// producer sent between them is not recovered. A warning the first time for that
// gap; the re-issues of the same request, with the window moving on, are debug.
func (m *machine) clamped(t *track, requestID int, gapStart, after time.Time) {
	msg := fmt.Sprintf("Recovery %d of producer %d: the gap began at %s, before the producer's stateful recovery window of %d minutes;"+
		" asking from %s, so %s of what the producer sent cannot be recovered",
		requestID, t.id, gapStart.UTC().Format(time.RFC3339Nano), t.producer.StatefulRecoveryWindowInMinutes(),
		after.UTC().Format(time.RFC3339Nano), after.Sub(gapStart))
	if !t.clampWarnedFrom.Equal(gapStart) {
		t.clampWarnedFrom = gapStart
		m.log.Warnf("%s", msg)
	} else {
		m.log.Debugf("%s", msg)
	}
}

// nextSeq numbers a request about to be sent.
func (m *machine) nextSeq() uint64 {
	m.seq++
	return m.seq
}

// maxIDDraws is how many ids newRequestID draws before it stops trusting its
// source and counts up from the last one.
const maxIDDraws = 1000

// newRequestID draws a request id that is not 0, which stands for a live message
// in processed, and not one a snapshot_complete could take for another request:
// one in flight, or one of a recovery or an event recovery that ended without
// completing and is still remembered, whose late snapshot_complete may yet
// come. It draws again; a source that keeps giving taken ids is left after
// maxIDDraws of them, and the id is the next free one above the last drawn.
func (m *machine) newRequestID() int {
	id := 0
	for range maxIDDraws {
		if id = m.nextID(); m.free(id) {
			return id
		}
	}
	for {
		id++
		if id < 1 || id > maxRequestID {
			id = 1
		}
		if m.free(id) {
			return id
		}
	}
}

// free reports whether a request can take the id: it is not 0, and no request
// in flight, waiting or remembered as ended has it.
func (m *machine) free(requestID int) bool {
	if requestID == 0 || m.inFlight(requestID) {
		return false
	}
	for _, t := range m.tracks {
		if slices.ContainsFunc(t.ended, func(e ended) bool { return e.requestID == requestID }) ||
			slices.ContainsFunc(t.endedEvents, func(e ended) bool { return e.requestID == requestID }) {
			return false
		}
	}
	return true
}

// inFlight reports whether a recovery or an event recovery of this request id has
// not ended, or an event recovery of it waits for a channel.
func (m *machine) inFlight(requestID int) bool {
	if _, ok := m.eventRecoveries[requestID]; ok {
		return true
	}
	if slices.ContainsFunc(m.deferred, func(d deferredEvent) bool { return d.requestID == requestID }) {
		return true
	}
	for _, t := range m.tracks {
		if t.active != nil && t.active.requestID == requestID {
			return true
		}
	}
	return false
}

// oldestGap is the start of the oldest gap of the producer, or of any of its
// lanes, the rewind's included. It is the zero time for none, which asks for a
// full snapshot.
func (m *machine) oldestGap(t *track) time.Time {
	var oldest time.Time
	found := false
	consider := func(g *gap) {
		if g != nil && (!found || g.from.Before(oldest)) {
			oldest, found = g.from, true
		}
	}
	consider(t.gap)
	consider(t.rewind)
	for _, s := range m.sessions {
		if l := s.lane(t.id); l != nil {
			consider(l.gap)
		}
	}
	return oldest
}

// after is where a recovery starts: the oldest open gap, clamped to the
// producer's stateful recovery window and capped at the producer's now; the zero
// time asks for a full snapshot.
func (m *machine) after(t *track, oldestGap time.Time) time.Time {
	after := oldestGap
	if start, ok := m.windowStart(t); ok && !after.IsZero() {
		after = laterOf(after, start)
	}
	return m.capped(t, after)
}

// windowStart is the earliest start a recovery of the producer may have now: the
// producer's now less its stateful recovery window, by the producer's clock, as the
// gaps' starts are. Not ok when the producer has no window.
func (m *machine) windowStart(t *track) (time.Time, bool) {
	window := time.Duration(t.producer.StatefulRecoveryWindowInMinutes()) * time.Minute
	if window <= 0 {
		return time.Time{}, false
	}
	return t.producerNow(m.clock.Now()).Add(-window), true
}

// capped is point, but no later than the producer's now (the SDK's, corrected
// by the offset of the producer's last alive): a timestamp from the producer's
// future, a component's fast clock or a bad value, moves neither a recovery nor a
// resume point past what the producer has sent. The zero time, a full snapshot,
// stays as it is.
func (m *machine) capped(t *track, point time.Time) time.Time {
	if point.IsZero() {
		return point
	}
	return earlierOf(point, t.producerNow(m.clock.Now()))
}

// accepted says the API took the request.
func (m *machine) accepted(t *track, a *active, mono time.Duration) {
	a.accepted = true
	a.acceptedAt = mono
	m.publishInfo(t, a, true)
	for _, s := range m.sessions {
		if p := s.pending; p != nil && p.unaccepted[t.id] {
			if m.cancelResetWaitingForDisabledProducer(s, mono) {
				continue
			}
			delete(p.unaccepted, t.id)
			if len(p.unaccepted) == 0 {
				if id, ok := m.producerOutsideWindow(s); ok {
					m.resetNotMadeOutsideWindow(s, id, mono)
					continue
				}
				m.reset(s, p)
			}
		}
	}
	if t.active == a && len(a.awaited) == 0 {
		// no session is left to take the snapshot_complete: nothing would ever complete it
		m.complete(t, a, mono)
	}
}

func (m *machine) publishInfo(t *track, a *active, successful bool) {
	m.out.recoveryInfo(t.id, newRecoveryInfoImpl(a.after, a.issuedWall, a.requestID, successful, m.settings.nodeID))
}

// complete says every awaited session has seen the snapshot_complete: the gaps
// it covers are closed. With gaps left that opened since, one more recovery;
// with none, the producer is up.
//
// A pending reset of another session is left as it is: a session's reset is
// cancelled only by its own snapshot_complete, the one evidence that its backlog
// was replaced. A session whose reset is under way is awaited too, and its
// snapshot_completes are ignored, so no recovery of a producer it receives
// completes while it is: the gap the reset opened stays until the recovery asked
// for after it.
func (m *machine) complete(t *track, a *active, mono time.Duration) {
	t.active = nil
	t.pausedUntil = mono
	m.out.requestEnded(a.requestID)
	if t.gap != nil && t.gap.seq <= a.covers {
		t.gap = nil
	}
	if t.rewind != nil && t.rewind.seq <= a.covers {
		t.rewind = nil
	}
	for _, s := range m.sessions {
		l := s.lane(t.id)
		if l == nil {
			continue
		}
		if l.gap != nil && l.gap.seq <= a.covers {
			l.gap = nil
			l.hasLoss = false
		}
		if a.seen[s.id] {
			// the snapshot is as of the request, and what came after was in the queue before it
			l.checkpoint = laterOf(l.checkpoint, a.requestedAt)
		}
	}
	t.failures = 0
	t.retryAt = 0
	m.counters.completed++
	if m.witnessed(a) {
		t.everRecovered = true
	}
	m.log.Infof("Recovery %d of producer %d completed in %d ms, for %v",
		a.requestID, t.id, (mono - a.issuedMono).Milliseconds(), a.causes)
	if m.hasDownGaps(t) {
		m.maybeRequest(t, mono)
		return
	}
	m.bringUp(t, causeRecoveryCompleted)
}

// witnessed reports whether a session still open saw the recovery's
// snapshot_complete. Only then did the snapshot reach a queue that is still
// there; a recovery whose every witness closed since leaves the producer as
// never recovered, so a session opened next starts from the client's seed.
func (m *machine) witnessed(a *active) bool {
	for id := range a.seen {
		if m.session(id) != nil {
			return true
		}
	}
	return false
}

// failed says the API did not accept the recovery, or it timed out: it is asked
// for again after the backoff. Once it has failed more often in a row than it is
// re-issued, the producer stays down and it is asked for at a slow, steady pace.
func (m *machine) failed(t *track, a *active, mono time.Duration, reason string) {
	m.ended(t, a, mono)
	m.publishInfo(t, a, false)
	m.counters.failed++
	for _, s := range m.sessions {
		if s.pending != nil && s.pending.unaccepted[t.id] {
			m.cancelReset(s, mono)
			m.counters.resetRequestsFailed++
			m.log.Warnf("The safety net did not reset session %s: recovery %d of producer %d failed, %s",
				s.id, a.requestID, t.id, reason)
		}
	}
	if !m.hasGaps(t) {
		// nothing is missing, so nothing is asked for again, and nothing failed in a row
		t.failures = 0
		m.upIfNothingMissing(t)
		return
	}
	t.failures++
	if t.failures > recoveryReissues {
		t.retryAt = mono + slowRetryInterval
		// the first of the streak that reaches the slow pace is a warning; the
		// rest, one per producer per interval for as long as it lasts, are debug
		msg := fmt.Sprintf("Recovery %d of producer %d failed, %s; %d in a row, so the producer stays down and it is asked for every %s",
			a.requestID, t.id, reason, t.failures, slowRetryInterval)
		if t.failures == recoveryReissues+1 {
			m.log.Warnf("%s", msg)
		} else {
			m.log.Debugf("%s", msg)
		}
		m.markDown(t, causeRecoveryFailed)
		return
	}
	t.retryAt = mono + backoff(firstReissueBackoff, t.failures)
	// the first failure of a streak is a warning; the rest, until a recovery ends it, are debug
	msg := fmt.Sprintf("Recovery %d of producer %d failed, %s; asked for again in %s",
		a.requestID, t.id, reason, t.retryAt-mono)
	if t.failures == 1 {
		m.log.Warnf("%s", msg)
	} else {
		m.log.Debugf("%s", msg)
	}
}

// backoff is the pause before the attempt-th re-issue: first, doubled for each
// next one.
func backoff(first time.Duration, attempt int) time.Duration {
	return first << min(max(attempt-1, 0), 20)
}

// giveUp says the recovery's snapshot will not come, through no fault of the
// request: lost with a queue, or by the producer. It is no longer waited for, its
// gaps stay open, and no failure is counted.
func (m *machine) giveUp(t *track, mono time.Duration, reason string) {
	a := t.active
	if a == nil {
		return
	}
	m.ended(t, a, mono)
	m.counters.abandoned++
	m.log.Infof("Recovery %d of producer %d given up: %s", a.requestID, t.id, reason)
	for _, s := range m.sessions {
		if s.pending != nil && s.pending.unaccepted[t.id] {
			m.cancelReset(s, mono)
		}
	}
}

// ended says the recovery is no longer waited for, without completing: what it
// sent may still be on its way, ahead of the snapshot of the next one asked for.
func (m *machine) ended(t *track, a *active, mono time.Duration) {
	t.active = nil
	t.pausedUntil = mono
	m.out.requestEnded(a.requestID)
	t.remember(ended{requestID: a.requestID, seq: a.seq})
}

// ---- event recoveries

var (
	// errEventRecoveryConnectionDown is why an event recovery is refused while the
	// connection is down: its snapshot would go to queues that are gone, or not bound
	// yet, and nothing would give it up. The transport tells the machine nothing
	// about the connection as a whole: the actor reports it up at the start, and what
	// a drop loses is told per session, as a lost channel. The refusal stays for a
	// transport that reports the connection as a whole.
	errEventRecoveryConnectionDown = errors.New("recovery: the connection is not up")
	// errFeedClosing is why an event recovery is refused once the feed is
	// closing, and why one in flight ends when the feed closes.
	errFeedClosing = errors.New("recovery: the feed is closing")
	// errLostQueue is why an event recovery is given up: its snapshot went with a
	// queue that was lost.
	errLostQueue = errors.New("its snapshot went with a lost queue")
)

// recoverEvent asks for one event's messages again, odds or stateful, and
// answers with the request id it will run under, or why it is refused: the feed
// is closing or closed, the producer is not known, the connection is down, or
// the producer has maxPendingEventRecoveries in flight. A producer the catalog
// has disabled is not refused: the request is the client's, whatever the
// producer's state. A request for an event and kind that is
// in flight, or waiting for a channel, is that one: it gets its id. The
// outcome comes through the outbox, whenever it is: a refusal here is no
// outcome, the caller has no recovery.
func (m *machine) recoverEvent(producerID int, eventID types.URN, stateful bool) (int, error) {
	if m.closing || m.closed {
		return 0, errFeedClosing
	}
	t := m.known(producerID)
	if t == nil {
		return 0, fmt.Errorf("recovery: unknown producer %d", producerID)
	}
	for _, e := range m.eventRecoveries {
		if e.producerID == producerID && e.eventID == eventID && e.stateful == stateful {
			return e.requestID, nil
		}
	}
	for _, d := range m.deferred {
		if d.producerID == producerID && d.eventID == eventID && d.stateful == stateful {
			return d.requestID, nil
		}
	}
	if !m.connected {
		m.counters.eventRefused++
		m.log.Debugf("Event recovery of %s not accepted: the connection is not up", eventID.ToString())
		return 0, errEventRecoveryConnectionDown
	}
	if n := m.pendingEvents(producerID); n >= maxPendingEventRecoveries {
		m.counters.eventRefused++
		m.log.Debugf("Event recovery of %s not accepted: %d in flight for producer %d", eventID.ToString(), n, producerID)
		return 0, fmt.Errorf("recovery: producer %d has %d pending event recoveries: %w",
			producerID, n, ErrTooManyPendingEventRecoveries)
	}
	requestID := m.newRequestID()
	if m.awaitsChannel(t) {
		// a session that receives it has lost its channel: its snapshot could go to
		// either queue, or to none
		m.log.Debugf("Event recovery of %s waits for a session's channel", eventID.ToString())
		m.deferred = append(m.deferred, deferredEvent{
			requestID: requestID, producerID: producerID, eventID: eventID, stateful: stateful, deferredAt: m.mono(),
		})
		return requestID, nil
	}
	m.issueEvent(t, requestID, eventID, stateful, m.mono())
	return requestID, nil
}

// pendingEvents is how many event recoveries of the producer are in flight or
// waiting for a channel.
func (m *machine) pendingEvents(producerID int) int {
	n := 0
	for _, e := range m.eventRecoveries {
		if e.producerID == producerID {
			n++
		}
	}
	for _, d := range m.deferred {
		if d.producerID == producerID {
			n++
		}
	}
	return n
}

// issueEvent sends the event recovery to a REST worker, to be awaited by the
// sessions that receive the producer now. askedMono is when it was asked for:
// the reading its maximum recovery time counts from, earlier than now for one
// that waited for a channel.
func (m *machine) issueEvent(t *track, requestID int, eventID types.URN, stateful bool, askedMono time.Duration) {
	now := m.clock.Now()
	m.eventRecoveries[requestID] = &eventActive{
		requestID:   requestID,
		producerID:  t.id,
		eventID:     eventID,
		stateful:    stateful,
		askedMono:   askedMono,
		seq:         m.nextSeq(),
		requestedAt: t.producerNow(now),
		awaited:     m.awaited(t),
		seen:        make(map[uuid.UUID]bool),
		heardAt:     now.mono,
	}
	m.counters.eventRequested++
	m.out.eventRequest(eventRequest{
		producerID: t.id,
		producer:   t.producer.Name(),
		requestID:  requestID,
		eventID:    eventID,
		stateful:   stateful,
	})
}

// releaseDeferred asks for the event recoveries that waited for a channel, once
// no session that receives their producer waits for one. With the connection
// down by then they are refused, as one asked for then is; with the feed
// closing they end as the feed's closing ends them, and are not sent; one that
// waited longer than the maximum recovery time ends as it does when it expires.
func (m *machine) releaseDeferred() {
	var waiting []deferredEvent
	mono := m.mono()
	for _, d := range m.deferred {
		t := m.byID[d.producerID]
		switch {
		case m.closing:
			m.failDeferred(d, errFeedClosing)
		case m.awaitsChannel(t):
			waiting = append(waiting, d)
		case !m.connected:
			m.counters.eventRefused++
			m.out.eventEnded(eventOutcome{
				producerID: d.producerID, requestID: d.requestID, eventID: d.eventID,
				status: types.RecoveryStatusFailed, err: errEventRecoveryConnectionDown,
			})
		case mono-d.deferredAt > m.settings.maxRecoveryTime:
			m.expireDeferred(d)
		default:
			m.issueEvent(t, d.requestID, d.eventID, d.stateful, d.deferredAt)
		}
	}
	m.deferred = waiting
}

// failDeferred reports an event recovery that waited for a channel as failed.
// The caller takes it out of m.deferred.
func (m *machine) failDeferred(d deferredEvent, err error) {
	m.out.eventEnded(eventOutcome{
		producerID: d.producerID, requestID: d.requestID, eventID: d.eventID,
		status: types.RecoveryStatusFailed, err: err,
	})
}

// expireDeferred reports an event recovery that waited for a channel longer
// than the maximum recovery time as timed out. The caller takes it out of
// m.deferred.
func (m *machine) expireDeferred(d deferredEvent) {
	m.counters.eventExpired++
	why := "waited for a session's channel longer than " + m.settings.maxRecoveryTime.String()
	if n := m.counters.eventExpired; n == 1 || n%1000 == 0 {
		m.log.Warnf("Event recovery of %s %s, %d expired so far", d.eventID.ToString(), why, n)
	} else {
		m.log.Debugf("Event recovery of %s %s", d.eventID.ToString(), why)
	}
	m.out.eventEnded(eventOutcome{
		producerID: d.producerID, requestID: d.requestID, eventID: d.eventID,
		status: types.RecoveryStatusTimedOut, err: errors.New(why),
	})
}

// eventAnswered is the API's answer to an event recovery: accepted when err is
// nil, which puts the deadline for its snapshot_complete off, or refused, which
// ends it.
func (m *machine) eventAnswered(e *eventActive, err error, mono time.Duration) {
	switch {
	case e.accepted:
		// a snapshot_complete came first and said the API took it: the answer changes nothing
		m.log.Debugf("Event recovery request %d answered after a snapshot complete of it", e.requestID)
	case err == nil:
		e.accepted = true
		e.heardAt = mono
		if len(e.awaited) == 0 {
			// no session takes snapshot_completes: nothing would ever complete it
			m.eventCompleted(e)
		}
	default:
		m.counters.eventRefused++
		if n := m.counters.eventRefused; n == 1 || n%1000 == 0 {
			m.log.Warnf("Event recovery request %d not accepted: %s, %d refused so far", e.requestID, err, n)
		} else {
			m.log.Debugf("Event recovery request %d not accepted: %s", e.requestID, err)
		}
		m.failEvent(e, types.RecoveryStatusFailed, fmt.Errorf("the API did not accept it: %w", err))
	}
}

// eventCompleted says every awaited session has seen the event recovery's
// snapshot_complete.
func (m *machine) eventCompleted(e *eventActive) {
	m.log.Infof("Event recovery %d of %s completed", e.requestID, e.eventID.ToString())
	m.endEvent(e, types.RecoveryStatusCompleted, nil)
}

// failEvent ends the event recovery without completing: what it sent may still
// be ahead of a recovery asked for later.
func (m *machine) failEvent(e *eventActive, status types.RecoveryRequestStatus, err error) {
	if t := m.byID[e.producerID]; t != nil {
		t.rememberEvent(ended{requestID: e.requestID, seq: e.seq})
	}
	m.endEvent(e, status, err)
}

// endEvent takes the event recovery out of flight and reports how it ended.
func (m *machine) endEvent(e *eventActive, status types.RecoveryRequestStatus, err error) {
	delete(m.eventRecoveries, e.requestID)
	m.out.eventEnded(eventOutcome{
		producerID: e.producerID, requestID: e.requestID, eventID: e.eventID, status: status, err: err,
	})
}

// giveUpEvents gives up the event recoveries whose snapshot_complete went with a
// lost queue: every one for a session nil, the connection's, else those still
// waiting for that session's. A queue lost is all that gives one up: the producer
// going down for any other cause, late or silent or unsubscribed, leaves them.
func (m *machine) giveUpEvents(lost *session) {
	for _, e := range m.sortedEvents() {
		if lost != nil && (!e.awaited[lost.id] || e.seen[lost.id]) {
			continue
		}
		m.counters.eventAbandoned++
		m.log.Debugf("Event recovery %d of %s given up: its snapshot went with a lost queue", e.requestID, e.eventID.ToString())
		m.failEvent(e, types.RecoveryStatusFailed, errLostQueue)
	}
}

// sortedEvents are the event recoveries in flight, the earliest asked for first,
// so what is reported of several does not depend on the map's order. The slice
// is the caller's to end them while it walks it.
func (m *machine) sortedEvents() []*eventActive {
	out := make([]*eventActive, 0, len(m.eventRecoveries))
	for _, e := range m.eventRecoveries {
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b *eventActive) int {
		return cmp.Compare(a.seq, b.seq)
	})
	return out
}

// ---- gaps and checkpoints

// openGap opens a gap from from, joined with the one open: the earlier start,
// and a new number, since what is missing now is missing after any recovery
// already asked for. From the zero time, with an initial snapshot interval set,
// the gap is fixed at now less the interval, so asking again does not move it.
// pendingReset marks a gap the safety net opens for a reset it has not made yet; a
// gap joined with another that is not the net's is not the net's.
func (m *machine) openGap(open *gap, from time.Time, pendingReset bool) *gap {
	if from.IsZero() && m.settings.initialSnapshot > 0 {
		from = m.clock.Now().wall.Add(-m.settings.initialSnapshot)
	}
	m.gapSeq++
	if open == nil {
		return &gap{from: from, seq: m.gapSeq, pendingReset: pendingReset}
	}
	open.from = earlierOf(open.from, from)
	open.seq = m.gapSeq
	open.pendingReset = open.pendingReset && pendingReset
	return open
}

// openProducerGap says the producer stopped sending: every session misses what
// came after its last subscribed alive, or after fallback when there was none.
// Without either, nothing was received to miss.
func (m *machine) openProducerGap(t *track, fallback time.Time) {
	from := t.safePoint
	if from.IsZero() {
		from = fallback
	}
	if !from.IsZero() {
		t.gap = m.openGap(t.gap, from, false)
	}
}

// resumePoint is where a recovery of the producer would have to start now for no
// session to miss anything: the oldest of the open gaps' starts (the producer's,
// the rewind's and the sessions') and of the checkpoints of the sessions that
// receive it, since a session with no gap still has what it has not processed
// only in its queue. It is unclamped, so a client that resumes from it misses
// nothing, and not ok when no session receives the producer.
func (m *machine) resumePoint(t *track) (time.Time, bool) {
	var point time.Time
	found := false
	consider := func(at time.Time) {
		if !found || at.Before(point) {
			point, found = at, true
		}
	}
	if t.gap != nil {
		consider(t.gap.from)
	}
	if t.rewind != nil {
		consider(t.rewind.from)
	}
	received := false
	for _, s := range m.sessions {
		if l := s.lane(t.id); l != nil {
			received = true
			consider(l.checkpoint)
			if l.gap != nil {
				consider(l.gap.from)
			}
		}
	}
	return point, received
}

// publishResumePoints publishes each producer's resume point that moved: the
// actor calls it after every fact. Nothing before the start, when the client's
// own start stands; a producer no session receives keeps what it had; once the
// feed is closing, only a point that goes back (and a gap of the producer's
// opened after its last session closed still takes it back). Each is capped at
// the producer's now, so a client that persists it never resumes from the
// producer's future.
func (m *machine) publishResumePoints() {
	if !m.started {
		return
	}
	for _, t := range m.tracks {
		point, ok := m.resumePoint(t)
		if !ok && m.closing && t.gap != nil && t.published != nil {
			// every session closed at shutdown, before what the producer's gap says
			// they missed: the point the client keeps goes back to its start all the
			// same; one never published stays so
			point, ok = earlierOf(*t.published, t.gap.from), true
		}
		if !ok {
			continue
		}
		point = m.capped(t, point)
		moved := t.published == nil || !point.Equal(*t.published)
		goesBack := !m.closing || t.published == nil || point.Before(*t.published)
		if moved && goesBack {
			t.published = &point
			m.out.resumePoint(t.id, point)
		}
	}
}

// seed is the client's recovery start, or the zero time for a full snapshot:
// what the client set before the feed opened, or since while the producer had not
// recovered.
func (m *machine) seed(t *track) time.Time {
	if t.seeded != nil {
		return *t.seeded
	}
	return m.catalog.recoveryFrom(t.id)
}

// hasGaps reports whether the producer or any of its lanes has a gap open. The
// rewind is not one: it triggers no request.
func (m *machine) hasGaps(t *track) bool {
	if t.gap != nil {
		return true
	}
	for _, s := range m.sessions {
		if l := s.lane(t.id); l != nil && l.gap != nil {
			return true
		}
	}
	return false
}

// hasDownGaps reports whether the producer has a gap that keeps it down: every gap
// but those of a reset the safety net has not made yet.
func (m *machine) hasDownGaps(t *track) bool {
	if t.gap != nil {
		return true
	}
	for _, s := range m.sessions {
		if l := s.lane(t.id); l != nil && l.gap != nil && !l.gap.pendingReset {
			return true
		}
	}
	return false
}

// ---- the safety net

// trigger says the session has been too far behind the producer for the window: a
// recovery for each of the producers it receives first, the reset once the API has
// accepted them all. With the net's resets spent the session is lagging instead.
//
// It waits for a recovery of any of the session's producers that is in flight. A
// producer that cannot be asked now (silent, in backoff, or waiting for its slow
// retry) sends the session nothing to drop, and once the reset is done it misses
// what the queue held like any other, to be asked for when it can be. Not once the
// feed is closing: a reset then drops what the shutdown is about to settle.
func (m *machine) trigger(s *session, t *track, age time.Duration, mono time.Duration) {
	m.pruneResets(s, mono)
	if len(s.resets) >= safetyNetResets {
		if !s.lagging {
			s.lagging = true
			m.counters.lagging++
			m.log.Warnf("Session %s is %s behind producer %d, with the safety net's %d resets spent: it lags",
				s.id, age, t.id, safetyNetResets)
		}
		return
	}
	if mono < s.nextResetAt || m.closing || m.closed {
		return
	}
	if s.underway != 0 || !m.canRequest(t, mono) {
		return
	}
	for _, l := range s.lanes {
		if other := m.byID[l.producer]; m.catalog.enabled(other.id) && other.active != nil {
			return
		}
	}
	if id, outside := m.producerOutsideWindow(s); outside {
		if !s.lagging {
			s.lagging = true
			m.counters.lagging++
			m.log.Warnf("Session %s is %s behind producer %d and its backlog of producer %d reaches back past the producer's recovery window of %d minutes:"+
				" the safety net does not reset it, as a recovery could not repeat what the reset would drop; it lags",
				s.id, age, t.id, id, m.byID[id].producer.StatefulRecoveryWindowInMinutes())
		}
		return
	}
	unaccepted := make(map[int]bool)
	var asked []*track
	for _, l := range s.lanes {
		other := m.byID[l.producer]
		if m.catalog.enabled(other.id) && m.canRequest(other, mono) {
			l.gap = m.openGap(l.gap, l.checkpoint, true)
			unaccepted[other.id] = true
			asked = append(asked, other)
		}
	}
	if len(unaccepted) == 0 {
		return
	}
	m.resetSeq++
	s.pending = &pendingReset{
		unaccepted: unaccepted,
		producers:  maps.Clone(unaccepted),
		producerID: t.id,
		age:        age,
		number:     m.resetSeq,
	}
	m.log.Warnf("Session %s is %s behind producer %d: the safety net asks for recoveries before it resets", s.id, age, t.id)
	for _, other := range asked {
		m.maybeRequest(other, mono)
	}
	// a producer disabled since the selection got no request, and none will answer for it
	for _, other := range asked {
		if other.active == nil {
			m.resetNotMadeForDisabledProducer(s, other.id, mono)
			return
		}
	}
}

// cancelResetsWaitingForDisabledProducers gives up the resets that wait for a
// recovery of a producer that has since been disabled: it would wait for ever.
func (m *machine) cancelResetsWaitingForDisabledProducers(mono time.Duration) {
	for _, s := range m.sessions {
		m.cancelResetWaitingForDisabledProducer(s, mono)
	}
}

// cancelResetWaitingForDisabledProducer gives up the session's pending reset when
// a recovery it waits for is of a producer that has been disabled; whether it did.
func (m *machine) cancelResetWaitingForDisabledProducer(s *session, mono time.Duration) bool {
	if s.pending == nil {
		return false
	}
	for _, l := range s.lanes {
		if s.pending.unaccepted[l.producer] && !m.catalog.enabled(l.producer) {
			m.resetNotMadeForDisabledProducer(s, l.producer, mono)
			return true
		}
	}
	return false
}

// windowMargin is how far inside the producer's recovery window a checkpoint must
// lie for a reset: resetWindowMargin, at most half the window.
func windowMargin(t *track) time.Duration {
	window := time.Duration(t.producer.StatefulRecoveryWindowInMinutes()) * time.Minute
	return min(resetWindowMargin, window/2)
}

// producerOutsideWindow finds, among the producers the session receives that the
// catalog has enabled, one whose checkpoint for the session lies before where a
// recovery of it would start now, clamped to its stateful recovery window, plus the
// margin (windowMargin): the window moves on before the recovery asked into the new
// queue, and a checkpoint at its edge would be outside it by then. A reset
// replaces the whole queue, so it drops the backlog of a producer that cannot be
// asked for now (silent, in backoff) as well, and that producer's later recovery
// starts from the checkpoint, clamped just the same: what lies before the window is
// out of reach. A lane with no checkpoint is asked for from the start, which is not
// clamped.
func (m *machine) producerOutsideWindow(s *session) (int, bool) {
	for _, l := range s.lanes {
		if !m.catalog.enabled(l.producer) || l.checkpoint.IsZero() {
			continue
		}
		if start, ok := m.windowStart(m.byID[l.producer]); ok && l.checkpoint.Before(start.Add(windowMargin(m.byID[l.producer]))) {
			return l.producer, true
		}
	}
	return 0, false
}

// resetNotMadeOutsideWindow cancels a reset whose recoveries were accepted, but
// whose session's backlog of the producer has meanwhile reached past the window.
func (m *machine) resetNotMadeOutsideWindow(s *session, producerID int, mono time.Duration) {
	m.cancelReset(s, mono)
	m.counters.resetRequestsFailed++
	m.log.Warnf("The safety net did not reset session %s: its backlog of producer %d reaches back past the producer's recovery window of %d minutes,"+
		" where a recovery could not repeat what the reset would drop",
		s.id, producerID, m.byID[producerID].producer.StatefulRecoveryWindowInMinutes())
}

func (m *machine) resetNotMadeForDisabledProducer(s *session, producerID int, mono time.Duration) {
	m.cancelReset(s, mono)
	m.counters.resetRequestsFailed++
	m.log.Warnf("The safety net did not reset session %s: producer %d was disabled before its recovery was accepted",
		s.id, producerID)
}

// reset says the API has accepted every recovery the reset waited for: the session's
// channel is replaced, and its producers are down for it. The reset stays under way
// until the transport reports it done, and meanwhile the session's
// snapshot_completes are ignored: they come from the queue being replaced. Its
// producers' recoveries are in flight all the while, so nothing more is asked for
// them.
func (m *machine) reset(s *session, p *pendingReset) {
	s.pending = nil
	s.underway = p.number
	s.underwayProducer = p.producerID
	s.underwayAge = p.age
	for _, l := range s.lanes {
		// what the reset drops starts here, before any sample of the new channel
		// moves the checkpoint; the gap, with its earlier start kept, is a new one,
		// so a recovery asked before the reset (one of a producer the net did not ask
		// for, say) does not cover it and cannot bring the producer up while the
		// queue is replaced
		m.loseLane(l)
	}
	m.log.Warnf("The safety net resets session %s, %s behind producer %d", s.id, p.age, p.producerID)
	m.out.reset(s.id, p.number)
	for _, l := range s.lanes {
		m.markDown(m.byID[l.producer], causeSafetyNetReset)
	}
}

// cancelReset says a reset will not be made: the gaps it opened close, and the net
// waits before it tries again, as long as after the resets already made.
func (m *machine) cancelReset(s *session, mono time.Duration) {
	s.pending = nil
	m.pruneResets(s, mono)
	s.nextResetAt = mono + backoff(firstResetBackoff, len(s.resets)+1)
	for _, l := range s.lanes {
		if l.gap != nil && l.gap.pendingReset {
			l.gap = nil
		}
	}
}

// underwayFor reports whether a session that receives the producer has a reset with
// the transport.
func (m *machine) underwayFor(t *track) bool {
	for _, s := range m.sessions {
		if s.underway != 0 && s.lane(t.id) != nil {
			return true
		}
	}
	return false
}

// ---- producer status

// upIfNothingMissing brings a producer back that is down only for gaps that went
// away with a closed session, or for a lost connection that no session it has now
// missed anything of. Not once the feed is closing: a request refused or a
// connection back then recovered nothing.
func (m *machine) upIfNothingMissing(t *track) {
	if t.down &&
		m.connected &&
		!m.closing &&
		t.everRecovered &&
		t.active == nil &&
		t.cause != causeProcessingDelay &&
		!m.hasDownGaps(t) {
		// nothing is missing: what failed before does not count against what comes next
		t.rearm()
		m.bringUp(t, causeRecoveryCompleted)
	}
}

// bringUp says nothing is missing any more: the producer is up, unless a session
// processes it late, which keeps it down for that until the session catches up.
func (m *machine) bringUp(t *track, cause statusCause) {
	if t.delayed {
		m.markDown(t, causeProcessingDelay)
	} else {
		m.markUp(t, cause)
	}
}

// markDown puts the producer down for cause: reported at the cause's level when
// the producer was up or down for another cause, and as a public change only when
// it was up or down for another public reason.
func (m *machine) markDown(t *track, cause statusCause) {
	if !m.catalog.enabled(t.id) || (t.down && t.cause == cause) {
		return
	}
	if a := t.active; a != nil {
		a.causes = append(a.causes, cause)
	}
	public := !t.down || t.cause.reason() != cause.reason()
	t.down = true
	t.cause = cause
	m.log.Infof("Producer %d down: %s", t.id, cause)
	m.tell(t, cause, public)
}

// markUp puts the producer up for cause; the first time since the feed opened,
// whatever brought it up, it is up for its first recovery, which the client
// hears of once per producer.
func (m *machine) markUp(t *track, given statusCause) {
	if !m.catalog.enabled(t.id) || !t.down {
		return
	}
	cause := given
	if !t.reportedUp {
		cause = causeFirstRecoveryCompleted
	}
	t.reportedUp = true
	t.down = false
	t.cause = cause
	m.log.Infof("Producer %d up: %s", t.id, cause)
	m.tell(t, cause, true)
}

// tell reports a status change: the cause-level change always, flagged public
// when the down flag or the public reason changed.
func (m *machine) tell(t *track, cause statusCause, public bool) {
	t.told, t.toldDelayed = true, t.delayed
	m.out.status(statusChange{
		producerID: t.id,
		down:       t.down,
		delayed:    t.delayed,
		cause:      cause,
		at:         m.clock.Now().wall,
		public:     public,
	})
}

// ---- small things

// awaitsChannel reports whether a session that receives the producer has no
// bound queue to send to yet: its reset is with the transport, or its lost
// channel has not been opened again.
func (m *machine) awaitsChannel(t *track) bool {
	for _, s := range m.sessions {
		if (s.underway != 0 || s.lost) && s.lane(t.id) != nil {
			return true
		}
	}
	return false
}

func (m *machine) aliveRecent(t *track, mono time.Duration) bool {
	return t.hasAlive && !t.inactive && mono-t.lastAliveAt <= m.settings.maxInactivity
}

// offsetFresh reports whether the offset is recent enough to correct ages with:
// an alive within two intervals.
func (m *machine) offsetFresh(t *track, mono time.Duration) bool {
	return t.hasAlive && mono-t.lastAliveAt <= 2*t.aliveInterval
}

// receivers is how many sessions receive the producer.
func (m *machine) receivers(t *track) int {
	n := 0
	for _, s := range m.sessions {
		if s.lane(t.id) != nil {
			n++
		}
	}
	return n
}

// awaited are the sessions a recovery of the producer waits for: every session
// that receives it. Every session with a lane takes snapshot_complete.
func (m *machine) awaited(t *track) map[uuid.UUID]bool {
	awaited := make(map[uuid.UUID]bool)
	for _, s := range m.sessions {
		if s.lane(t.id) != nil {
			awaited[s.id] = true
		}
	}
	return awaited
}

// known is the track of a producer; one the machine has not seen before is
// added from the catalog, and one the catalog does not have is counted and
// ignored.
func (m *machine) known(producerID int) *track {
	if t := m.byID[producerID]; t != nil {
		return t
	}
	if producer, ok := m.catalog.producer(producerID); ok {
		return m.addTrack(producer)
	}
	m.counters.unknownProducers++
	// the first, then one in a thousand: a feed of such a producer would flood the log
	if n := m.counters.unknownProducers; n == 1 || n%1000 == 0 {
		m.log.Warnf("A fact about producer %d, which the producer list does not have, is ignored, %d so far", producerID, n)
	}
	return nil
}

// addTrack starts tracking a producer, and gives every session that receives it
// a lane.
func (m *machine) addTrack(producer types.Producer) *track {
	t := &track{
		id:            producer.ID(),
		producer:      producer,
		down:          true,
		cause:         causeStarting,
		aliveInterval: expectedAliveInterval,
		createdAt:     m.mono(),
	}
	m.tracks = append(m.tracks, t)
	m.byID[t.id] = t
	for _, s := range m.sessions {
		if hasLane(s.interest) && s.interest.IsProducerInScope(producer) {
			// a session already open has its queue bound: the lane starts now
			m.addLane(s, t, m.clock.Now())
		}
	}
	return t
}

// laneOf is the session, the track and the session's lane of a producer, nil for
// any that does not exist.
func (m *machine) laneOf(id uuid.UUID, producerID int) (*session, *track, *lane) {
	t := m.known(producerID)
	s := m.session(id)
	if t == nil || s == nil {
		return nil, nil, nil
	}
	return s, t, s.lane(t.id)
}

func (m *machine) session(id uuid.UUID) *session {
	for _, s := range m.sessions {
		if s.id == id {
			return s
		}
	}
	return nil
}

func (m *machine) removeSession(id uuid.UUID) *session {
	for i, s := range m.sessions {
		if s.id == id {
			m.sessions = append(m.sessions[:i], m.sessions[i+1:]...)
			return s
		}
	}
	return nil
}

// mono is the reading of the monotonic clock: for every duration between two
// local events, never published.
func (m *machine) mono() time.Duration { return m.clock.Now().mono }

// validStamp reports whether a timestamp is a time at all: a message or alive
// stamped with none, or with a time before the epoch, moves nothing.
func validStamp(at time.Time) bool { return at.UnixMilli() > 0 }

// futureSkew is how far ahead of the SDK's wall clock a timestamp may be and
// still count as the producer's clock being a little fast.
const futureSkew = time.Minute

// aliveMark is the mark of an alive generated at generatedAt and received at
// received: what the safe point and a checkpoint are taken from. It is
// generatedAt, unless that is further ahead of the SDK's clock than futureSkew
// (a bad value, or a producer far off): then it is the SDK's reading, so one
// such alive cannot move a point past what the producer has sent.
func aliveMark(generatedAt time.Time, received Stamp) time.Time {
	if generatedAt.After(received.wall.Add(futureSkew)) {
		return received.wall
	}
	return generatedAt
}

// markOf is the mark of a message of the producer generated at generatedAt and
// taken by a session at the reading at: generatedAt, but no later than the
// producer's now at that reading, by its last alive. The checkpoint is a running
// maximum, so a timestamp ahead of the producer's clock would hold it ahead of
// everything the producer sends for good, and a loss would start its gap there.
// Before the producer's first alive it is aliveMark's bound.
func (m *machine) markOf(producerID int, generatedAt time.Time, at Stamp) time.Time {
	t := m.byID[producerID]
	if t == nil || !t.hasAlive {
		return aliveMark(generatedAt, at)
	}
	return earlierOf(generatedAt, t.producerNow(at))
}

func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func earlierOf(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// ---- the state

// track is one producer, as the machine tracks it.
type track struct {
	id       int
	producer types.Producer
	// createdAt is when the machine learned of the producer, on the monotonic
	// clock: its inactivity counts from then, or from the start if that is later.
	createdAt time.Duration

	down          bool
	cause         statusCause
	delayed       bool
	everRecovered bool
	// reportedUp says it has been reported up since the feed opened.
	reportedUp bool

	// hasAlive says an alive has arrived; lastAliveAt is when, on the monotonic clock.
	hasAlive    bool
	lastAliveAt time.Duration
	// lastAliveMark is the last alive's mark: where a gap starts for a producer
	// that has no safe point.
	lastAliveMark time.Time
	// safePoint is the last subscribed alive's mark: the producer has sent
	// everything before it.
	safePoint time.Time
	// offset is the producer's clock against the SDK's, measured on the last alive.
	offset clockOffset
	// aliveInterval is how often the producer sends an alive, a moving average.
	aliveInterval time.Duration
	// inactive says the producer went too long without an alive, and no alive has
	// come since.
	inactive bool
	// awaitsAlive says an unsubscribed alive gave the last recovery up, and no
	// alive has come since.
	awaitsAlive bool

	// gap is what every session misses because the producer stopped sending; nil
	// for nothing.
	gap *gap
	// rewind is the client's rewind of the recovery timestamp: a gap that
	// triggers no request.
	rewind *gap
	// told says a status of the producer has been told; toldDelayed is the delayed
	// flag it carried.
	told        bool
	toldDelayed bool
	// clampWarnedFrom is the start of the gap whose recovery was last clamped to
	// the stateful window and warned of.
	clampWarnedFrom time.Time
	// seeded is the start the client set after the feed opened, which replaced the
	// seed of a producer that had not recovered; nil for none.
	seeded *time.Time
	// published is the resume point last published, nil for none.
	published *time.Time

	active *active
	// ended are the recoveries that failed or were given up, newest first, at
	// most endedRemembered: what they sent may still be ahead of the one in flight.
	ended []ended
	// endedEvents are the event recoveries that failed, expired or were given
	// up, newest first, at most endedRemembered: what they sent may still be
	// ahead of a recovery or an event recovery asked for later.
	endedEvents []ended

	// failures is how many recoveries failed in a row; retryAt is the monotonic
	// time before which the next is not asked for.
	failures int
	retryAt  time.Duration
	// pausedUntil is the monotonic time up to which the samples taken are not the
	// safety net's: a recovery of the producer was in flight until then. Zero for
	// none; the monotonic clock never reads zero.
	pausedUntil time.Duration
}

// rearm forgets the failures in a row and any backoff: the next recovery goes out
// at once.
func (t *track) rearm() {
	t.failures = 0
	t.retryAt = 0
}

// remember keeps the recovery among the last that ended without completing.
func (t *track) remember(e ended) {
	t.ended = remembered(t.ended, e)
}

// rememberEvent keeps the event recovery among the last that ended without
// completing.
func (t *track) rememberEvent(e ended) {
	t.endedEvents = remembered(t.endedEvents, e)
}

// remembered is the list with e first, and no more than endedRemembered of them.
func remembered(list []ended, e ended) []ended {
	list = append([]ended{e}, list...)
	if len(list) > endedRemembered {
		list = list[:endedRemembered]
	}
	return list
}

// producerNow is the producer's clock at a reading: the SDK's wall clock less
// the offset as of that reading. Before the first alive there is no measure to
// correct, and the producer's clock reads as the SDK's.
func (t *track) producerNow(at Stamp) time.Time {
	return at.wall.Add(-t.offsetAt(at))
}

// offsetAt is the offset of the producer's clock as of a reading.
func (t *track) offsetAt(at Stamp) time.Duration {
	if !t.hasAlive {
		return 0
	}
	return t.offset.at(at)
}

// session is one session of the client, as the machine tracks it.
type session struct {
	id       uuid.UUID
	interest types.MessageInterest
	// lanes are the producers it receives.
	lanes []*lane
	// lost says its channel was lost, or has not been bound yet, and no queue
	// is bound.
	lost bool
	// processing says a message of the client is in processing in the session,
	// since processingSince on the monotonic clock.
	processing      bool
	processingSince time.Duration

	// pending is the reset the safety net waits for the API to make: it asked for
	// recoveries first. underway is the number of the reset the transport is
	// making, 0 for none, with the producer whose messages were too old and how
	// old they were.
	pending          *pendingReset
	underway         uint64
	underwayProducer int
	underwayAge      time.Duration
	// resets are when the net reset the session, within the cool-down, oldest
	// first, on the monotonic clock; nextResetAt is when it may reset it next.
	resets      []time.Duration
	nextResetAt time.Duration
	// lagging says the session is too far behind and the net's resets are spent.
	lagging bool
}

func (s *session) lane(producerID int) *lane {
	for _, l := range s.lanes {
		if l.producer == producerID {
			return l
		}
	}
	return nil
}

// lane is one session's view of one producer.
type lane struct {
	producer int
	// checkpoint is the running maximum of what it finished, zero for nothing.
	checkpoint time.Time
	gap        *gap
	// hasLoss says a loss of the session's queue opened the lane's gap, and lossFrom is
	// where the earliest began: the checkpoint it had then.
	hasLoss  bool
	lossFrom time.Time
	// age is the last sample's age, when hasAge.
	age    time.Duration
	hasAge bool
	// lastAt is when the session last took a message or an alive of the producer, on
	// the monotonic clock, when taken.
	lastAt time.Duration
	taken  bool
	// above says the samples have been over the stale limit since aboveSince, on
	// the monotonic clock.
	above      bool
	aboveSince time.Duration
}

// gap is what a session misses of a producer, from from; the zero time for
// everything.
type gap struct {
	from time.Time
	// seq is the gap's number: a recovery covers the gaps numbered up to the last
	// at its start.
	seq uint64
	// pendingReset says the safety net opened it for a reset it has not made yet.
	pendingReset bool
}

// active is a producer's recovery in flight.
type active struct {
	requestID int
	// after is where it starts, the zero time for a full snapshot.
	after time.Time
	// issuedWall is when it was asked for by the SDK's wall clock: what the
	// recovery info publishes; issuedMono, on the monotonic clock: what its
	// deadlines count from.
	issuedWall time.Time
	issuedMono time.Duration
	// seq is its sequence number: what orders it among the requests sent.
	seq uint64
	// requestedAt is when it was asked for, by the producer's clock.
	requestedAt time.Time
	// covers is the number of the last gap it covers.
	covers uint64
	// awaited are the sessions whose snapshot_complete it waits for.
	awaited map[uuid.UUID]bool
	seen    map[uuid.UUID]bool
	// causes are why it was asked for, and what joined it since.
	causes []statusCause

	accepted bool
	// acceptedAt is when the API accepted it, or a snapshot_complete said so, on
	// the monotonic clock.
	acceptedAt time.Duration
	// heardAt is when a session that awaits it last took something that says it
	// is on its way, or when it was asked for: its snapshot_complete is waited
	// for this long after, at most.
	heardAt time.Duration
}

// allSeen reports whether every awaited session has seen the snapshot_complete.
func (a *active) allSeen() bool { return allSeen(a.awaited, a.seen) }

// allSeen reports whether every awaited session is among those that have seen it.
func allSeen(awaited, seen map[uuid.UUID]bool) bool {
	for id := range awaited {
		if !seen[id] {
			return false
		}
	}
	return true
}

// ended is a recovery that ended without completing: its id, and its sequence
// number.
type ended struct {
	requestID int
	seq       uint64
}

// eventActive is an event recovery in flight.
type eventActive struct {
	requestID  int
	producerID int
	eventID    types.URN
	// stateful says it asks for the event's stateful messages, not only its odds.
	stateful bool
	// askedMono is when it was asked for, on the monotonic clock: what its maximum
	// recovery time counts from, also for one that waited for a channel before it
	// was sent. seq is its sequence number: what orders it among the requests
	// sent. requestedAt is when it was sent, by the producer's clock.
	askedMono   time.Duration
	seq         uint64
	requestedAt time.Time
	// awaited are the sessions whose snapshot_complete it waits for.
	awaited map[uuid.UUID]bool
	seen    map[uuid.UUID]bool
	// accepted says the API took it, or a snapshot_complete said so.
	accepted bool
	// heardAt is when the API accepted it, or a session that awaits it last took
	// something of it or queued ahead of it, or its first snapshot_complete: its
	// snapshot_complete is waited for this long after, at most, once accepted.
	heardAt time.Duration
}

// allSeen reports whether every awaited session has seen the snapshot_complete.
func (e *eventActive) allSeen() bool { return allSeen(e.awaited, e.seen) }

// pendingReset is a reset the safety net waits for the API to make: once made it
// is the session's underway. unaccepted are the producers whose recoveries the API
// has not accepted yet; producers all it asked for.
type pendingReset struct {
	unaccepted map[int]bool
	producers  map[int]bool
	// producerID is the producer whose messages were too old, and age how old.
	producerID int
	age        time.Duration
	number     uint64
}

// deferredEvent is an event recovery waiting for a session's channel.
type deferredEvent struct {
	requestID  int
	producerID int
	eventID    types.URN
	stateful   bool
	// deferredAt is when it was asked for, on the monotonic clock.
	deferredAt time.Duration
}
