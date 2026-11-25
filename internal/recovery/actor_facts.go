package recovery

import (
	"time"

	"github.com/google/uuid"

	"github.com/oddin-gg/gosdk/types"
)

// A fact is what the rest of the feed tells the recovery actor: something that
// happened, handled by the actor's goroutine in the order the queues give. The
// essential facts are never dropped, since losing one leaves the state wrong
// for good; the samples are many, each worth little, and a full queue drops one.

// fact is the marker of everything the actor's queues carry.
type fact interface{ isFact() }

// ---- essential facts

// factOpened is a session of the client registered, its queue not bound yet. at
// is the reading of the clock taken before the queue is bound.
type factOpened struct {
	session  uuid.UUID
	interest types.MessageInterest
	at       Stamp
}

// factClosed is a session of the client gone.
type factClosed struct{ session uuid.UUID }

// factStart is the feed opening: the sessions registered so far miss everything
// before now.
type factStart struct{}

// factAlives is the alives of one producer that arrived one after the other with no
// essential fact between them: the machine is given what it needs of them. An alive
// that arrives after any other essential fact is in a fact of its own, so no alive
// is handled before an essential fact posted before it.
type factAlives struct {
	producer int
	slot     aliveSlot
}

// factSnapshotComplete is a snapshot_complete a session took.
type factSnapshotComplete struct {
	session   uuid.UUID
	producer  int
	requestID int
}

// factChannelLost is a session's channel lost, and the queue it held with it.
type factChannelLost struct{ session uuid.UUID }

// factChannelReopened is a session's queue bound: after its first open, and after a loss.
type factChannelReopened struct{ session uuid.UUID }

// factAnswered is a REST worker's answer to a request: err is nil when the API
// accepted it.
type factAnswered struct {
	requestID int
	err       error
}

// factRewind is the client setting the producer's recovery start after the feed
// opened; the zero time clears it.
type factRewind struct {
	producer int
	to       time.Time
}

// ---- commands

// recoverEventCommand is the client asking for one event's messages again; the
// actor answers on reply.
type recoverEventCommand struct {
	producerID int
	eventID    types.URN
	stateful   bool
	reply      chan recoverEventReply
}

// recoverEventReply is the answer to a recoverEventCommand: the handle of the
// event recovery, or why it was refused.
type recoverEventReply struct {
	handle *Handle
	err    error
}

// ---- samples

// factProcessed is a message of the client a session finished: handed over, and
// its delivery settled.
type factProcessed struct {
	session     uuid.UUID
	producer    int
	generatedAt time.Time
	taken       Stamp
	requestID   int
}

// factSessionAlive is an alive a session of the client finished.
type factSessionAlive struct {
	session     uuid.UUID
	producer    int
	generatedAt time.Time
	taken       Stamp
	subscribed  bool
}

// ---- the clock

// factTick is the actor's own look at the clock.
type factTick struct{}

func (factOpened) isFact()           {}
func (factClosed) isFact()           {}
func (factStart) isFact()            {}
func (factAlives) isFact()           {}
func (factSnapshotComplete) isFact() {}
func (factChannelLost) isFact()      {}
func (factChannelReopened) isFact()  {}
func (factAnswered) isFact()         {}
func (factRewind) isFact()           {}
func (*recoverEventCommand) isFact() {}
func (factProcessed) isFact()        {}
func (factSessionAlive) isFact()     {}
func (factTick) isFact()             {}

// aliveSeen is an alive of a producer's alive channel, as the slot keeps it.
type aliveSeen struct {
	generatedAt time.Time
	received    Stamp
	subscribed  bool
}

// aliveSlot is the alives of a fact not taken yet: the alives of a producer come
// every few seconds, but a stalled actor must not hold them one by one. It
// keeps what the machine needs of them: the last subscribed one before the first
// unsubscribed one, where a gap starts; that first unsubscribed one, which opens
// the gap; and the latest.
type aliveSlot struct {
	latest                       aliveSeen
	lastSubscribedBeforeFirstOff *aliveSeen
	firstUnsubscribed            *aliveSeen
	afterUnsubscribed            bool
}

// add joins an alive to the slot.
func (s *aliveSlot) add(alive aliveSeen) {
	switch {
	case s.firstUnsubscribed != nil:
		s.afterUnsubscribed = true
	case alive.subscribed:
		s.lastSubscribedBeforeFirstOff = &alive
	default:
		s.firstUnsubscribed = &alive
	}
	s.latest = alive
}

// inOrder is what the machine is given, in the order the alives came.
func (s *aliveSlot) inOrder() []aliveSeen {
	if s.firstUnsubscribed == nil {
		return []aliveSeen{s.latest}
	}
	out := make([]aliveSeen, 0, 3)
	if s.lastSubscribedBeforeFirstOff != nil {
		out = append(out, *s.lastSubscribedBeforeFirstOff)
	}
	out = append(out, *s.firstUnsubscribed)
	if s.afterUnsubscribed {
		out = append(out, s.latest)
	}
	return out
}
