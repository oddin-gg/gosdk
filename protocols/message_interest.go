package protocols

// MessageInterest ...
type MessageInterest string

// MessageInterests
const (
	LiveOnlyMessageInterest             MessageInterest = "*.*.live.*.*.*.*"
	PrematchOnlyMessageInterest         MessageInterest = "*.pre.*.*.*.*.*"
	HiPriorityOnlyMessageInterest       MessageInterest = "hi.*.*.*.*.*.*"
	LowPriorityOnlyMessageInterest      MessageInterest = "lo.*.*.*.*.*.*"
	SpecifiedMatchesOnlyMessageInterest MessageInterest = ""
	AllMessageInterest                  MessageInterest = "*.*.*.*.*.*.*"
	SystemAliveOnly                     MessageInterest = "-.-.-.alive.#"
)

// PossibleSourceProducers ...
func (m MessageInterest) PossibleSourceProducers(availableProducers map[uint]Producer) []uint {
	var possibleProducers []uint

	switch m {
	case LiveOnlyMessageInterest:
		possibleProducers = m.findProducerIDByScope(availableProducers, LiveProducerScope)
	case PrematchOnlyMessageInterest:
		possibleProducers = m.findProducerIDByScope(availableProducers, PrematchProducerScope)
	default:
		for id := range availableProducers {
			possibleProducers = append(possibleProducers, id)
		}
	}

	return possibleProducers
}

// IsProducerInScope ...
func (m MessageInterest) IsProducerInScope(producer Producer) bool {
	switch m {
	case LiveOnlyMessageInterest:
		return m.isProducerInScope(producer, LiveProducerScope)
	case PrematchOnlyMessageInterest:
		return m.isProducerInScope(producer, PrematchProducerScope)
	default:
		return true
	}
}

func (m MessageInterest) isProducerInScope(producer Producer, scope ProducerScope) bool {
	for _, pScope := range producer.ProducerScopes() {
		if pScope == scope {
			return true
		}
	}

	return false
}

func (m MessageInterest) findProducerIDByScope(producers map[uint]Producer, scope ProducerScope) []uint {
	possibleProducers := make(map[uint]struct{}, 0)

	for _, producer := range producers {
		for _, pScope := range producer.ProducerScopes() {
			if pScope == scope {
				possibleProducers[producer.ID()] = struct{}{}
			}
		}
	}

	result := make([]uint, len(possibleProducers))
	for id := range possibleProducers {
		result = append(result, id)
	}

	return result
}
