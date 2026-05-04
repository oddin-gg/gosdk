package replay

import (
	"context"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/protocols"
)

// Manager ...
type Manager struct {
	apiClient             *api.Client
	oddsFeedConfiguration protocols.OddsFeedConfiguration
	sportsInfoManager     protocols.SportsInfoManager
}

// ReplayList returns the queued replay events as Match value snapshots.
func (m *Manager) ReplayList(ctx context.Context) ([]protocols.Match, error) {
	events, err := m.apiClient.FetchReplaySetContent(ctx, m.oddsFeedConfiguration.SdkNodeID())
	if err != nil {
		return nil, err
	}

	result := make([]protocols.Match, 0, len(events))
	for _, event := range events {
		id, err := protocols.ParseURN(event.ID)
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

// AddSportEventID ...
func (m *Manager) AddSportEventID(ctx context.Context, id protocols.URN) (bool, error) {
	return m.apiClient.PutReplayEvent(ctx, id, m.oddsFeedConfiguration.SdkNodeID())
}

// RemoveSportEventID ...
func (m *Manager) RemoveSportEventID(ctx context.Context, id protocols.URN) (bool, error) {
	return m.apiClient.DeleteReplayEvent(ctx, id, m.oddsFeedConfiguration.SdkNodeID())
}

// Play ...
func (m *Manager) Play(ctx context.Context, params protocols.ReplayPlayParams) (bool, error) {
	return m.apiClient.PostReplayStart(ctx,
		m.oddsFeedConfiguration.SdkNodeID(),
		params.Speed,
		params.MaxDelayInMs,
		params.RewriteTimestamps,
		params.Producer,
		params.RunParallel,
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

// NewManager ...
func NewManager(apiClient *api.Client, oddsFeedConfiguration protocols.OddsFeedConfiguration, sportsInfoManager protocols.SportsInfoManager) protocols.ReplayManager {
	return &Manager{
		apiClient:             apiClient,
		oddsFeedConfiguration: oddsFeedConfiguration,
		sportsInfoManager:     sportsInfoManager,
	}
}
