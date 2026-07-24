package types

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

// IsKnown reports whether m is one of the documented MessageInterest
// constants above. MessageInterest values are interpolated VERBATIM
// into RabbitMQ topic bindings, so an arbitrary string is dangerous in
// three distinct ways: a typo creates a valid subscription that
// silently receives nothing intended; a wildcard like "#" broadens the
// binding beyond the SDK's documented routing shapes; and unknown
// values default-accept every producer scope in IsProducerInScope,
// bypassing live/prematch filtering. Subscribe rejects unknown values
// via this check.
func (m MessageInterest) IsKnown() bool {
	switch m {
	case LiveOnlyMessageInterest,
		PrematchOnlyMessageInterest,
		HiPriorityOnlyMessageInterest,
		LowPriorityOnlyMessageInterest,
		SpecifiedMatchesOnlyMessageInterest,
		AllMessageInterest,
		SystemAliveOnly:
		return true
	}
	return false
}

// PossibleSourceProducers ...
func (m MessageInterest) PossibleSourceProducers(availableProducers map[int]Producer) []int {
	var possibleProducers []int

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

// IsProducerInScope reports whether the given producer satisfies the
// scope constraints implied by this MessageInterest.
//
// Defensive nil guard: a nil Producer interface (e.g., from a future
// failure path that forgot to early-return) makes this function
// fail-closed (returns false) instead of panicking on the
// ProducerScopes() dispatch. The session-level pre-filter already
// returns early on producer-lookup failure (v2.31), so nil should
// never reach here in practice.
func (m MessageInterest) IsProducerInScope(producer Producer) bool {
	if producer == nil {
		return false
	}
	switch m {
	case LiveOnlyMessageInterest:
		return ProducerHasScope(producer, LiveProducerScope)
	case PrematchOnlyMessageInterest:
		return ProducerHasScope(producer, PrematchProducerScope)
	default:
		return true
	}
}

func (m MessageInterest) findProducerIDByScope(producers map[int]Producer, scope ProducerScope) []int {
	// Each producer is a unique map entry and ProducerHasScope is a
	// boolean membership check, so a producer contributes its ID at most
	// once — no dedup set needed (the earlier per-scope inner loop did).
	result := make([]int, 0, len(producers))
	for _, producer := range producers {
		if ProducerHasScope(producer, scope) {
			result = append(result, producer.ID())
		}
	}

	return result
}
