package whoami

import (
	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/protocols"
	log "github.com/sirupsen/logrus"
	"time"
)

const tokenExpiryWarningWindow = 7 * 24 * time.Hour

func tokenExpiringSoon(exp, now time.Time) bool {
	return exp.Sub(now) < tokenExpiryWarningWindow
}

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
	bookmakerDetails protocols.BookmakerDetail
	cfg              protocols.OddsFeedConfiguration
	apiClient        *api.Client
	logger           *log.Entry
}

// NewManager ...
func NewManager(cfg protocols.OddsFeedConfiguration, client *api.Client, logger *log.Entry) protocols.WhoAmIManager {
	return &Manager{
		cfg:       cfg,
		apiClient: client,
		logger:    logger,
	}
}

// BookmakerDetails ...
func (m *Manager) BookmakerDetails() (protocols.BookmakerDetail, error) {
	if m.bookmakerDetails != nil {
		return m.bookmakerDetails, nil
	}

	var err error
	m.bookmakerDetails, err = m.fetchBookmakerDetails()

	return m.bookmakerDetails, err
}

func (m *Manager) fetchBookmakerDetails() (protocols.BookmakerDetail, error) {
	details, err := m.apiClient.FetchWhoAmI()
	if err != nil {
		return nil, err
	}

	exp := time.Time(details.ExpireAt)
	if tokenExpiringSoon(exp, time.Now()) {
		m.logger.Warn("access token will expire soon")
	}

	return bookmakerDetailImpl{
		expireAt:    exp,
		bookmakerID: details.BookmakerID,
		virtualHost: details.VirtualHost,
	}, nil
}
