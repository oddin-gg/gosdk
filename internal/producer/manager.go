package producer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/protocols"
	log "github.com/sirupsen/logrus"
)

// Manager ...
//
// NOTE: this implementation has known concurrency issues (mutable producer
// fields read/written across goroutines without locking). Those are addressed
// by the Phase 5 recovery-state-machine rewrite, which moves all per-producer
// state into a single owning goroutine. For Phase 2, only the public interface
// is reshaped (ctx-aware methods).
type Manager struct {
	apiClient   *api.Client
	cfg         protocols.OddsFeedConfiguration
	logger      *log.Entry
	mu          sync.RWMutex
	producerMap map[uint]*data
}

func (m *Manager) producers(ctx context.Context) (map[uint]*data, error) {
	m.mu.RLock()
	if m.producerMap != nil {
		defer m.mu.RUnlock()
		return m.producerMap, nil
	}
	m.mu.RUnlock()

	if err := m.Open(ctx); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.producerMap, nil
}

func (m *Manager) producer(ctx context.Context, id uint) (*data, error) {
	producers, err := m.producers(ctx)
	if err != nil {
		return nil, err
	}
	producer, ok := producers[id]
	if !ok {
		return nil, fmt.Errorf("missing producer %d", id)
	}
	return producer, nil
}

// Open fetches the producer list from the API and populates the in-memory map.
// Safe to call multiple times; subsequent calls re-fetch and overwrite.
func (m *Manager) Open(ctx context.Context) error {
	apiProducers, err := m.apiClient.FetchProducers(ctx)
	if err != nil {
		return err
	}

	m.logger.Debugf("fetched producer list - size %d", len(apiProducers))

	pm := make(map[uint]*data, len(apiProducers))
	for i := range apiProducers {
		p := apiProducers[i]
		pm[p.ID] = newData(p)
	}

	m.mu.Lock()
	m.producerMap = pm
	m.mu.Unlock()

	m.logger.Debugf("mapped producer list - %v", apiProducers)
	return nil
}

// SetProducerDown ...
func (m *Manager) SetProducerDown(id uint, flaggedDown bool) error {
	producer, err := m.producer(context.Background(), id)
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
	producer, err := m.producer(context.Background(), id)
	if err != nil {
		return err
	}
	producer.lastMessageTimestamp = timestamp
	return nil
}

// SetLastProcessedMessageGenTimestamp ...
func (m *Manager) SetLastProcessedMessageGenTimestamp(id uint, timestamp time.Time) error {
	producer, err := m.producer(context.Background(), id)
	if err != nil {
		return err
	}
	producer.lastProcessedMessageGenTimestamp = timestamp
	return nil
}

// SetLastAliveReceivedGenTimestamp ...
func (m *Manager) SetLastAliveReceivedGenTimestamp(id uint, timestamp time.Time) error {
	producer, err := m.producer(context.Background(), id)
	if err != nil {
		return err
	}
	producer.lastAliveReceivedGenTimestamp = timestamp
	return nil
}

// SetProducerRecoveryInfo ...
func (m *Manager) SetProducerRecoveryInfo(id uint, recoveryInfo protocols.RecoveryInfo) error {
	producer, err := m.producer(context.Background(), id)
	if err != nil {
		return err
	}
	producer.lastRecoveryInfo = recoveryInfo
	return nil
}

// AvailableProducers ...
func (m *Manager) AvailableProducers(ctx context.Context) (map[uint]protocols.Producer, error) {
	producers, err := m.producers(ctx)
	if err != nil {
		return nil, err
	}
	res := make(map[uint]protocols.Producer, len(producers))
	for i := range producers {
		d := producers[i]
		res[i], err = buildProducerImpl(d)
		if err != nil {
			return nil, err
		}
	}
	return res, nil
}

// ActiveProducers ...
func (m *Manager) ActiveProducers(ctx context.Context) (map[uint]protocols.Producer, error) {
	producers, err := m.producers(ctx)
	if err != nil {
		return nil, err
	}
	res := make(map[uint]protocols.Producer, len(producers))
	for i := range producers {
		d := producers[i]
		if !d.active {
			continue
		}
		res[i], err = buildProducerImpl(d)
		if err != nil {
			return nil, err
		}
	}
	return res, nil
}

// ActiveProducersInScope ...
func (m *Manager) ActiveProducersInScope(ctx context.Context, scope protocols.ProducerScope) (map[uint]protocols.Producer, error) {
	producers, err := m.producers(ctx)
	if err != nil {
		return nil, err
	}
	res := make(map[uint]protocols.Producer, len(producers))
	for i := range producers {
		d := producers[i]
		if !d.active {
			continue
		}
		p, err := buildProducerImpl(d)
		if err != nil {
			return nil, err
		}
		for _, s := range p.producerScopes {
			if s == scope {
				res[i] = p
				break
			}
		}
	}
	return res, nil
}

// GetProducer ...
func (m *Manager) GetProducer(ctx context.Context, id uint) (protocols.Producer, error) {
	producers, err := m.producers(ctx)
	if err != nil {
		return nil, err
	}
	p, ok := producers[id]
	if !ok {
		return buildProducerImplFromUnknown(id, m.cfg)
	}
	return buildProducerImpl(p)
}

// SetProducerState ...
func (m *Manager) SetProducerState(ctx context.Context, id uint, enabled bool) error {
	producer, err := m.producer(ctx, id)
	if err != nil {
		return err
	}
	producer.enabled = enabled
	return nil
}

// SetProducerRecoveryFromTimestamp ...
func (m *Manager) SetProducerRecoveryFromTimestamp(ctx context.Context, id uint, timestamp time.Time) error {
	producer, err := m.producer(ctx, id)
	if err != nil {
		return err
	}
	maxRequestMinutes := producer.statefulRecoveryWindowInMinutes
	switch {
	case timestamp.IsZero():
		break
	case time.Since(timestamp).Minutes() > float64(maxRequestMinutes):
		return errors.New("last received message timestamp can not be so long in past")
	}
	producer.recoveryFromTimestamp = timestamp
	return nil
}

// IsProducerEnabled ...
func (m *Manager) IsProducerEnabled(ctx context.Context, id uint) (bool, error) {
	producer, err := m.producer(ctx, id)
	if err != nil {
		return false, err
	}
	return producer.enabled, nil
}

// IsProducerDown ...
func (m *Manager) IsProducerDown(ctx context.Context, id uint) (bool, error) {
	producer, err := m.producer(ctx, id)
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
