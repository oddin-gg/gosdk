package recovery

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

// machine is every producer's recovery state and the rules that change it: the
// state machine the recovery actor will run. Each method is one fact, handled
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
// restarts resumes to miss nothing, published through publishResumePoints.
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
	nextID   func() int
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
	// startedAt is when the feed opened, on the monotonic clock.
	startedAt time.Duration
	// connected says the transport has reported its connection up with every
	// session's channel bound: not before its first up, so the first recovery
	// cannot go out to queues that do not exist yet.
	connected bool
	// gapSeq is the number of the last gap opened; a recovery covers the gaps
	// up to the number at its start.
	gapSeq uint64
}

// machineCounters count what happened, for tests and logs.
type machineCounters struct {
	requested          int
	reissued           int
	completed          int
	failed             int
	abandoned          int
	unknownCompletions int
	unknownProducers   int
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
	}
	for _, producer := range catalog.producers() {
		m.addTrack(producer)
	}
	return m
}

// ---- the lifecycle

// sessionOpened registers a session, before or after start. A session whose
// interest has a lane gets one for every producer it covers.
func (m *machine) sessionOpened(id uuid.UUID, interest types.MessageInterest) {
	if m.session(id) != nil {
		m.log.Warnf("Session %s opened twice; the second is ignored", id)
		return
	}
	s := &session{id: id, interest: interest}
	m.sessions = append(m.sessions, s)
	if !hasLane(interest) {
		// a client's alive-only session receives no producer's messages: no
		// lane, so its channel's loss and its pace take no producer down, and
		// no recovery waits for its snapshot_complete
		return
	}
	for _, t := range m.tracks {
		if interest.IsProducerInScope(t.producer) {
			m.addLane(s, t)
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
// recovery could bring.
func (m *machine) addLane(s *session, t *track) {
	l := &lane{producer: t.id}
	s.lanes = append(s.lanes, l)
	if !m.started {
		return
	}
	if !t.everRecovered {
		l.gap = m.openGap(nil, m.seed(t))
		l.checkpoint = l.gap.from
		return
	}
	l.checkpoint = t.producerNow(m.clock.Now())
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
	// S4 seam: the event recoveries the session was awaited by leave here.
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
			l.gap = m.openGap(nil, m.seed(m.byID[l.producer]))
			// the start it got (an initial interval counted back from now, say)
			// is where a later loss starts too, while the session has processed
			// nothing to move it
			l.checkpoint = l.gap.from
		}
	}
}

// beginClosing says the feed has begun to close: from now on the published
// resume points only go back. A session that closes takes its checkpoint out of
// the point; at shutdown that would move the point past its unprocessed
// messages, and the point the client persists would depend on the order the
// sessions close in. A gap opened meanwhile still takes the point back.
func (m *machine) beginClosing() {
	m.closing = true
}

// rewind lowers the point the next recovery starts from, for a client that sets
// the producer's recovery timestamp after the feed has started. It opens no
// request by itself: it is a gap that does not trigger one, folded into the
// start of the next request and into the published resume point, and closed
// only when a recovery that covers it completes, so a failed request keeps it.
// A point later than the one the producer is at is ignored.
//
// Before the start the client's value is the seed, read at start.
func (m *machine) rewind(producerID int, to time.Time) {
	if !m.started {
		return
	}
	t := m.known(producerID)
	if t == nil {
		return
	}
	if current := m.currentPoint(t); to.After(current) {
		m.log.Warnf("The recovery timestamp of producer %d, %s, is later than the point it is at, %s: ignored",
			t.id, to.UTC().Format(time.RFC3339Nano), current.UTC().Format(time.RFC3339Nano))
		return
	}
	t.rewind = m.openGap(t.rewind, to)
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
		for _, l := range s.lanes {
			l.gap = m.openGap(l.gap, l.checkpoint)
		}
	}
	// S4 seam: the event recoveries in flight are given up here.
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
	// S4 seam: the event recoveries that waited for the channel are released here.
}

// queueLost says the session's queue lost what it held: it misses everything
// after its checkpoints, and a recovery that waited for its snapshot_complete
// will not see it.
func (m *machine) queueLost(s *session, cause statusCause, mono time.Duration) {
	for _, l := range s.lanes {
		l.gap = m.openGap(l.gap, l.checkpoint)
	}
	for _, l := range s.lanes {
		m.markDown(m.byID[l.producer], cause)
	}
	m.notSeen(s, mono)
}

// notSeen says the session will not see the snapshot_completes it has not seen
// yet: the recoveries waiting for them are given up, and asked for again as far
// as anything is missing.
func (m *machine) notSeen(s *session, mono time.Duration) {
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
	if subscribed && t.down && m.started && m.connected && m.receivers(t) == 0 {
		// no session has a lane of this producer, so there is nothing to
		// recover: a fresh alive is all it takes to be up
		m.bringUp(t, causeRecoveryCompleted)
	}
}

// processed says a session has finished a message of the producer. A live one
// moves the session's checkpoint; a snapshot one, from a recovery, does not. One
// without a positive timestamp moves none of that, as an alive stamped so,
// which sessionAlive ignores, moves none.
//
// markAt is what the session's checkpoint is taken from: generatedAt, or the
// producer's now at receipt when the message is stamped ahead of it. taken is
// when the session began processing it. requestID is the recovery's request id a
// snapshot message carries, 0 for a live one.
func (m *machine) processed(id uuid.UUID, producerID int, generatedAt, markAt time.Time, taken Stamp, requestID int) {
	t, l := m.laneOf(id, producerID)
	if l == nil {
		return
	}
	l.lastAt, l.taken = taken.mono, true
	if !validStamp(generatedAt) {
		// not a time: as the sample it would read as an age from the epoch
		return
	}
	if requestID == 0 {
		l.checkpoint = laterOf(l.checkpoint, markAt)
		m.sample(l, t, generatedAt, taken)
	}
}

// sessionAlive says a session has finished an alive from its own queue:
// everything the producer sent before it is done, so a subscribed one moves the
// session's checkpoint. One without a positive timestamp is ignored, as alive
// ignores it. The checkpoint is taken from markAt, as processed takes it.
func (m *machine) sessionAlive(id uuid.UUID, producerID int, generatedAt, markAt time.Time, taken Stamp, subscribed bool) {
	if !validStamp(generatedAt) {
		return
	}
	t, l := m.laneOf(id, producerID)
	if l == nil {
		return
	}
	l.lastAt, l.taken = taken.mono, true
	if subscribed {
		l.checkpoint = laterOf(l.checkpoint, markAt)
	}
	m.sample(l, t, generatedAt, taken)
}

// sample records how old the session's last live message or alive was: its
// taken time against its timestamp by the producer's clock. It is unknown when
// the offset is not fresh.
//
// S7 seam: the safety net judges the sample here.
func (m *machine) sample(l *lane, t *track, generatedAt time.Time, taken Stamp) {
	if !m.offsetFresh(t, m.mono()) {
		l.hasAge = false
		return
	}
	l.age = taken.wall.Sub(generatedAt) - t.offsetAt(taken)
	l.hasAge = true
}

// snapshotComplete says a session has seen a snapshot_complete; an id the
// machine has not in flight is counted, and ignored.
func (m *machine) snapshotComplete(id uuid.UUID, producerID, requestID int) {
	t := m.known(producerID)
	if t == nil {
		return
	}
	if a := t.active; a != nil && a.requestID == requestID {
		if a.awaited[id] {
			a.seen[id] = true
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
	// S4 seam: an event recovery's snapshot_complete is looked up here.
	m.counters.unknownCompletions++
	m.log.Debugf("Snapshot complete of producer %d for request %d, which is not in flight", producerID, requestID)
}

// ---- the REST workers

// answered is a request's outcome: accepted when err is nil.
func (m *machine) answered(requestID int, err error) {
	mono := m.mono()
	// S4 seam: the answer to an event recovery is looked up first.
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
			m.accepted(t, a, mono)
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
	requestID := m.nextID()
	a := &active{
		requestID:   requestID,
		after:       after,
		issuedWall:  now.wall,
		issuedMono:  mono,
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
	if window := time.Duration(t.producer.StatefulRecoveryWindowInMinutes()) * time.Minute; !after.IsZero() && window > 0 {
		// the window counts back from now by the producer's clock, as the gaps' starts are
		after = laterOf(after, t.producerNow(m.clock.Now()).Add(-window))
	}
	return m.capped(t, after)
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
func (m *machine) complete(t *track, a *active, mono time.Duration) {
	t.active = nil
	if !a.accepted {
		// the snapshot_complete was quicker than the API's answer
		m.publishInfo(t, a, true)
	}
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
		}
		if a.seen[s.id] {
			// the snapshot is as of the request, and what came after was in the queue before it
			l.checkpoint = laterOf(l.checkpoint, a.requestedAt)
		}
	}
	t.failures = 0
	t.retryAt = 0
	m.counters.completed++
	t.everRecovered = true
	m.log.Infof("Recovery %d of producer %d completed in %d ms, for %v",
		a.requestID, t.id, (mono - a.issuedMono).Milliseconds(), a.causes)
	if m.hasGaps(t) {
		m.maybeRequest(t, mono)
		return
	}
	m.bringUp(t, causeRecoveryCompleted)
}

// failed says the API did not accept the recovery, or it timed out: it is asked
// for again after the backoff. Once it has failed more often in a row than it is
// re-issued, the producer stays down and it is asked for at a slow, steady pace.
func (m *machine) failed(t *track, a *active, mono time.Duration, reason string) {
	m.ended(t, a)
	m.publishInfo(t, a, false)
	m.counters.failed++
	if !m.hasGaps(t) {
		// nothing is missing, so nothing is asked for again, and nothing failed in a row
		t.failures = 0
		m.upIfNothingMissing(t)
		return
	}
	t.failures++
	if t.failures > recoveryReissues {
		t.retryAt = mono + slowRetryInterval
		m.log.Warnf("Recovery %d of producer %d failed, %s; %d in a row, so the producer stays down and it is asked for every %s",
			a.requestID, t.id, reason, t.failures, slowRetryInterval)
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
	m.ended(t, a)
	m.counters.abandoned++
	m.log.Infof("Recovery %d of producer %d given up: %s", a.requestID, t.id, reason)
}

// ended says the recovery is no longer waited for, without completing: what it
// sent may still be on its way, ahead of the snapshot of the next one asked for.
func (m *machine) ended(t *track, a *active) {
	t.active = nil
	t.remember(ended{requestID: a.requestID, issuedMono: a.issuedMono})
}

// ---- gaps and checkpoints

// openGap opens a gap from from, joined with the one open: the earlier start,
// and a new number, since what is missing now is missing after any recovery
// already asked for. From the zero time, with an initial snapshot interval set,
// the gap is fixed at now less the interval, so asking again does not move it.
func (m *machine) openGap(open *gap, from time.Time) *gap {
	if from.IsZero() && m.settings.initialSnapshot > 0 {
		from = m.clock.Now().wall.Add(-m.settings.initialSnapshot)
	}
	m.gapSeq++
	if open == nil {
		return &gap{from: from, seq: m.gapSeq}
	}
	open.from = earlierOf(open.from, from)
	open.seq = m.gapSeq
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
		t.gap = m.openGap(t.gap, from)
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
// what the client set before the feed opened.
func (m *machine) seed(t *track) time.Time {
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

// ---- producer status

// upIfNothingMissing brings a producer back that is down only for gaps that went
// away with a closed session, or for a lost connection that no session it has now
// missed anything of.
func (m *machine) upIfNothingMissing(t *track) {
	if t.down &&
		m.connected &&
		t.everRecovered &&
		t.active == nil &&
		t.cause != causeProcessingDelay &&
		!m.hasGaps(t) {
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
// bound queue to send to yet: its lost channel has not been opened again.
func (m *machine) awaitsChannel(t *track) bool {
	for _, s := range m.sessions {
		if s.lost && s.lane(t.id) != nil {
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
	}
	m.tracks = append(m.tracks, t)
	m.byID[t.id] = t
	for _, s := range m.sessions {
		if hasLane(s.interest) && s.interest.IsProducerInScope(producer) {
			m.addLane(s, t)
		}
	}
	return t
}

// laneOf is the track and the session's lane of a producer, nil for either that
// does not exist.
func (m *machine) laneOf(id uuid.UUID, producerID int) (*track, *lane) {
	t := m.known(producerID)
	s := m.session(id)
	if t == nil || s == nil {
		return nil, nil
	}
	return t, s.lane(t.id)
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
	// clampWarnedFrom is the start of the gap whose recovery was last clamped to
	// the stateful window and warned of.
	clampWarnedFrom time.Time
	// published is the resume point last published, nil for none.
	published *time.Time

	active *active
	// ended are the recoveries that failed or were given up, newest first, at
	// most endedRemembered: what they sent may still be ahead of the one in flight.
	ended []ended

	// failures is how many recoveries failed in a row; retryAt is the monotonic
	// time before which the next is not asked for.
	failures int
	retryAt  time.Duration
}

// rearm forgets the failures in a row and any backoff: the next recovery goes out
// at once.
func (t *track) rearm() {
	t.failures = 0
	t.retryAt = 0
}

// remember keeps the recovery among the last that ended without completing.
func (t *track) remember(e ended) {
	t.ended = append([]ended{e}, t.ended...)
	if len(t.ended) > endedRemembered {
		t.ended = t.ended[:endedRemembered]
	}
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
	// lost says its channel was lost and no new one is bound yet.
	lost bool
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
	// age is the last sample's age, when hasAge.
	age    time.Duration
	hasAge bool
	// lastAt is when the session last took a message or an alive of the producer, on
	// the monotonic clock, when taken.
	lastAt time.Duration
	taken  bool
}

// gap is what a session misses of a producer, from from; the zero time for
// everything.
type gap struct {
	from time.Time
	// seq is the gap's number: a recovery covers the gaps numbered up to the last
	// at its start.
	seq uint64
}

// active is a producer's recovery in flight.
type active struct {
	requestID int
	// after is where it starts, the zero time for a full snapshot.
	after time.Time
	// issuedWall is when it was asked for by the SDK's wall clock: what the
	// recovery info publishes; issuedMono, on the monotonic clock: what its
	// deadlines and its order count from.
	issuedWall time.Time
	issuedMono time.Duration
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
func (a *active) allSeen() bool {
	for id := range a.awaited {
		if !a.seen[id] {
			return false
		}
	}
	return true
}

// ended is a recovery that ended without completing: its id, and when it was
// asked for.
type ended struct {
	requestID  int
	issuedMono time.Duration
}
