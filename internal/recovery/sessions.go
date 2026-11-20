package recovery

import (
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/oddin-gg/gosdk/types"
)

// sessionRecord is what the manager knows of one session of the client: who
// it is, what it receives, and the last fact it reported per producer.
//
// A session reports from its own goroutines (the facts about a message are
// posted when the broker has been told the message is settled), so the
// facts are kept per session and per producer, not merged: what the feed
// has handled is a per-session truth, and a producer is only as far as its
// slowest session.
type sessionRecord struct {
	id       uuid.UUID
	interest types.MessageInterest
	// takesSnapshotComplete is whether the session's queue gets the
	// snapshot_complete messages a recovery waits for. Every session does,
	// except a low-priority one next to a high-priority one, which would
	// see each completion twice. It changes as sessions come and go, since
	// "next to" means the other open sessions of the client.
	takesSnapshotComplete bool
	// producers are the producers of the catalog the session's interest
	// covers, as of when it registered, in id order. A producer the catalog
	// learns later is not in it.
	producers []int

	processed        map[int]processedFact
	alive            map[int]aliveFact
	snapshotComplete map[int]snapshotCompleteFact
}

// processedFact is the last message of a producer a session has finished
// with: handed over to the client and acknowledged to the broker.
type processedFact struct {
	generatedAt time.Time
	// taken is when the session began processing the message, both
	// readings: its age includes the time it waited in the broker's queue
	// and the session's own.
	taken stamp
	// requestID is the recovery the message belongs to; 0 for a live message.
	requestID int
	// count is how many messages of the producer the session has reported.
	count int
}

// aliveFact is the last alive of a producer a session has handled.
type aliveFact struct {
	generatedAt time.Time
	taken       stamp
	subscribed  bool
}

// snapshotCompleteFact is the last snapshot_complete of a producer a session
// has handled.
type snapshotCompleteFact struct {
	requestID int
	at        stamp
}

// sessionFacts is a copy of a sessionRecord for readers outside the lock.
type sessionFacts struct {
	id                    uuid.UUID
	interest              types.MessageInterest
	takesSnapshotComplete bool
	producers             []int
	processed             map[int]processedFact
	alive                 map[int]aliveFact
	snapshotComplete      map[int]snapshotCompleteFact
}

// hasLane reports whether the session has a lane of its own to recover: it
// receives messages. The alive-only interest has none, so it holds no
// recovery and loses nothing.
func hasLane(interest types.MessageInterest) bool {
	return interest != types.SystemAliveOnly
}

// takesSnapshotComplete is the rule for one session among the interests of
// all the open sessions of the client.
func takesSnapshotComplete(interest types.MessageInterest, all []types.MessageInterest) bool {
	if !hasLane(interest) {
		return false
	}
	return !(interest == types.LowPriorityOnlyMessageInterest && slices.Contains(all, types.HiPriorityOnlyMessageInterest))
}

// producersOfInterest lists the catalog's producers the interest covers, in
// id order. A catalog that is not there yet gives none.
func (m *Manager) producersOfInterest(interest types.MessageInterest) []int {
	if m.producerManager == nil {
		return nil
	}
	catalog, err := m.producerManager.AvailableProducersCached()
	if err != nil {
		return nil
	}
	ids := make([]int, 0, len(catalog))
	for id, prod := range catalog {
		if interest.IsProducerInScope(prod) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// refreshSnapshotCompleteLocked recomputes which sessions take
// snapshot_complete. clientMu must be held.
func (m *Manager) refreshSnapshotCompleteLocked() {
	all := make([]types.MessageInterest, 0, len(m.clientSessions))
	for _, rec := range m.clientSessions {
		all = append(all, rec.interest)
	}
	for _, rec := range m.clientSessions {
		rec.takesSnapshotComplete = takesSnapshotComplete(rec.interest, all)
	}
}

// sessionFactsOf returns a copy of the record of a session, if it is
// registered.
func (m *Manager) sessionFactsOf(session uuid.UUID) (sessionFacts, bool) {
	m.clientMu.Lock()
	defer m.clientMu.Unlock()
	rec, ok := m.clientSessions[session]
	if !ok {
		return sessionFacts{}, false
	}
	return sessionFacts{
		id:                    rec.id,
		interest:              rec.interest,
		takesSnapshotComplete: rec.takesSnapshotComplete,
		producers:             slices.Clone(rec.producers),
		processed:             cloneMap(rec.processed),
		alive:                 cloneMap(rec.alive),
		snapshotComplete:      cloneMap(rec.snapshotComplete),
	}, true
}

func cloneMap[K comparable, V any](in map[K]V) map[K]V {
	out := make(map[K]V, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// OnMessageProcessed records that the session has finished with a message of
// the producer: the client has it and the broker has been told. It is
// posted when the delivery is acknowledged, so what it records is what the
// broker has let go of, not what the session has merely read.
//
// generatedAt is the message's timestamp by the producer's clock, taken is
// when the session began processing it, and requestID the recovery it
// belongs to (0 for a live message).
//
// Only the session's record changes. The actor's cursor still moves where it
// always did, at OnMessageProcessingEnded, so nothing of what the actor
// decides changes with this fact. A session that is not registered (the
// alive-only one, or one already gone) keeps nothing.
func (m *Manager) OnMessageProcessed(session uuid.UUID, producerID int, generatedAt time.Time, taken Stamp, requestID int) {
	m.clientMu.Lock()
	defer m.clientMu.Unlock()
	rec, ok := m.clientSessions[session]
	if !ok {
		return
	}
	f := rec.processed[producerID]
	f.generatedAt, f.taken, f.requestID = generatedAt, taken, requestID
	f.count++
	rec.processed[producerID] = f
}

// OnSessionAlive records an alive of the producer that a session of the
// client has handled, and tells the actor of it as it always was told: the
// alive of a client session is the sign the client side is getting messages.
//
// The alive-only session reports its alives through OnAliveReceived, which
// is the alive channel of the SDK and bound to no session.
func (m *Manager) OnSessionAlive(session uuid.UUID, producerID int, generatedAt time.Time, taken Stamp, isSubscribed bool, messageInterest types.MessageInterest) {
	m.clientMu.Lock()
	if rec, ok := m.clientSessions[session]; ok {
		rec.alive[producerID] = aliveFact{generatedAt: generatedAt, taken: taken, subscribed: isSubscribed}
	}
	m.clientMu.Unlock()

	m.OnAliveReceived(producerID, types.MessageTimestamp{Created: generatedAt}, isSubscribed, messageInterest)
}

// recordSnapshotComplete notes that a snapshot_complete has been admitted to the actor for the session:
// recorded as the session's last completion of the producer.
func (m *Manager) recordSnapshotComplete(session uuid.UUID, producerID int, requestID int) {
	at := m.clock().now()
	m.clientMu.Lock()
	defer m.clientMu.Unlock()
	if rec, ok := m.clientSessions[session]; ok {
		rec.snapshotComplete[producerID] = snapshotCompleteFact{requestID: requestID, at: at}
	}
}

// clock is the manager's clock; a manager built without one reads the
// system's.
func (m *Manager) clock() clock {
	if m.clk == nil {
		return systemClock{}
	}
	return m.clk
}
