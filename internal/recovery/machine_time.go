package recovery

import (
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/oddin-gg/gosdk/types"
)

// The rules that need the clock to run: the deadlines of a recovery, a
// producer's inactivity, and a session that processes late. Failure, backoff
// and the slow retry that these deadlines feed are in machine.go, next to
// complete.

// processing says which sessions have a message of the client in processing,
// and since when, on the monotonic clock: the sessions not in it have none. The
// actor tells it before each tick, from what the sessions report. It is what
// holds the deadline of a recovery while a session's queue is stuck behind a
// message, see stalledAmong.
func (m *machine) processing(since map[uuid.UUID]time.Duration) {
	for _, s := range m.sessions {
		s.processingSince, s.processing = since[s.id]
	}
}

// stalledAmong reports whether a session of those awaited that has not seen the
// snapshot_complete has had one message of the client in processing for longer
// than the maximum inactivity. A busy client always has some message in
// processing; only one that stays there that long says the session's queue is
// stuck behind it, with the snapshot it waits for.
func (m *machine) stalledAmong(awaited, seen map[uuid.UUID]bool, mono time.Duration) bool {
	for _, s := range m.sessions {
		if awaited[s.id] && !seen[s.id] && s.processing && mono-s.processingSince > m.settings.maxInactivity {
			return true
		}
	}
	return false
}

// tick says the clock moved on: recoveries time out, producers without alives
// go down, sessions that lag take producers down and bring them back, and
// recoveries waiting for their backoff go out. The actor calls it at least once a
// second.
//
// Event recoveries expire here, and the deferred ones.
//
// Seam for the safety net: a pending reset of it, waiting for a disabled producer, is
// cancelled here.
func (m *machine) tick() {
	mono := m.mono()
	for _, t := range m.tracks {
		if !m.catalog.enabled(t.id) {
			continue
		}
		if a := t.active; a != nil {
			if m.stalledAmong(a.awaited, a.seen, mono) {
				// a session with a client message in processing for longer than the
				// maximum inactivity counts as heard: its queue is stuck behind the
				// message, the snapshot with it, and a request again would queue a
				// second one there
				a.heardAt = mono
			}
			switch {
			case mono-a.issuedMono > m.settings.maxRecoveryTime:
				m.counters.timedOut++
				m.failed(t, a, mono, "no snapshot complete within "+m.settings.maxRecoveryTime.String())
			case mono-a.heardAt > m.settings.snapshotCompleteTimeout:
				m.counters.timedOut++
				m.failed(t, a, mono,
					"no snapshot complete, nor anything on its way, within "+m.settings.snapshotCompleteTimeout.String())
			}
		}
		if m.started {
			last := t.lastAliveAt
			if !t.hasAlive {
				last = max(t.createdAt, m.startedAt)
			}
			if !t.inactive && mono-last > m.settings.maxInactivity {
				t.inactive = true
				m.openProducerGap(t, t.lastAliveMark)
				// a producer that went silent while a recovery was in flight may have lost it
				m.giveUp(t, mono, "the producer sent no alive for "+m.settings.maxInactivity.String())
				m.markDown(t, causeAliveIntervalViolation)
			}
			m.checkDelay(t, mono)
		}
		m.maybeRequest(t, mono)
	}
	m.expireEvents(mono)
}

// expireEvents ends the event recoveries that took longer than the maximum
// recovery time, or that the API accepted and nothing of was heard for the
// snapshot-complete timeout since: their snapshot_complete is lost. One the API
// has not answered yet waits for its REST worker's own deadlines, and the
// maximum recovery time. A deferred one expires at the maximum recovery time
// too: the channel it waited for never came back. The maximum counts from when
// the recovery was asked for, whether it waited or not.
func (m *machine) expireEvents(mono time.Duration) {
	for _, e := range m.sortedEvents() {
		if e.accepted && m.stalledAmong(e.awaited, e.seen, mono) {
			// as for a recovery: a stuck callback holds the deadline
			e.heardAt = mono
		}
		var why string
		switch {
		case mono-e.askedMono > m.settings.maxRecoveryTime:
			why = "no snapshot complete within " + m.settings.maxRecoveryTime.String()
		case e.accepted && mono-e.heardAt > m.settings.snapshotCompleteTimeout:
			why = "no snapshot complete, nor anything of it heard, within " + m.settings.snapshotCompleteTimeout.String()
		default:
			continue
		}
		m.counters.eventExpired++
		m.log.Debugf("Event recovery %d of %s: %s", e.requestID, e.eventID.ToString(), why)
		m.failEvent(e, types.RecoveryStatusTimedOut, errors.New(why))
	}
	var waiting []deferredEvent
	for _, d := range m.deferred {
		if mono-d.deferredAt <= m.settings.maxRecoveryTime {
			waiting = append(waiting, d)
			continue
		}
		m.expireDeferred(d)
	}
	m.deferred = waiting
}

// heard says what a session took may say the recovery in flight is still on its
// way to it, which puts off the deadline for its snapshot_complete: a message of
// its snapshot, or one the snapshot and its snapshot_complete queue behind (a
// live message or an alive sent before the request, or a snapshot message of an
// earlier recovery of the producer that failed or was given up, or of an event
// recovery asked for before it). This is evidence per session: nothing global is
// read. Only a session that awaits the recovery and has not seen its
// snapshot_complete is heard. An event recovery is put off the same way, by its
// own snapshot and by what queues ahead of it.
func (m *machine) heard(t *track, id uuid.UUID, generatedAt time.Time, requestID int) {
	mono := m.mono()
	for _, e := range m.eventRecoveries {
		if e.producerID == t.id && e.awaited[id] && !e.seen[id] && m.eventAhead(t, e, generatedAt, requestID) {
			e.heardAt = mono
		}
	}
	a := t.active
	if a == nil || !a.awaited[id] || a.seen[id] {
		return
	}
	if requestID == 0 {
		if !generatedAt.Before(a.requestedAt) {
			return
		}
	} else if !m.ahead(t, a, requestID) {
		return
	}
	a.heardAt = mono
}

// ahead reports whether a snapshot message of requestID is the recovery's own,
// or queued ahead of it: one of the recoveries or event recoveries of the
// producer that ended without completing, or an event recovery in flight, asked
// for before the one in flight. An event recovery asked for since is not: one
// after another they would hold a recovery whose snapshot_complete is lost for
// the maximum recovery time. Order is by when each was sent, never by the
// value of an id: the sequence number says it, not the clock, which can read the
// same for two requests.
func (m *machine) ahead(t *track, a *active, requestID int) bool {
	if requestID == a.requestID {
		return true
	}
	if e := m.eventRecoveries[requestID]; e != nil {
		return e.producerID == t.id && e.seq < a.seq
	}
	// what ended before the one in flight was asked for: the one in flight is the
	// only one at a time, so no order is read
	for _, e := range t.ended {
		if e.requestID == requestID {
			return true
		}
	}
	return askedBefore(t.endedEvents, requestID, a.seq)
}

// eventAhead reports whether a message a session took is the event recovery's own
// snapshot, or queued ahead of it: a live message or an alive generated before it
// was asked for, or a snapshot message of a recovery or an event recovery of the
// producer asked for before it, in flight still or ended without completing.
func (m *machine) eventAhead(t *track, e *eventActive, generatedAt time.Time, requestID int) bool {
	if requestID == 0 {
		return generatedAt.Before(e.requestedAt)
	}
	if requestID == e.requestID {
		return true
	}
	if a := t.active; a != nil && a.requestID == requestID {
		return a.seq < e.seq
	}
	if other := m.eventRecoveries[requestID]; other != nil {
		return other.producerID == t.id && other.seq < e.seq
	}
	return askedBefore(t.ended, requestID, e.seq) || askedBefore(t.endedEvents, requestID, e.seq)
}

// askedBefore reports whether the request id is one of list, sent before the
// request numbered seq.
func askedBefore(list []ended, requestID int, seq uint64) bool {
	for _, e := range list {
		if e.requestID == requestID {
			return e.seq < seq
		}
	}
	return false
}

// checkDelay says whether a session processes the producer late: its last
// sample older than the maximum inactivity, or nothing taken for that long
// while alives say the producer sends. Each session is judged by what it took
// itself. A session that has taken nothing yet counts as on time. Late takes an
// up producer down, without a recovery; on time again brings it back.
func (m *machine) checkDelay(t *track, mono time.Duration) {
	limit := m.settings.maxInactivity
	delayed := false
	for _, s := range m.sessions {
		l := s.lane(t.id)
		if l == nil {
			continue
		}
		if (l.hasAge && l.age > limit && m.offsetFresh(t, mono)) ||
			(l.taken && mono-l.lastAt > limit && m.aliveRecent(t, mono)) {
			delayed = true
		}
	}
	t.delayed = delayed
	switch {
	case delayed && !t.down:
		m.markDown(t, causeProcessingDelay)
	case !delayed && t.down && t.cause == causeProcessingDelay:
		m.markUp(t, causeDelayStabilized)
	}
	if t.told && t.toldDelayed != t.delayed {
		// the flag flipped with no change of the producer's state to carry it: it is
		// told on its own, with the cause the producer has
		m.tell(t, t.cause, true)
	}
}
