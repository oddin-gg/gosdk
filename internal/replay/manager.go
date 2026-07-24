package replay

import (
	"context"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/config"
	"github.com/oddin-gg/gosdk/types"
)

// SportsInfoLookup is the narrow surface this package needs from
// gosdk's sportsInfoManager — only the Match resolver, used by
// ReplayList. Defined locally because internal/replay can't import
// the gosdk root package (cycle); a concrete *sport.Manager
// satisfies it structurally.
type SportsInfoLookup interface {
	Match(ctx context.Context, id types.URN) (types.Match, error)
}

// Manager ...
type Manager struct {
	apiClient             *api.Client
	oddsFeedConfiguration config.Config
	sportsInfoManager     SportsInfoLookup
}

// ReplayList returns the queued replay events as Match value snapshots.
func (m *Manager) ReplayList(ctx context.Context) ([]types.Match, error) {
	events, err := m.apiClient.FetchReplaySetContent(ctx, m.oddsFeedConfiguration.SdkNodeID())
	if err != nil {
		return nil, err
	}

	result := make([]types.Match, 0, len(events))
	for _, event := range events {
		id, err := types.ParseURN(event.ID)
		if err != nil {
			return nil, err
		}
		match, err := m.sportsInfoManager.Match(ctx, *id)
		if err != nil {
			return nil, err
		}
		result = append(result, match)
	}

	return result, nil
}

// ReplayEventIDs returns the queued event URNs without the per-event
// sports-info Match lookup ReplayList does. Mirrors .NET's
// IReplayManager.GetEventsInQueue.
func (m *Manager) ReplayEventIDs(ctx context.Context) ([]types.URN, error) {
	events, err := m.apiClient.FetchReplaySetContent(ctx, m.oddsFeedConfiguration.SdkNodeID())
	if err != nil {
		return nil, err
	}
	result := make([]types.URN, 0, len(events))
	for _, event := range events {
		id, err := types.ParseURN(event.ID)
		if err != nil {
			return nil, err
		}
		result = append(result, *id)
	}
	return result, nil
}

// AddSportEventID ...
func (m *Manager) AddSportEventID(ctx context.Context, id types.URN) (bool, error) {
	return m.apiClient.PutReplayEvent(ctx, id, m.oddsFeedConfiguration.SdkNodeID())
}

// RemoveSportEventID ...
func (m *Manager) RemoveSportEventID(ctx context.Context, id types.URN) (bool, error) {
	return m.apiClient.DeleteReplayEvent(ctx, id, m.oddsFeedConfiguration.SdkNodeID())
}

// Play ...
//
// v2.29: ReplayPlayParams fields are Optional[T]; the api.PostReplayStart
// signature is unchanged (*int / *bool / *string for HTTP query-string
// assembly). Bridge via Optional.Ptr() — None → nil (omits the
// query-string entry), Some → a fresh pointer per call.
func (m *Manager) Play(ctx context.Context, params types.ReplayPlayParams) (bool, error) {
	return m.apiClient.PostReplayStart(ctx,
		m.oddsFeedConfiguration.SdkNodeID(),
		params.Speed.Ptr(),
		params.MaxDelayInMs.Ptr(),
		params.RewriteTimestamps.Ptr(),
		params.Producer.Ptr(),
		params.RunParallel.Ptr(),
	)
}

// Stop ...
func (m *Manager) Stop(ctx context.Context) (bool, error) {
	return m.apiClient.PostReplayStop(ctx, m.oddsFeedConfiguration.SdkNodeID())
}

// Clear ...
func (m *Manager) Clear(ctx context.Context) (bool, error) {
	return m.apiClient.PostReplayClear(ctx, m.oddsFeedConfiguration.SdkNodeID())
}

// Status ...
func (m *Manager) Status(ctx context.Context) (string, error) {
	return m.apiClient.FetchReplayStatus(ctx, m.oddsFeedConfiguration.SdkNodeID())
}

// NewManager constructs a *Manager. The concrete return type
// satisfies gosdk's replayManager interface structurally —
// internal/replay can't import gosdk (cycle).
func NewManager(apiClient *api.Client, oddsFeedConfiguration config.Config, sportsInfoManager SportsInfoLookup) *Manager {
	return &Manager{
		apiClient:             apiClient,
		oddsFeedConfiguration: oddsFeedConfiguration,
		sportsInfoManager:     sportsInfoManager,
	}
}
