package recovery

import (
	"time"

	"github.com/google/uuid"

	"github.com/oddin-gg/gosdk/types"
)

// What the machine works with, and what it hands out. The machine reads nothing
// but what its facts carry, the catalog and the clock, and writes nothing but
// through the outbox: so a test captures everything it does, and the actor that
// owns it decides where the work runs.

// The numbers of the recovery rules that the client does not set.
const (
	// recoveryReissues is how many times in a row a failed recovery is asked for
	// again with backoff, before the slow retry.
	recoveryReissues = 3
	// firstReissueBackoff is the pause before the first re-issue; it doubles for
	// each next one: 5 s, 10 s, 20 s.
	firstReissueBackoff = 5 * time.Second
	// slowRetryInterval is how often a recovery that has failed more than
	// recoveryReissues times in a row is asked for again, for as long as it fails.
	slowRetryInterval = 60 * time.Second
	// expectedAliveInterval is how often a producer is expected to send an alive,
	// until its alives say.
	expectedAliveInterval = 10 * time.Second
	// endedRemembered is how many of a producer's recoveries that ended without
	// completing are remembered: each sends a snapshot of its own at most, so the
	// traffic that puts off a deadline stays bounded.
	endedRemembered = 8
	// safetyNetResets is how many times the safety net resets one session within
	// safetyNetCooldown; once spent, the session is lagging and is not reset again
	// until the oldest of them has aged out.
	safetyNetResets = 3
	// safetyNetCooldown is how long a reset counts against safetyNetResets.
	safetyNetCooldown = 10 * time.Minute
	// firstResetBackoff is the pause before the net may reset a session again
	// after the first reset; it doubles for each reset made within the cool-down:
	// 1 min, 2 min, 4 min.
	firstResetBackoff = time.Minute
	// resetWindowMargin is how far inside a producer's recovery window a session's
	// checkpoint of it must lie for the net to reset the session: the time from the
	// acceptance of the last recovery to the recovery asked into the new queue (the
	// channel is closed and bound again, a producer that was in backoff asks later),
	// during which the window moves on. It is a margin, not a proof; capped at half
	// the producer's window.
	resetWindowMargin = 10 * time.Minute
)

// recoverySettings are the client's settings the recovery rules use.
type recoverySettings struct {
	// maxInactivity is how long a producer may go without an alive, and a session
	// may lag, before the producer is down.
	maxInactivity time.Duration
	// maxRecoveryTime is how long a recovery may take before it counts as failed.
	maxRecoveryTime time.Duration
	// snapshotCompleteTimeout is how long a recovery waits for its
	// snapshot_complete after it was asked for, or after a session that awaits it
	// last took something that says it is on its way, before it counts as failed.
	snapshotCompleteTimeout time.Duration
	// initialSnapshot is how far back a cold start with no recovery point asks
	// for; 0 for a full snapshot.
	initialSnapshot time.Duration
	// staleLimit is how old, by the producer's clock, a session's live messages
	// of a producer may be before they count as stale; zero switches the safety
	// net off.
	staleLimit time.Duration
	// staleWindow is how long a session's messages have to stay stale before the
	// safety net acts.
	staleWindow time.Duration
	// nodeID is the node id the recovery info carries.
	nodeID *int
}

// producerCatalog is where the machine learns of producers.
type producerCatalog interface {
	// producers are the producers the catalog has now.
	producers() []types.Producer
	// producer looks one up; ok is false when the catalog does not have it.
	producer(id int) (producer types.Producer, ok bool)
	// enabled reports whether the producer is enabled.
	enabled(id int) bool
	// recoveryFrom is the recovery start the client set, the zero time for none.
	recoveryFrom(id int) time.Time
}

// outbox is everything the machine does to the outside. Each call hands the
// work over and returns; a request's outcome comes back as a fact.
type outbox interface {
	// request sends one snapshot recovery request to a REST worker.
	request(snapshotRequest)
	// requestEnded reports that a snapshot recovery request is no longer in
	// flight: completed, failed, timed out or given up. Once per request.
	requestEnded(requestID int)
	// recoveryInfo publishes the outcome of a request: asked for and accepted, or
	// failed.
	recoveryInfo(producerID int, info types.RecoveryInfo)
	// resumePoint publishes the point a client that restarts resumes from.
	resumePoint(producerID int, point time.Time)
	// status reports that a producer went down or came back, or is down for
	// another cause than before.
	status(statusChange)
	// eventRequest sends one event recovery request to a REST worker.
	eventRequest(eventRequest)
	// eventEnded reports that an event recovery ended: completed, failed or
	// timed out. Once per recovery, whatever it ended by.
	eventEnded(eventOutcome)
	// reset replaces the session's channel, and its queue with what it held. The
	// number identifies the reset: its end comes back as a fact carrying it, once.
	reset(session uuid.UUID, number uint64)
}

// snapshotRequest is one recovery request.
type snapshotRequest struct {
	producerID int
	// producer is the producer's name, which the request's path takes.
	producer  string
	requestID int
	// after is where it starts: everything since, or a full snapshot for the zero time.
	after time.Time
}

// eventRequest is one event recovery request: the producer's messages of one
// event again.
type eventRequest struct {
	producerID int
	// producer is the producer's name, which the request's path takes.
	producer  string
	requestID int
	eventID   types.URN
	// stateful asks for the event's stateful messages, not only its odds.
	stateful bool
}

// eventOutcome is how an event recovery ended. The status is never pending, and
// err is nil for a completed one.
type eventOutcome struct {
	producerID int
	requestID  int
	eventID    types.URN
	status     types.RecoveryRequestStatus
	err        error
}

// statusChange is a producer going down or coming back, or being down for
// another cause than before.
type statusChange struct {
	producerID int
	down       bool
	// delayed says a session processes the producer's messages later than the
	// maximum inactivity.
	delayed bool
	cause   statusCause
	at      time.Time
	// public says the down flag or the public reason changed: what the client's
	// producer status events are made of. The rest is for the log.
	public bool
}

// reason is the public reason of the change.
func (c statusChange) reason() types.ProducerStatusReason { return c.cause.reason() }

// statusCause is why a producer went down or came back, more precisely than the
// public reason: several causes share one reason.
type statusCause int

const (
	// causeStarting: every producer starts down, before its first recovery; never reported.
	causeStarting statusCause = iota
	// causeFirstRecoveryCompleted: the first recovery since the feed opened is complete.
	causeFirstRecoveryCompleted
	// causeRecoveryCompleted: a recovery after a down is complete.
	causeRecoveryCompleted
	// causeDelayStabilized: the sessions caught up with a producer that was down
	// for being processed late.
	causeDelayStabilized
	// causeUnsubscribed: an alive said the feed is no longer subscribed to the producer.
	causeUnsubscribed
	// causeAliveIntervalViolation: no alive from the producer for longer than the
	// maximum inactivity.
	causeAliveIntervalViolation
	// causeProcessingDelay: a session processes the producer's messages later than
	// the maximum inactivity.
	causeProcessingDelay
	// causeConnectionLost: the broker connection was lost, and what its queues held with it.
	causeConnectionLost
	// causeChannelLost: a session's channel was lost, and what its queue held with it.
	causeChannelLost
	// causeSessionOpened: a session opened whose lane has a recovery to make.
	causeSessionOpened
	// causeRecoveryFailed: recovery failed as many times in a row as it is
	// re-issued with backoff; it is asked for at a slow pace until it succeeds.
	causeRecoveryFailed
	// causeSafetyNetReset: a session that was too far behind has its channel
	// replaced, and what its queue held with it.
	causeSafetyNetReset
)

var causeDescriptions = map[statusCause]string{
	causeStarting:               "not recovered since the feed opened",
	causeFirstRecoveryCompleted: "first recovery completed",
	causeRecoveryCompleted:      "recovery completed",
	causeDelayStabilized:        "processing caught up",
	causeUnsubscribed:           "an alive said the producer is unsubscribed",
	causeAliveIntervalViolation: "no alive for too long",
	causeProcessingDelay:        "messages processed too late",
	causeConnectionLost:         "the broker connection was lost",
	causeChannelLost:            "a session's channel was lost",
	causeSessionOpened:          "a session opened that has a recovery to make",
	causeRecoveryFailed:         "recovery failed too often in a row",
	causeSafetyNetReset:         "a session too far behind was reset",
}

// String is the cause in a few words, for the log.
func (c statusCause) String() string { return causeDescriptions[c] }

// reason is the public reason of the cause, in the module's own status reasons.
func (c statusCause) reason() types.ProducerStatusReason {
	switch c {
	case causeFirstRecoveryCompleted:
		return types.FirstRecoveryCompletedProducerStatusReason
	case causeRecoveryCompleted, causeDelayStabilized:
		return types.ReturnedFromInactivityProducerStatusReason
	case causeAliveIntervalViolation:
		return types.AliveIntervalViolationProducerStatusReason
	case causeProcessingDelay, causeSafetyNetReset:
		return types.ProcessingQueueDelayViolationProducerStatusReason
	case causeConnectionLost, causeChannelLost:
		return types.ConnectionDownProducerStatusReason
	case causeStarting, causeUnsubscribed, causeSessionOpened, causeRecoveryFailed:
		return types.OtherProducerStatusReason
	default:
		return types.OtherProducerStatusReason
	}
}
