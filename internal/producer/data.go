package producer

import (
	"time"

	"github.com/oddin-gg/gosdk/internal/api/xml"
	"github.com/oddin-gg/gosdk/protocols"
	errors "github.com/pkg/errors"
)

type data struct {
	id                               uint
	name                             xml.MQSubscriptionTypeName
	description                      string
	active                           bool
	apiEndpoint                      string
	producerScope                    xml.Scope
	statefulRecoveryWindowInMinutes  uint
	lastMessageTimestamp             time.Time
	enabled                          bool
	flaggedDown                      bool
	lastProcessedMessageGenTimestamp time.Time
	lastAliveReceivedGenTimestamp    time.Time
	recoveryFromTimestamp            time.Time
	lastRecoveryInfo                 protocols.RecoveryInfo
}

func newData(producer xml.Producer) *data {
	return &data{
		id:                              producer.ID,
		name:                            producer.Name,
		description:                     producer.Description,
		active:                          producer.Active,
		apiEndpoint:                     producer.APIEndpoint,
		producerScope:                   producer.Scope,
		statefulRecoveryWindowInMinutes: producer.RecoveryWindow,
		enabled:                         producer.Active,
		flaggedDown:                     true,
	}
}

const statefulRecoveryMinutes = 4320

type producerImpl struct {
	id                              uint
	active                          bool
	name                            string
	description                     string
	enabled                         bool
	apiEndpoint                     string
	producerScopes                  []protocols.ProducerScope
	statefulRecoveryWindowInMinutes uint
	producerData                    *data
}

func (p producerImpl) ID() uint {
	return p.id
}

func (p producerImpl) Name() string {
	return p.name
}

func (p producerImpl) Description() string {
	return p.description
}

func (p producerImpl) LastMessageTimestamp() time.Time {
	switch {
	case p.producerData == nil:
		return time.Time{}
	default:
		return p.producerData.lastMessageTimestamp
	}
}

func (p producerImpl) IsAvailable() bool {
	return p.active
}

func (p producerImpl) IsEnabled() bool {
	return p.enabled
}

func (p producerImpl) IsFlaggedDown() bool {
	switch {
	case p.producerData == nil:
		return true
	default:
		return p.producerData.flaggedDown
	}
}

func (p producerImpl) APIEndpoint() string {
	return p.apiEndpoint
}

func (p producerImpl) ProducerScopes() []protocols.ProducerScope {
	return p.producerScopes
}

func (p producerImpl) LastProcessedMessageGenTimestamp() time.Time {
	switch {
	case p.producerData == nil:
		return time.Time{}
	default:
		return p.producerData.lastProcessedMessageGenTimestamp
	}
}

func (p producerImpl) ProcessingQueDelay() time.Duration {
	return time.Since(p.LastProcessedMessageGenTimestamp())
}

func (p producerImpl) TimestampForRecovery() time.Time {
	var timestamp time.Time
	if p.producerData != nil {
		timestamp = p.producerData.lastAliveReceivedGenTimestamp
	}

	switch {
	case timestamp.IsZero() && p.producerData != nil:
		return p.producerData.recoveryFromTimestamp
	default:
		return timestamp
	}
}

func (p producerImpl) StatefulRecoveryWindowInMinutes() uint {
	return p.statefulRecoveryWindowInMinutes
}

func (p producerImpl) RecoveryInfo() *protocols.RecoveryInfo {
	panic("implement me")
}

func buildProducerImpl(producerData *data) (*producerImpl, error) {
	var producerScope protocols.ProducerScope
	switch producerData.producerScope {
	case xml.ScopeLive:
		producerScope = protocols.LiveProducerScope
	case xml.ScopePrematch:
		producerScope = protocols.PrematchProducerScope
	default:
		return nil, errors.Errorf("unknown producer scope %s", producerData.producerScope)
	}

	return &producerImpl{
		id:                              producerData.id,
		active:                          producerData.active,
		name:                            string(producerData.name),
		description:                     producerData.description,
		enabled:                         producerData.enabled,
		apiEndpoint:                     producerData.apiEndpoint,
		producerScopes:                  []protocols.ProducerScope{producerScope},
		statefulRecoveryWindowInMinutes: producerData.statefulRecoveryWindowInMinutes,
		producerData:                    producerData,
	}, nil
}

func buildProducerImplFromUnknown(unknownProducerID uint, cfg protocols.OddsFeedConfiguration) (*producerImpl, error) {
	apiURL, err := cfg.APIURL()
	if err != nil {
		return nil, err
	}
	return &producerImpl{
		id:                              unknownProducerID,
		active:                          true,
		name:                            "unknown",
		description:                     "unknown producer",
		enabled:                         true,
		apiEndpoint:                     apiURL,
		producerScopes:                  []protocols.ProducerScope{protocols.LiveProducerScope, protocols.PrematchProducerScope},
		statefulRecoveryWindowInMinutes: statefulRecoveryMinutes,
	}, nil
}
