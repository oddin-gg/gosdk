package recovery

import (
	"sync"
	"time"

	"github.com/oddin-gg/gosdk/internal/producer"
	"github.com/oddin-gg/gosdk/protocols"
	"github.com/pkg/errors"
)

type producerRecoveryData struct {
	producerID      uint
	producerManager *producer.Manager

	lock sync.Mutex

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

func (p *producerRecoveryData) isPerformingRecovery() bool {
	if p.recoveryState == protocols.DefaultRecoveryState {
		return false
	}

	return p.recoveryState == protocols.StartedRecoveryState || p.recoveryState == protocols.InterruptedRecoveryState
}

func (p *producerRecoveryData) isFlaggedDown() bool {
	down, err := p.producerManager.IsProducerDown(p.producerID)
	if err != nil {
		return true
	}

	return down
}

func (p *producerRecoveryData) isDisabled() bool {
	enabled, err := p.producerManager.IsProducerEnabled(p.producerID)
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

func (p *producerRecoveryData) systemAliveReceived(receivedTimestamp time.Time, aliveGenTimestamp time.Time) error {
	p.lastSystemAliveReceivedTimestamp = &receivedTimestamp
	flaggedDown := p.isFlaggedDown()
	if !flaggedDown {
		err := p.producerManager.SetLastAliveReceivedGenTimestamp(p.producerID, aliveGenTimestamp)
		if err != nil {
			return err
		}
	}

	if p.recoveryState == protocols.StartedRecoveryState {
		p.lastValidAliveGenTimestampInRecovery = aliveGenTimestamp
	}

	return nil
}

func (p *producerRecoveryData) validateSnapshotComplete(recoveryID uint, messageInterest protocols.MessageInterest) bool {
	switch {
	case !p.isPerformingRecovery():
		return false
	case p.currentRecovery != nil && p.currentRecovery.recoveryID != recoveryID:
		return false
	case !p.snapshotValidationNeeded(messageInterest):
		return true
	case p.currentRecovery == nil:
		return false
	}

	res, err := p.validateProducerSnapshotCompletes(p.currentRecovery.snapshotComplete(messageInterest))
	if err != nil {
		return false
	}
	return res
}

func (p *producerRecoveryData) validateEventSnapshotComplete(recoveryID uint, interest protocols.MessageInterest) bool {
	eventRecovery, ok := p.eventRecoveries[recoveryID]
	if !ok {
		return false
	}

	switch {
	case !p.snapshotValidationNeeded(interest):
		return true
	}

	res, err := p.validateProducerSnapshotCompletes(eventRecovery.snapshotComplete(interest))
	if err != nil {
		return false
	}

	return res
}

func (p *producerRecoveryData) isKnownRecovery(requestID uint) bool {
	hasRequestID := p.currentRecovery != nil && p.currentRecovery.recoveryID == requestID
	_, ok := p.eventRecoveries[requestID]
	return hasRequestID || ok
}

func (p *producerRecoveryData) validateProducerSnapshotCompletes(receivedSnapshotCompletes []protocols.MessageInterest) (bool, error) {
	prod, err := p.producerManager.GetProducer(p.producerID)
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
				return false, errors.Errorf("unknown producer scope - %d", scope)
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
	return p.eventRecoveries[recoveryID]
}

func (p *producerRecoveryData) lastRecoveryStartedAt() time.Time {
	switch {
	case p.currentRecovery != nil:
		return p.currentRecovery.recoveryStartedAt
	default:
		return time.Time{}
	}
}

func (p *producerRecoveryData) timestampForRecovery() (time.Time, error) {
	prod, err := p.producerManager.GetProducer(p.producerID)
	if err != nil {
		return time.Time{}, err
	}

	return prod.TimestampForRecovery(), nil
}

func (p *producerRecoveryData) lastMessageReceivedTimestamp() (time.Time, error) {
	prod, err := p.producerManager.GetProducer(p.producerID)
	if err != nil {
		return time.Time{}, err
	}

	return prod.LastMessageTimestamp(), nil
}

func (p *producerRecoveryData) setLastMessageReceivedTimestamp(timestamp time.Time) error {
	if timestamp.IsZero() {
		return errors.New("required non zero timestamp")
	}

	return p.producerManager.SetProducerLastMessageTimestamp(p.producerID, timestamp)
}

func (p *producerRecoveryData) lastProcessedMessageGenTimestamp() (time.Time, error) {
	prod, err := p.producerManager.GetProducer(p.producerID)
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
	prod, err := p.producerManager.GetProducer(p.producerID)
	if err != nil {
		return "", err
	}

	return prod.Name(), nil
}

func (p *producerRecoveryData) statefulRecoveryWindowInMinutes() (uint, error) {
	prod, err := p.producerManager.GetProducer(p.producerID)
	if err != nil {
		return 0, err
	}

	return prod.StatefulRecoveryWindowInMinutes(), nil
}

func (p *producerRecoveryData) setProducerRecoveryState(recoveryID uint, recoveryStatedAt time.Time, recoveryState protocols.RecoveryState) {
	p.recoveryState = recoveryState

	recoveryData := newRecoveryData(recoveryID, recoveryStatedAt)
	p.currentRecovery = &recoveryData
}

func (p *producerRecoveryData) interruptProducerRecovery() {
	p.recoveryState = protocols.InterruptedRecoveryState
}

func (p *producerRecoveryData) setProducerDown(reason protocols.ProducerDownReason) error {
	if err := p.producerManager.SetProducerDown(p.producerID, true); err != nil {
		return err
	}

	p.producerDownReason = reason
	p.eventRecoveries = make(map[uint]*eventRecovery)

	return nil
}

func (p *producerRecoveryData) setProducerUp() error {
	if err := p.producerManager.SetProducerDown(p.producerID, false); err != nil {
		return err
	}
	p.producerDownReason = protocols.DefaultProducerDownReason
	return nil
}

func (p *producerRecoveryData) setEventRecoveryState(eventID protocols.URN, recoveryID uint, recoveryStartedAt time.Time) {
	switch {
	case recoveryID == 0 && recoveryStartedAt.IsZero():
		delete(p.eventRecoveries, recoveryID)
	default:
		eventRecovery := newEventRecovery(eventID, recoveryID, recoveryStartedAt)
		p.eventRecoveries[recoveryID] = &eventRecovery
	}
}

func newProducerRecoveryData(producerID uint, producerManager *producer.Manager) *producerRecoveryData {
	return &producerRecoveryData{
		producerID:      producerID,
		producerManager: producerManager,
		eventRecoveries: make(map[uint]*eventRecovery),
	}
}
