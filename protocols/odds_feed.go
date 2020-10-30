package protocols

// GlobalMessage ...
type GlobalMessage struct {
	APIMessage *Response
	Recovery   *RecoveryMessage
}

// GlobalMessageDelivery ...
type GlobalMessageDelivery <-chan GlobalMessage

// OddsFeed ...
type OddsFeed interface {
	SessionBuilder() (OddsFeedSessionBuilder, error)
	BookmakerDetails() (BookmakerDetail, error)
	ProducerManager() (ProducerManager, error)
	MarketDescriptionManager() (MarketDescriptionManager, error)
	SportsInfoManager() (SportsInfoManager, error)
	RecoveryManager() (RecoveryManager, error)
	ReplayManager() (ReplayManager, error)
	Close() error
	Open() (GlobalMessageDelivery, error)
}
