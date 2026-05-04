package recovery

import (
	"github.com/oddin-gg/gosdk/types"
)

type producerStatusImpl struct {
	producer             types.Producer
	timestamp            types.MessageTimestamp
	isDown               bool
	isDelayed            bool
	producerStatusReason types.ProducerStatusReason
}

func (p producerStatusImpl) Producer() types.Producer {
	return p.producer
}

func (p producerStatusImpl) Timestamp() types.MessageTimestamp {
	return p.timestamp
}

func (p producerStatusImpl) IsDown() bool {
	return p.isDown
}

func (p producerStatusImpl) IsDelayed() bool {
	return p.isDelayed
}

func (p producerStatusImpl) ProducerStatusReason() types.ProducerStatusReason {
	return p.producerStatusReason
}

func newProducerStatusImpl(
	producer types.Producer,
	timestamp types.MessageTimestamp,
	isDown bool,
	isDelayed bool,
	producerStatusReason types.ProducerStatusReason) types.ProducerStatus {
	return &producerStatusImpl{
		producer:             producer,
		timestamp:            timestamp,
		isDown:               isDown,
		isDelayed:            isDelayed,
		producerStatusReason: producerStatusReason,
	}
}
