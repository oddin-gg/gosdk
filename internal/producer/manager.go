package producer

import (
	"errors"
	"fmt"
	"time"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/protocols"
	log "github.com/sirupsen/logrus"
)

// Manager ...
type Manager struct {
	apiClient   *api.Client
	cfg         protocols.OddsFeedConfiguration
	logger      *log.Entry
	producerMap map[uint]*data
}

func (m *Manager) producers() (map[uint]*data, error) {
	if m.producerMap != nil {
		return m.producerMap, nil
	}

	if err := m.Open(); err != nil {
		return nil, err
	}

	return m.producerMap, nil
}

func (m *Manager) producer(id uint) (*data, error) {
	producers, err := m.producers()
	if err != nil {
		return nil, err
	}

	producer, ok := producers[id]
	if !ok {
		return nil, fmt.Errorf("missing producer %d", id)
	}

	return producer, nil
}

// Open ...
func (m *Manager) Open() error {
	apiProducers, err := m.apiClient.FetchProducers()
	if err != nil {
		return err
	}

	m.logger.Debugf("fetched producer list - size %d", len(apiProducers))

	m.producerMap = make(map[uint]*data, len(apiProducers))
	for i := range apiProducers {
		p := apiProducers[i]
		m.producerMap[p.ID] = newData(p)
	}

	m.logger.Debugf("mapped producer list - %v", apiProducers)
	return nil
}

// SetProducerDown ...
func (m *Manager) SetProducerDown(id uint, flaggedDown bool) error {
	producer, err := m.producer(id)
	if err != nil {
		return err
	}

	producer.flaggedDown = flaggedDown
	return nil
}

// SetProducerLastMessageTimestamp ...
func (m *Manager) SetProducerLastMessageTimestamp(id uint, timestamp time.Time) error {
	if timestamp.IsZero() {
		return errors.New("required non zero timestamp")
	}
	producer, err := m.producer(id)
	if err != nil {
		return err
	}

	producer.lastMessageTimestamp = timestamp
	return nil
}

// SetLastProcessedMessageGenTimestamp ...
func (m *Manager) SetLastProcessedMessageGenTimestamp(id uint, timestamp time.Time) error {
	producer, err := m.producer(id)
	if err != nil {
		return err
	}

	producer.lastProcessedMessageGenTimestamp = timestamp
	return nil
}

// SetLastAliveReceivedGenTimestamp ...
func (m *Manager) SetLastAliveReceivedGenTimestamp(id uint, timestamp time.Time) error {
	producer, err := m.producer(id)
	if err != nil {
		return err
	}

	producer.lastAliveReceivedGenTimestamp = timestamp
	return nil
}

// SetProducerRecoveryInfo ...
func (m *Manager) SetProducerRecoveryInfo(id uint, recoveryInfo protocols.RecoveryInfo) error {
	producer, err := m.producer(id)
	if err != nil {
		return err
	}

	producer.lastRecoveryInfo = recoveryInfo
	return nil
}

// AvailableProducers ...
func (m *Manager) AvailableProducers() (map[uint]protocols.Producer, error) {
	producers, err := m.producers()
	if err != nil {
		return nil, err
	}

	res := make(map[uint]protocols.Producer, len(producers))
	for i := range producers {
		data := producers[i]
		res[i], err = buildProducerImpl(data)
		if err != nil {
			return nil, err
		}
	}

	return res, nil
}

// ActiveProducers ...
func (m *Manager) ActiveProducers() (map[uint]protocols.Producer, error) {
	producers, err := m.producers()
	if err != nil {
		return nil, err
	}

	res := make(map[uint]protocols.Producer, len(producers))
	for i := range producers {
		data := producers[i]
		if !data.active {
			continue
		}

		res[i], err = buildProducerImpl(data)
		if err != nil {
			return nil, err
		}
	}

	return res, nil
}

// ActiveProducersInScope ...
func (m *Manager) ActiveProducersInScope(scope protocols.ProducerScope) (map[uint]protocols.Producer, error) {
	producers, err := m.producers()
	if err != nil {
		return nil, err
	}

	res := make(map[uint]protocols.Producer, len(producers))
	for i := range producers {
		data := producers[i]
		if !data.active {
			continue
		}

		p, err := buildProducerImpl(data)
		if err != nil {
			return nil, err
		}

		inScope := false
		for _, s := range p.producerScopes {
			if s == scope {
				inScope = true
				break
			}
		}
		if inScope {
			res[i] = p
		}
	}
	return res, nil
}

// GetProducer ...
func (m *Manager) GetProducer(id uint) (protocols.Producer, error) {
	producers, err := m.producers()
	if err != nil {
		return nil, err
	}

	producer, ok := producers[id]
	if !ok {
		return buildProducerImplFromUnknown(id, m.cfg)
	}

	return buildProducerImpl(producer)
}

// SetProducerState ...
func (m *Manager) SetProducerState(id uint, enabled bool) error {
	producer, err := m.producer(id)
	if err != nil {
		return err
	}

	producer.enabled = enabled
	return nil
}

// SetProducerRecoveryFromTimestamp ...
func (m *Manager) SetProducerRecoveryFromTimestamp(id uint, timestamp time.Time) error {
	producer, err := m.producer(id)
	if err != nil {
		return err
	}

	maxRequestMinutes := producer.statefulRecoveryWindowInMinutes
	switch {
	case timestamp.IsZero():
		break
	case time.Since(timestamp) > (time.Duration(maxRequestMinutes) * time.Minute):
		return errors.New("last received message timestamp can not be so long in past")
	}

	producer.recoveryFromTimestamp = timestamp
	return nil
}

// IsProducerEnabled ...
func (m *Manager) IsProducerEnabled(id uint) (bool, error) {
	producer, err := m.producer(id)
	if err != nil {
		return false, err
	}

	return producer.enabled, nil
}

// IsProducerDown ...
func (m *Manager) IsProducerDown(id uint) (bool, error) {
	producer, err := m.producer(id)
	if err != nil {
		return false, err
	}

	return producer.flaggedDown, nil
}

// NewManager ...
func NewManager(cfg protocols.OddsFeedConfiguration, apiClient *api.Client, logger *log.Entry) *Manager {
	return &Manager{
		apiClient: apiClient,
		cfg:       cfg,
		logger:    logger,
	}
}
