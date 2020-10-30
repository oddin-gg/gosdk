package replay

import (
	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/protocols"
)

// Manager ...
type Manager struct {
	apiClient             *api.Client
	oddsFeedConfiguration protocols.OddsFeedConfiguration
	sportsInfoManager     protocols.SportsInfoManager
}

// ReplayList ...
func (m *Manager) ReplayList() ([]protocols.SportEvent, error) {
	events, err := m.apiClient.FetchReplaySetContent(m.oddsFeedConfiguration.SdkNodeID())
	if err != nil {
		return nil, err
	}

	result := make([]protocols.SportEvent, len(events))
	for i, event := range events {
		id, err := protocols.ParseURN(event.ID)
		if err != nil {
			return nil, err
		}
		match, err := m.sportsInfoManager.Match(*id)
		if err != nil {
			return nil, err
		}
		result[i] = match
	}

	return result, nil
}

// AddSportEvent ...
func (m *Manager) AddSportEvent(event protocols.SportEvent) (bool, error) {
	return m.AddSportEventID(event.ID())
}

// AddSportEventID ...
func (m *Manager) AddSportEventID(id protocols.URN) (bool, error) {
	return m.apiClient.PutReplayEvent(id, m.oddsFeedConfiguration.SdkNodeID())
}

// RemoveSportEvent ...
func (m *Manager) RemoveSportEvent(event protocols.SportEvent) (bool, error) {
	return m.RemoveSportEventID(event.ID())
}

// RemoveSportEventID ...
func (m *Manager) RemoveSportEventID(id protocols.URN) (bool, error) {
	return m.apiClient.DeleteReplayEvent(id, m.oddsFeedConfiguration.SdkNodeID())
}

// Play ...
func (m *Manager) Play(params protocols.ReplayPlayParams) (bool, error) {
	return m.apiClient.PostReplayStart(
		m.oddsFeedConfiguration.SdkNodeID(),
		params.Speed,
		params.MaxDelayInMs,
		params.RewriteTimestamps,
		params.Producer,
		params.RunParallel,
	)
}

// Stop ...
func (m *Manager) Stop() (bool, error) {
	return m.apiClient.PostReplayStop(m.oddsFeedConfiguration.SdkNodeID())
}

// Clear ...
func (m *Manager) Clear() (bool, error) {
	return m.apiClient.PostReplayClear(m.oddsFeedConfiguration.SdkNodeID())
}

// NewManager ...
func NewManager(apiClient *api.Client, oddsFeedConfiguration protocols.OddsFeedConfiguration, sportsInfoManager protocols.SportsInfoManager) protocols.ReplayManager {
	return &Manager{
		apiClient:             apiClient,
		oddsFeedConfiguration: oddsFeedConfiguration,
		sportsInfoManager:     sportsInfoManager,
	}
}
