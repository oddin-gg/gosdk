package recovery

import (
	"time"

	"github.com/google/uuid"
)

// The rules that need the clock to run: the deadlines of a recovery, a
// producer's inactivity, and a session that processes late. Failure, backoff
// and the slow retry that these deadlines feed are in machine.go, next to
// complete.

// tick says the clock moved on: recoveries time out, producers without alives
// go down, sessions that lag take producers down and bring them back, and
// recoveries waiting for their backoff go out. The actor calls it at least once a
// second.
//
// S4 seam: the event recoveries expire here, and the deferred ones.
// S7 seam: a pending reset of the safety net waiting for a disabled producer is
// cancelled here.
func (m *machine) tick() {
	mono := m.mono()
	for _, t := range m.tracks {
		if !m.catalog.enabled(t.id) {
			continue
		}
		if a := t.active; a != nil {
			switch {
			case mono-a.issuedMono > m.settings.maxRecoveryTime:
				m.counters.timedOut++
				m.failed(t, a, mono, "no snapshot complete within "+m.settings.maxRecoveryTime.String())
			case mono-a.heardAt > m.settings.snapshotCompleteTimeout:
				// Seam for the client's processing hold: a session with a client
				// message in processing for longer than the maximum inactivity counts
				// as heard here, so the deadline does not fire on a stuck callback.
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
}

// heard says what a session took may say the recovery in flight is still on its
// way to it, which puts off the deadline for its snapshot_complete: a message of
// its snapshot, or one the snapshot and its snapshot_complete queue behind (a
// live message or an alive sent before the request, or a snapshot message of an
// earlier recovery of the producer that failed or was given up). This is
// evidence per session: nothing global is read. Only a session that awaits the
// recovery and has not seen its snapshot_complete is heard.
//
// S4 seam: an event recovery's snapshot is heard here too.
func (m *machine) heard(t *track, id uuid.UUID, generatedAt time.Time, requestID int) {
	a := t.active
	if a == nil || !a.awaited[id] || a.seen[id] {
		return
	}
	if requestID == 0 {
		if !generatedAt.Before(a.requestedAt) {
			return
		}
	} else if !t.ahead(a, requestID) {
		return
	}
	a.heardAt = m.mono()
}

// ahead reports whether a snapshot message of requestID is the recovery's own,
// or queued ahead of it: one of the recoveries that ended before the one in
// flight was asked for (the one in flight is the only one at a time, so what
// ended is earlier by construction, and no order of ids or clocks is read).
func (t *track) ahead(a *active, requestID int) bool {
	if requestID == a.requestID {
		return true
	}
	for _, e := range t.ended {
		if e.requestID == requestID {
			return true
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
}
