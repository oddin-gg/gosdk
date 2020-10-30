package whoami

import (
	"errors"
	"fmt"
	log "github.com/sirupsen/logrus"
	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/protocols"
	"time"
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
	bookmakerDetails protocols.BookmakerDetail
	cfg              protocols.OddsFeedConfiguration
	apiClient        *api.Client
	logger           *log.Logger
}

// NewManager ...
func NewManager(cfg protocols.OddsFeedConfiguration, client *api.Client, logger *log.Logger) protocols.WhoAmIManager {
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

func (m *Manager) bookmakerDescription() (*string, error) {
	if m.bookmakerDetails == nil {
		return nil, errors.New("missing bookmaker detail")
	}

	var sdkNodeID int
	if m.cfg.SdkNodeID() == nil {
		sdkNodeID = -1
	} else {
		sdkNodeID = *m.cfg.SdkNodeID()
	}

	output := fmt.Sprintf("of-sdk-%d-%d", m.bookmakerDetails.BookmakerID(), sdkNodeID)
	return &output, nil
}

func (m *Manager) fetchBookmakerDetails() (protocols.BookmakerDetail, error) {
	details, err := m.apiClient.FetchWhoAmI()
	if err != nil {
		return nil, err
	}

	m.logger.Infof("client id: %d", details.BookmakerID)

	exp := time.Time(details.ExpireAt)
	if exp.After(exp.Add(7 * 24 * time.Hour)) {
		m.logger.Warn("access token will expire soon")
	}

	return bookmakerDetailImpl{
		expireAt:    exp,
		bookmakerID: details.BookmakerID,
		virtualHost: details.VirtualHost,
	}, nil
}
