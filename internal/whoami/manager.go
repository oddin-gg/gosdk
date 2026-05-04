package whoami

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/types"
)

type bookmakerDetailImpl struct {
	expireAt    time.Time
	bookmakerID uint
	virtualHost string
}

func (b bookmakerDetailImpl) ExpireAt() time.Time {
	return b.expireAt
}

func (b bookmakerDetailImpl) BookmakerID() uint {
	return b.bookmakerID
}

func (b bookmakerDetailImpl) VirtualHost() string {
	return b.virtualHost
}

// Manager ...
type Manager struct {
	mu        sync.Mutex
	cached    types.BookmakerDetail
	cfg       types.OddsFeedConfiguration
	apiClient *api.Client
	logger    *slog.Logger
}

// NewManager ...
func NewManager(cfg types.OddsFeedConfiguration, client *api.Client) types.WhoAmIManager {
	return NewManagerWithLogger(cfg, client, nil)
}

// NewManagerWithLogger constructs a WhoAmIManager with a caller-supplied
// slog.Logger; pass nil for slog.Default().
func NewManagerWithLogger(cfg types.OddsFeedConfiguration, client *api.Client, logger *slog.Logger) types.WhoAmIManager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		cfg:       cfg,
		apiClient: client,
		logger:    logger,
	}
}

// BookmakerDetails returns cached bookmaker details, fetching once on first call.
//
// Concurrent callers serialize on the manager's mutex so that only one fetch
// is in flight; later callers re-use the cached result.
func (m *Manager) BookmakerDetails(ctx context.Context) (types.BookmakerDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cached != nil {
		return m.cached, nil
	}

	details, err := m.apiClient.FetchWhoAmI(ctx)
	if err != nil {
		return nil, err
	}

	exp := time.Time(details.ExpireAt)
	// Warn when the token expires within the next week.
	if time.Until(exp) < 7*24*time.Hour {
		m.logger.Warn("api: access token expires soon", slog.Time("expire_at", exp))
	}

	m.cached = bookmakerDetailImpl{
		expireAt:    exp,
		bookmakerID: details.BookmakerID,
		virtualHost: details.VirtualHost,
	}
	return m.cached, nil
}
