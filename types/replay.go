package types

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
//
// Phase 6 reshape: ReplayList returns Match values directly (the
// previous SportEvent interface is gone — replay queues are populated
// from match URNs).
type ReplayManager interface {
	ReplayList(ctx context.Context) ([]Match, error)

	AddSportEventID(ctx context.Context, id URN) (bool, error)
	RemoveSportEventID(ctx context.Context, id URN) (bool, error)

	Play(ctx context.Context, params ReplayPlayParams) (bool, error)

	Stop(ctx context.Context) (bool, error)
	Clear(ctx context.Context) (bool, error)
}
