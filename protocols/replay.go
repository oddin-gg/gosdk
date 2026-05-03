package protocols

import "context"

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
	ReplayList(ctx context.Context) ([]SportEvent, error)

	AddSportEvent(ctx context.Context, event SportEvent) (bool, error)
	AddSportEventID(ctx context.Context, id URN) (bool, error)

	RemoveSportEvent(ctx context.Context, event SportEvent) (bool, error)
	RemoveSportEventID(ctx context.Context, id URN) (bool, error)

	Play(ctx context.Context, params ReplayPlayParams) (bool, error)

	Stop(ctx context.Context) (bool, error)
	Clear(ctx context.Context) (bool, error)
}
