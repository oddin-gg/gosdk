package recovery

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/oddin-gg/gosdk/internal/producer"
	"github.com/oddin-gg/gosdk/protocols"
)

// producerRecoveryData holds the per-producer recovery state. Phase 5b
// rewrite: every mutable field is now accessed only through methods that
// take p.lock — the partial-locking pattern (only `name`/`eventRecoveries`
// guarded, every other field racing) is the documented source of the
// 65 %-out-of-order rate observed in the smoke run.
//
// External callers (manager.go) MUST go through the accessor methods
// rather than touching fields directly.
//
// Phase 5 v2 (deferred) replaces this struct with a per-producer actor
// goroutine per NEXT.md §11.
type producerRecoveryData struct {
	producerID      uint
	producerManager *producer.Manager

	lock sync.RWMutex

	// All fields below are guarded by lock.

	currentRecovery *recoveryData
	eventRecoveries map[uint]*eventRecovery

	lastUserSessionAliveReceivedTimestamp time.Time
	lastValidAliveGenTimestampInRecovery  time.Time

	recoveryState                    protocols.RecoveryState
	lastSystemAliveReceivedTimestamp *time.Time

	firstRecoveryCompleted bool

	producerDownReason   protocols.ProducerDownReason
	producerStatusReason protocols.ProducerStatusReason
}

func newProducerRecoveryData(producerID uint, producerManager *producer.Manager) *producerRecoveryData {
	return &producerRecoveryData{
		producerID:      producerID,
		producerManager: producerManager,
		eventRecoveries: make(map[uint]*eventRecovery),
	}
}

// --- guarded accessors (read) ---

func (p *producerRecoveryData) getRecoveryState() protocols.RecoveryState {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.recoveryState
}

func (p *producerRecoveryData) getLastSystemAliveReceivedTimestamp() *time.Time {
	p.lock.RLock()
	defer p.lock.RUnlock()
	if p.lastSystemAliveReceivedTimestamp == nil {
		return nil
	}
	v := *p.lastSystemAliveReceivedTimestamp
	return &v
}

func (p *producerRecoveryData) getLastUserSessionAliveReceivedTimestamp() time.Time {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.lastUserSessionAliveReceivedTimestamp
}

func (p *producerRecoveryData) getLastValidAliveGenTimestampInRecovery() time.Time {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.lastValidAliveGenTimestampInRecovery
}

func (p *producerRecoveryData) getFirstRecoveryCompleted() bool {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.firstRecoveryCompleted
}

func (p *producerRecoveryData) getProducerDownReason() protocols.ProducerDownReason {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.producerDownReason
}

func (p *producerRecoveryData) getProducerStatusReason() protocols.ProducerStatusReason {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.producerStatusReason
}

// --- guarded accessors (write) ---

func (p *producerRecoveryData) setLastUserSessionAliveReceivedTimestamp(t time.Time) {
	p.lock.Lock()
	defer p.lock.Unlock()
	p.lastUserSessionAliveReceivedTimestamp = t
}

func (p *producerRecoveryData) setFirstRecoveryCompleted(v bool) {
	p.lock.Lock()
	defer p.lock.Unlock()
	p.firstRecoveryCompleted = v
}

func (p *producerRecoveryData) setProducerStatusReason(r protocols.ProducerStatusReason) {
	p.lock.Lock()
	defer p.lock.Unlock()
	p.producerStatusReason = r
}

// --- compound operations ---

// isPerformingRecovery reads recoveryState under RLock.
func (p *producerRecoveryData) isPerformingRecovery() bool {
	p.lock.RLock()
	defer p.lock.RUnlock()
	if p.recoveryState == protocols.DefaultRecoveryState {
		return false
	}
	return p.recoveryState == protocols.StartedRecoveryState || p.recoveryState == protocols.InterruptedRecoveryState
}

// isFlaggedDown delegates to the producer manager (its own state).
func (p *producerRecoveryData) isFlaggedDown() bool {
	down, err := p.producerManager.IsProducerDown(context.Background(), p.producerID)
	if err != nil {
		return true
	}
	return down
}

// isDisabled delegates to the producer manager.
func (p *producerRecoveryData) isDisabled() bool {
	enabled, err := p.producerManager.IsProducerEnabled(context.Background(), p.producerID)
	if err != nil {
		return true
	}
	return !enabled
}

func (p *producerRecoveryData) eventRecoveryCompleted(recoveryID uint) {
	p.lock.Lock()
	defer p.lock.Unlock()
	delete(p.eventRecoveries, recoveryID)
}

// systemAliveReceived updates timestamp + (conditionally) the
// gen-timestamp-in-recovery, all under a single lock.
func (p *producerRecoveryData) systemAliveReceived(receivedTimestamp time.Time, aliveGenTimestamp time.Time) error {
	p.lock.Lock()
	t := receivedTimestamp
	p.lastSystemAliveReceivedTimestamp = &t
	if p.recoveryState == protocols.StartedRecoveryState {
		p.lastValidAliveGenTimestampInRecovery = aliveGenTimestamp
	}
	flaggedDownNeeded := false
	p.lock.Unlock()

	// External producer-manager call must NOT hold p.lock.
	if !p.isFlaggedDown() {
		flaggedDownNeeded = true
	}
	if flaggedDownNeeded {
		if err := p.producerManager.SetLastAliveReceivedGenTimestamp(p.producerID, aliveGenTimestamp); err != nil {
			return err
		}
	}
	return nil
}

func (p *producerRecoveryData) validateSnapshotComplete(recoveryID uint, messageInterest protocols.MessageInterest) bool {
	p.lock.RLock()
	state := p.recoveryState
	currentRecovery := p.currentRecovery
	p.lock.RUnlock()

	switch {
	case state == protocols.DefaultRecoveryState,
		state != protocols.StartedRecoveryState && state != protocols.InterruptedRecoveryState:
		return false
	case currentRecovery != nil && currentRecovery.recoveryID != recoveryID:
		return false
	case !p.snapshotValidationNeeded(messageInterest):
		return true
	case currentRecovery == nil:
		return false
	}

	res, err := p.validateProducerSnapshotCompletes(currentRecovery.snapshotComplete(messageInterest))
	if err != nil {
		return false
	}
	return res
}

func (p *producerRecoveryData) validateEventSnapshotComplete(recoveryID uint, interest protocols.MessageInterest) bool {
	p.lock.RLock()
	er, ok := p.eventRecoveries[recoveryID]
	p.lock.RUnlock()

	switch {
	case !ok:
		return false
	case !p.snapshotValidationNeeded(interest):
		return true
	}

	res, err := p.validateProducerSnapshotCompletes(er.snapshotComplete(interest))
	if err != nil {
		return false
	}
	return res
}

func (p *producerRecoveryData) isKnownRecovery(requestID uint) bool {
	p.lock.RLock()
	defer p.lock.RUnlock()
	hasRequestID := p.currentRecovery != nil && p.currentRecovery.recoveryID == requestID
	_, ok := p.eventRecoveries[requestID]
	return hasRequestID || ok
}

// validateProducerSnapshotCompletes calls into the producer manager which
// has its own locking — must NOT hold p.lock here.
func (p *producerRecoveryData) validateProducerSnapshotCompletes(receivedSnapshotCompletes []protocols.MessageInterest) (bool, error) {
	prod, err := p.producerManager.GetProducer(context.Background(), p.producerID)
	if err != nil {
		return false, err
	}

	finished := make([]bool, len(prod.ProducerScopes()))
	for i, scope := range prod.ProducerScopes() {
		for _, interest := range receivedSnapshotCompletes {
			switch scope {
			case protocols.LiveProducerScope:
				finished[i] = interest == protocols.LiveOnlyMessageInterest
			case protocols.PrematchProducerScope:
				finished[i] = interest == protocols.PrematchOnlyMessageInterest
			default:
				return false, fmt.Errorf("unknown producer scope - %d", scope)
			}
		}
	}

	for _, value := range finished {
		if !value {
			return false, nil
		}
	}
	return true, nil
}

func (p *producerRecoveryData) snapshotValidationNeeded(interest protocols.MessageInterest) bool {
	return interest == protocols.LiveOnlyMessageInterest || interest == protocols.PrematchOnlyMessageInterest
}

func (p *producerRecoveryData) eventRecovery(recoveryID uint) *eventRecovery {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.eventRecoveries[recoveryID]
}

func (p *producerRecoveryData) lastRecoveryStartedAt() time.Time {
	p.lock.RLock()
	defer p.lock.RUnlock()
	if p.currentRecovery != nil {
		return p.currentRecovery.recoveryStartedAt
	}
	return time.Time{}
}

func (p *producerRecoveryData) timestampForRecovery() (time.Time, error) {
	prod, err := p.producerManager.GetProducer(context.Background(), p.producerID)
	if err != nil {
		return time.Time{}, err
	}
	return prod.TimestampForRecovery(), nil
}

func (p *producerRecoveryData) setLastMessageReceivedTimestamp(timestamp time.Time) error {
	if timestamp.IsZero() {
		return errors.New("required non zero timestamp")
	}
	return p.producerManager.SetProducerLastMessageTimestamp(p.producerID, timestamp)
}

func (p *producerRecoveryData) lastProcessedMessageGenTimestamp() (time.Time, error) {
	prod, err := p.producerManager.GetProducer(context.Background(), p.producerID)
	if err != nil {
		return time.Time{}, err
	}
	return prod.LastProcessedMessageGenTimestamp(), nil
}

func (p *producerRecoveryData) setLastProcessedMessageGenTimestamp(timestamp time.Time) error {
	if timestamp.IsZero() {
		return errors.New("required non zero timestamp")
	}
	return p.producerManager.SetLastProcessedMessageGenTimestamp(p.producerID, timestamp)
}

func (p *producerRecoveryData) producerName() (string, error) {
	prod, err := p.producerManager.GetProducer(context.Background(), p.producerID)
	if err != nil {
		return "", err
	}
	return prod.Name(), nil
}

func (p *producerRecoveryData) setProducerRecoveryState(recoveryID uint, recoveryStartedAt time.Time, recoveryState protocols.RecoveryState) {
	p.lock.Lock()
	defer p.lock.Unlock()
	p.recoveryState = recoveryState
	rd := newRecoveryData(recoveryID, recoveryStartedAt)
	p.currentRecovery = &rd
}

func (p *producerRecoveryData) interruptProducerRecovery() {
	p.lock.Lock()
	defer p.lock.Unlock()
	p.recoveryState = protocols.InterruptedRecoveryState
}

func (p *producerRecoveryData) setProducerDown(reason protocols.ProducerDownReason) error {
	if err := p.producerManager.SetProducerDown(p.producerID, true); err != nil {
		return err
	}
	p.lock.Lock()
	defer p.lock.Unlock()
	p.producerDownReason = reason
	p.eventRecoveries = make(map[uint]*eventRecovery)
	return nil
}

func (p *producerRecoveryData) setProducerUp() error {
	if err := p.producerManager.SetProducerDown(p.producerID, false); err != nil {
		return err
	}
	p.lock.Lock()
	defer p.lock.Unlock()
	p.producerDownReason = protocols.DefaultProducerDownReason
	return nil
}

func (p *producerRecoveryData) setEventRecoveryState(eventID protocols.URN, recoveryID uint, recoveryStartedAt time.Time) {
	p.lock.Lock()
	defer p.lock.Unlock()
	switch {
	case recoveryID == 0 && recoveryStartedAt.IsZero():
		delete(p.eventRecoveries, recoveryID)
	default:
		er := newEventRecovery(eventID, recoveryID, recoveryStartedAt)
		p.eventRecoveries[recoveryID] = &er
	}
}
