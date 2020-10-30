package recovery

import (
	"github.com/oddin-gg/gosdk/protocols"
)

type producerStatusImpl struct {
	producer             protocols.Producer
	timestamp            protocols.MessageTimestamp
	isDown               bool
	isDelayed            bool
	producerStatusReason protocols.ProducerStatusReason
}

func (p producerStatusImpl) Producer() protocols.Producer {
	return p.producer
}

func (p producerStatusImpl) Timestamp() protocols.MessageTimestamp {
	return p.timestamp
}

func (p producerStatusImpl) IsDown() bool {
	return p.isDown
}

func (p producerStatusImpl) IsDelayed() bool {
	return p.isDelayed
}

func (p producerStatusImpl) ProducerStatusReason() protocols.ProducerStatusReason {
	return p.producerStatusReason
}

func newProducerStatusImpl(
	producer protocols.Producer,
	timestamp protocols.MessageTimestamp,
	isDown bool,
	isDelayed bool,
	producerStatusReason protocols.ProducerStatusReason) protocols.ProducerStatus {
	return &producerStatusImpl{
		producer:             producer,
		timestamp:            timestamp,
		isDown:               isDown,
		isDelayed:            isDelayed,
		producerStatusReason: producerStatusReason,
	}
}
