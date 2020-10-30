package protocols

// ReplayPlayParams ...
type ReplayPlayParams struct {
	Speed             *int
	MaxDelayInMs      *int
	RunParallel       *bool
	RewriteTimestamps *bool
	Producer          *string
}

// ReplayManager ...
type ReplayManager interface {
	ReplayList() ([]SportEvent, error)

	AddSportEvent(event SportEvent) (bool, error)
	AddSportEventID(id URN) (bool, error)

	RemoveSportEvent(event SportEvent) (bool, error)
	RemoveSportEventID(id URN) (bool, error)

	Play(params ReplayPlayParams) (bool, error)

	Stop() (bool, error)
	Clear() (bool, error)
}
