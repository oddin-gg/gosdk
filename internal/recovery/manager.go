package recovery

import (
	"github.com/google/uuid"
	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/producer"
	"github.com/oddin-gg/gosdk/protocols"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"sync"
	"time"
)

const (
	initialDelay = 60 * time.Second
	tickPeriod   = 10 * time.Second
)

// Manager ...
type Manager struct {
	cfg                    protocols.OddsFeedConfiguration
	producerManager        *producer.Manager
	apiClient              *api.Client
	lock                   sync.Mutex
	producerRecoveryData   map[uint]*producerRecoveryData
	logger                 *log.Logger
	ticker                 *time.Ticker
	closeCh                chan bool
	messageProcessingTimes map[uuid.UUID]time.Time
	msgCh                  chan protocols.RecoveryMessage
	sequence               *generator
}

// OnMessageProcessingStarted ...
func (m *Manager) OnMessageProcessingStarted(sessionID uuid.UUID, producerID uint, timestamp time.Time) {
	m.lock.Lock()
	defer m.lock.Unlock()

	m.messageProcessingTimes[sessionID] = timestamp
	data := m.findOrMakeProducerRecoveryData(producerID)
	err := data.setLastMessageReceivedTimestamp(timestamp)
	if err != nil {
		m.logger.WithError(err).Error("failed to set last message received timestamp")
	}
}

// OnMessageProcessingEnded ...
func (m *Manager) OnMessageProcessingEnded(sessionID uuid.UUID, producerID uint, timestamp time.Time) {
	m.lock.Lock()
	defer m.lock.Unlock()

	if !timestamp.IsZero() {
		data := m.findOrMakeProducerRecoveryData(producerID)
		err := data.setLastProcessedMessageGenTimestamp(timestamp)
		if err != nil {
			m.logger.WithError(err).Error("failed to set processed message gen timestamp")
		}
	}

	start, ok := m.messageProcessingTimes[sessionID]
	if !ok {
		start = time.Time{}
	}

	switch {
	case start.IsZero():
		m.logger.Warn("message processing ended, but was not started")
	case time.Now().Sub(start).Milliseconds() > 1000:
		m.logger.Warn("processing message took more than 1s")
	}

	delete(m.messageProcessingTimes, sessionID)
}

// OnAliveReceived ...
func (m *Manager) OnAliveReceived(producerID uint, timestamp protocols.MessageTimestamp, isSubscribed bool, messageInterest protocols.MessageInterest) {
	m.lock.Lock()
	defer m.lock.Unlock()

	data := m.findOrMakeProducerRecoveryData(producerID)
	switch {
	case data.isDisabled():
		return

	case messageInterest == protocols.SystemAliveOnly:
		err := m.systemSessionAliveReceived(timestamp, isSubscribed, data)
		if err != nil {
			m.logger.WithError(err).Error("failed to process alive message")
		}

	default:
		data.lastUserSessionAliveReceivedTimestamp = timestamp.Created
	}
}

// OnSnapshotCompleteReceived ...
func (m *Manager) OnSnapshotCompleteReceived(producerID uint, requestID uint, messageInterest protocols.MessageInterest) {
	m.lock.Lock()
	defer m.lock.Unlock()

	data, ok := m.producerRecoveryData[producerID]
	if !ok {
		return
	}

	switch {
	case data.isDisabled():
		m.logger.Infof("received snapshot recovery complete for disabled producer %d", producerID)

	case !data.isKnownRecovery(requestID):
		m.logger.Infof("unknown snapshot recovery complete received for request %d and producer %d", requestID, producerID)

	case data.validateEventSnapshotComplete(requestID, messageInterest):
		err := m.eventRecoveryFinished(requestID, data)
		if err != nil {
			m.logger.WithError(err).Error("event recovery failed")
		}

	case data.validateSnapshotComplete(requestID, messageInterest):
		err := m.snapshotRecoveryFinished(requestID, data)
		if err != nil {
			m.logger.WithError(err).Error("snapshot recovery finished failed")
		}
	}
}

// InitiateEventOddsMessagesRecovery ...
func (m *Manager) InitiateEventOddsMessagesRecovery(producerID uint, eventID protocols.URN) (uint, error) {
	return m.makeEventRecovery(producerID, eventID, m.apiClient.PostEventOddsRecovery)
}

// InitiateEventStatefulMessagesRecovery ...
func (m *Manager) InitiateEventStatefulMessagesRecovery(producerID uint, eventID protocols.URN) (uint, error) {
	return m.makeEventRecovery(producerID, eventID, m.apiClient.PostEventStatefulRecovery)
}

// Open ...
func (m *Manager) Open() (<-chan protocols.RecoveryMessage, error) {
	if m.msgCh != nil {
		return nil, errors.New("already opened")
	}

	activeProducers, err := m.producerManager.ActiveProducers()
	switch {
	case err != nil:
		return nil, err
	case len(activeProducers) == 0:
		m.logger.Warn("no active producers")
	}

	m.producerRecoveryData = make(map[uint]*producerRecoveryData)
	for id := range activeProducers {
		m.producerRecoveryData[id] = newProducerRecoveryData(id, m.producerManager)
	}

	m.msgCh = make(chan protocols.RecoveryMessage, 0)
	m.closeCh = make(chan bool, 1)
	go func() {
		time.Sleep(initialDelay)

		m.ticker = time.NewTicker(tickPeriod)

		for {
			select {
			case <-m.ticker.C:
				m.timerTick()

			case <-m.closeCh:
				return
			}
		}
	}()

	return m.msgCh, nil
}

// Close ...
func (m *Manager) Close() {
	if m.ticker != nil {
		m.ticker.Stop()
	}

	if m.closeCh != nil {
		m.closeCh <- true
	}

	m.closeCh = nil

	if m.msgCh != nil {
		close(m.msgCh)
	}
}

func (m *Manager) makeEventRecovery(producerID uint, eventID protocols.URN, callback func(string, protocols.URN, uint, *int) (bool, error)) (uint, error) {
	m.lock.Lock()
	defer m.lock.Unlock()

	now := time.Now()
	data := m.findOrMakeProducerRecoveryData(producerID)

	producerName, err := data.producerName()
	if err != nil {
		return 0, err
	}

	requestID := m.sequence.next()

	data.setEventRecoveryState(eventID, requestID, now)
	success, err := callback(producerName, eventID, requestID, m.cfg.SdkNodeID())
	if !success {
		data.eventRecoveryCompleted(requestID)
	}

	if err != nil {
		m.logger.WithError(err).Error("event recovery failed")
		return 0, err
	}

	return requestID, nil
}

func (m *Manager) findOrMakeProducerRecoveryData(producerID uint) *producerRecoveryData {
	data, ok := m.producerRecoveryData[producerID]
	if !ok {
		data = newProducerRecoveryData(producerID, m.producerManager)
		m.producerRecoveryData[producerID] = data
	}

	return data
}

func (m *Manager) timerTick() {
	m.lock.Lock()
	defer m.lock.Unlock()

	now := time.Now()

	for i := range m.producerRecoveryData {
		recoveryData := m.producerRecoveryData[i]
		disabled := recoveryData.isDisabled()
		if disabled {
			continue
		}

		var lastTimestamp time.Time
		if recoveryData.lastSystemAliveReceivedTimestamp != nil {
			lastTimestamp = *recoveryData.lastSystemAliveReceivedTimestamp
		}

		aliveInterval := now.Sub(lastTimestamp)
		var err error
		switch {
		case aliveInterval.Seconds() > float64(m.cfg.MaxInactivitySeconds()):
			err = m.producerDown(recoveryData, protocols.AliveInternalViolationProducerDownReason)
		case !m.calculateTiming(recoveryData, now):
			err = m.producerDown(recoveryData, protocols.ProcessingQueueDelayViolationProducerDownReason)
		}

		if err != nil {
			m.logger.WithError(err).Errorf("failed to check recovery")
		}
	}
}

func (m *Manager) calculateTiming(data *producerRecoveryData, now time.Time) bool {
	maxInactivity := float64(m.cfg.MaxInactivitySeconds())

	lastProcessedMessageGenTimestamp, err := data.lastProcessedMessageGenTimestamp()
	if err != nil {
		m.logger.WithError(err).Warn("failed to get last processed message gen timestamp")
		return false
	}
	lastUserSessionAliveReceivedTimestamp := data.lastUserSessionAliveReceivedTimestamp

	messageProcessingDelay := now.Sub(lastProcessedMessageGenTimestamp)
	userAliveDelay := now.Sub(lastUserSessionAliveReceivedTimestamp)

	return messageProcessingDelay.Seconds() < maxInactivity && userAliveDelay.Seconds() < maxInactivity
}

func (m *Manager) producerDown(data *producerRecoveryData, reason protocols.ProducerDownReason) error {
	if data.isDisabled() {
		return nil
	}

	if data.isFlaggedDown() && data.producerDownReason != reason {
		name, err := data.producerName()
		if err != nil {
			return err
		}

		m.logger.Infof("changing producer %s down reason from %d to %d", name, data.producerDownReason, reason)
		err = data.setProducerDown(reason)
		if err != nil {
			return err
		}
	}

	if data.recoveryState == protocols.StartedRecoveryState && reason != protocols.ProcessingQueueDelayViolationProducerDownReason {
		data.interruptProducerRecovery()
	}

	if !data.isFlaggedDown() {
		err := data.setProducerDown(reason)
		if err != nil {
			return err
		}
	}

	return m.notifyProducerChangedState(data, reason.ToProducerStatusReason())
}

func (m *Manager) notifyProducerChangedState(data *producerRecoveryData, reason protocols.ProducerStatusReason) error {
	if data.producerStatusReason == reason {
		return nil
	}

	data.producerStatusReason = reason

	producerData, err := m.producerManager.GetProducer(data.producerID)
	if err != nil {
		return err
	}

	now := time.Now()
	delayed := !m.calculateTiming(data, now)
	msg := newProducerStatusImpl(
		producerData,
		protocols.MessageTimestamp{
			Created:   now,
			Sent:      now,
			Received:  now,
			Published: now,
		},
		data.isFlaggedDown(),
		delayed,
		reason,
	)

	m.msgCh <- protocols.RecoveryMessage{
		ProducerStatus: msg,
	}

	return nil
}

func (m *Manager) systemSessionAliveReceived(timestamp protocols.MessageTimestamp, subscribed bool, data *producerRecoveryData) error {
	err := data.setLastMessageReceivedTimestamp(timestamp.Received)
	if err != nil {
		return err
	}

	var recoveryTimestamp time.Time
	recoveryTimestamp, err = data.timestampForRecovery()
	if err != nil {
		return err
	}

	if !subscribed {
		if !data.isFlaggedDown() {
			err := m.producerDown(data, protocols.OtherProducerDownReason)
			if err != nil {
				return err
			}
		}

		return m.makeSnapshotRecovery(data, recoveryTimestamp)
	}

	now := time.Now()
	isBackFromInactivity := data.isFlaggedDown() &&
		!data.isPerformingRecovery() &&
		data.producerDownReason == protocols.ProcessingQueueDelayViolationProducerDownReason &&
		m.calculateTiming(data, now)
	isInRecovery := data.recoveryState != protocols.NotStartedRecoveryState &&
		data.recoveryState != protocols.ErrorRecoveryState &&
		data.recoveryState != protocols.InterruptedRecoveryState

	switch {
	case isBackFromInactivity:
		err = m.producerUp(data, protocols.ReturnedFromInactivityProducerUpReason)
	case isInRecovery:

		if data.isFlaggedDown() && !data.isPerformingRecovery() && data.producerDownReason != protocols.ProcessingQueueDelayViolationProducerDownReason {
			err = m.makeSnapshotRecovery(data, recoveryTimestamp)
			if err != nil {
				return err
			}
		}

		recoveryTiming := now.Sub(data.lastRecoveryStartedAt())
		maxInterval := float64(m.cfg.MaxRecoveryExecutionMinutes())
		if data.isPerformingRecovery() && recoveryTiming.Minutes() > maxInterval {
			data.setProducerRecoveryState(0, time.Time{}, protocols.ErrorRecoveryState)
			err = m.makeSnapshotRecovery(data, recoveryTimestamp)
			if err != nil {
				return err
			}
		}
	default:
		err = m.makeSnapshotRecovery(data, recoveryTimestamp)
	}

	if err != nil {
		return err
	}

	return data.systemAliveReceived(timestamp.Received, timestamp.Created)
}

func (m *Manager) snapshotRecoveryFinished(requestID uint, data *producerRecoveryData) error {
	started := data.lastRecoveryStartedAt()
	if started.IsZero() {
		return errors.New("inconsistent recovery state")
	}
	finished := time.Now()
	m.logger.Infof("recovery finished for request %d in %d ms", requestID, finished.Sub(started).Milliseconds())

	if data.recoveryState == protocols.InterruptedRecoveryState {
		err := m.makeSnapshotRecovery(data, data.lastValidAliveGenTimestampInRecovery)
		if err != nil {
			return err
		}
	}

	var reason protocols.ProducerUpReason
	if data.firstRecoveryCompleted {
		reason = protocols.ReturnedFromInactivityProducerUpReason
	} else {
		reason = protocols.FirstRecoveryCompletedProducerUpReason
	}

	if !data.firstRecoveryCompleted {
		data.firstRecoveryCompleted = true
	}

	data.setProducerRecoveryState(requestID, started, protocols.CompletedRecoveryState)
	return m.producerUp(data, reason)
}

func (m *Manager) eventRecoveryFinished(id uint, data *producerRecoveryData) error {
	eventRecovery := data.eventRecovery(id)
	if eventRecovery == nil {
		return errors.New("inconsistent event recovery state")
	}

	started := eventRecovery.recoveryStartedAt
	finished := time.Now()
	m.logger.Infof("event %s recovery finished for request %d in %d ms", eventRecovery.eventID.ToString(), id, finished.Sub(started).Milliseconds())

	producerData, err := m.producerManager.GetProducer(data.producerID)
	if err != nil {
		return err
	}

	m.msgCh <- protocols.RecoveryMessage{
		EventRecoveryMessage: &eventRecoveryMessageImpl{
			eventID:   eventRecovery.eventID,
			requestID: id,
			producer:  producerData,
			timestamp: protocols.MessageTimestamp{
				Created:   finished,
				Sent:      finished,
				Received:  finished,
				Published: finished,
			},
		},
	}

	data.eventRecoveryCompleted(id)
	return nil
}

func (m *Manager) makeSnapshotRecovery(data *producerRecoveryData, timestamp time.Time) error {
	if m.msgCh == nil {
		return nil
	}

	now := time.Now()
	recoverFrom := timestamp
	if !timestamp.IsZero() {
		recoveryTime := now.Sub(recoverFrom)
		if recoveryTime.Minutes() > float64(m.cfg.MaxRecoveryExecutionMinutes()) {
			recoverFrom = now.Add(-time.Duration(m.cfg.MaxRecoveryExecutionMinutes()) * time.Minute)
		}
	}

	requestID := m.sequence.next()
	producerName, err := data.producerName()
	if err != nil {
		return err
	}

	data.setProducerRecoveryState(requestID, now, protocols.StartedRecoveryState)

	m.logger.Infof("recovery started for request %d", requestID)

	success, err := m.apiClient.PostRecovery(
		producerName,
		requestID,
		m.cfg.SdkNodeID(),
		recoverFrom,
	)

	recoveryInfo := newRecoveryInfoImpl(recoverFrom, now, requestID, success, m.cfg.SdkNodeID())
	return m.producerManager.SetProducerRecoveryInfo(data.producerID, recoveryInfo)
}

func (m *Manager) producerUp(data *producerRecoveryData, reason protocols.ProducerUpReason) error {
	if data.isDisabled() {
		return nil
	}

	if data.isFlaggedDown() {
		err := data.setProducerUp()
		if err != nil {
			return err
		}
	}

	return m.notifyProducerChangedState(data, reason.ToProducerStatusReason())
}

// NewManager ...
func NewManager(cfg protocols.OddsFeedConfiguration, producerManager *producer.Manager, apiClient *api.Client, logger *log.Logger) *Manager {
	return &Manager{
		cfg:                    cfg,
		producerManager:        producerManager,
		apiClient:              apiClient,
		logger:                 logger,
		messageProcessingTimes: make(map[uuid.UUID]time.Time),
		sequence:               newGenerator(1),
		producerRecoveryData:   make(map[uint]*producerRecoveryData),
	}
}

type eventRecoveryMessageImpl struct {
	eventID   protocols.URN
	requestID uint
	producer  protocols.Producer
	timestamp protocols.MessageTimestamp
}

func (e eventRecoveryMessageImpl) Producer() protocols.Producer {
	return e.producer
}

func (e eventRecoveryMessageImpl) Timestamp() protocols.MessageTimestamp {
	return e.timestamp
}

func (e eventRecoveryMessageImpl) EventID() protocols.URN {
	return e.eventID
}

func (e eventRecoveryMessageImpl) RequestID() uint {
	return e.requestID
}
