package main

import (
	"errors"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/oddin-gg/gosdk"
	"github.com/oddin-gg/gosdk/protocols"
)

// Demo constants
const (
	token  = "YOUR TOKEN"
	env    = protocols.IntegrationEnvironment
	nodeID = 1
	locale = protocols.EnLocale
)

// Sample demo working with Oddin.gg Api and Feed
func main() {

	example, err := newExample()
	if err != nil {
		log.Fatal(err)
	}

	// run API examples
	example.apiExamples()

	// start and process Feed
	example.startFeed()

	// Start catching signals.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	<-sigCh

	// Make sure the signal kills us for good.
	signal.Stop(sigCh)
	example.stop()
}

type Example struct {
	feed            protocols.OddsFeed
	feedChannel     protocols.GlobalMessageDelivery
	sessionChannel  protocols.SessionMessageDelivery
	producerManager protocols.ProducerManager
	recoveryManager protocols.RecoveryManager
	marketManager   protocols.MarketDescriptionManager
	sportsManager   protocols.SportsInfoManager

	// close channel
	closeCh chan bool
}

// newExample inits configuration and feed
func newExample() (*Example, error) {
	cfg := gosdk.NewConfiguration(token, env, nodeID, false)
	feed := gosdk.NewOddsFeed(cfg)

	producerManager, err := feed.ProducerManager()
	if err != nil {
		return nil, err
	}

	recoveryManager, err := feed.RecoveryManager()
	if err != nil {
		return nil, err
	}

	marketManager, err := feed.MarketDescriptionManager()
	if err != nil {
		return nil, err
	}

	sportsManager, err := feed.SportsInfoManager()
	if err != nil {
		return nil, err
	}

	sessionBuilder, err := feed.SessionBuilder()
	if err != nil {
		return nil, err
	}

	sessionChannel, err := sessionBuilder.SetMessageInterest(protocols.AllMessageInterest).Build()
	if err != nil {
		return nil, err
	}

	globalChannel, err := feed.Open()
	if err != nil {
		return nil, err
	}

	return &Example{
		feed:            feed,
		feedChannel:     globalChannel,
		sessionChannel:  sessionChannel,
		producerManager: producerManager,
		recoveryManager: recoveryManager,
		marketManager:   marketManager,
		sportsManager:   sportsManager,

		closeCh: make(chan bool, 1),
	}, nil
}

// Run api examples
func (e *Example) apiExamples() {
	go func() {
		if err := e.workWithRecovery(); err != nil {
			log.Println(err)
		}
		if err := e.workWithMarketManager(); err != nil {
			log.Println(err)
		}
		if err := e.workWithSportsManager(); err != nil {
			log.Println(err)
		}
		if err := e.workWithBookmaker(); err != nil {
			log.Println(err)
		}
	}()
}

func (e *Example) workWithMarketManager() error {
	markets, err := e.marketManager.MarketDescriptions()
	if err != nil {
		return err
	}
	for _, market := range markets {
		marketName, err := market.LocalizedName(locale)
		if err != nil {
			return err
		}
		log.Println("Market: ", *marketName)
		outcomeDescriptions, err := market.Outcomes()
		if err != nil {
			return err
		}
		for _, outcomeDesc := range outcomeDescriptions {
			log.Println("   Outcome:", *outcomeDesc.LocalizedName(locale))
		}
	}

	return nil
}

func (e *Example) workWithSportsManager() error {
	// sports
	sports, err := e.sportsManager.Sports()
	switch {
	case err != nil:
		return err
	case len(sports) == 0:
		return errors.New("no tournaments returned")
	}
	for _, sport := range sports {
		log.Println("Sport: ", sport.ID().ToString())
		names, err := sport.Names()
		if err != nil {
			return err
		}
		for locale, name := range names {
			log.Println("   ", locale, name)
		}
	}

	// active tournaments
	tournaments, err := e.sportsManager.ActiveTournaments()
	switch {
	case err != nil:
		return err
	case len(tournaments) == 0:
		return errors.New("no tournaments returned")
	}
	t := tournaments[0]
	name, err := t.LocalizedName(locale)
	if err != nil {
		return err
	}
	log.Println("Tournament:", *name)

	sport := t.Sport()
	sportURN := sport.ID()

	sportName, err := sport.LocalizedName(locale)
	if err != nil {
		return err
	}
	log.Println("   Sport:", sportURN.ToString(), *sportName)

	competitors, err := t.Competitors()
	if err != nil {
		return err
	}
	for _, c := range competitors {
		name, err := c.LocalizedName(locale)
		if err != nil {
			return err
		}
		log.Println("   Competitor:", *name)
	}

	// fixture changes
	changes, err := e.sportsManager.FixtureChanges(time.Now().Add(-1 * time.Hour))
	if err != nil {
		return err
	}
	for _, change := range changes {
		log.Println("Fixture change:", change.SportEventID().ToString(), change.UpdateTime())
	}

	// matches
	matches, err := e.sportsManager.ListOfMatches(0, 2)
	if err != nil {
		return err
	}
	for _, match := range matches {
		name, err := match.LocalizedName(locale)
		if err != nil {
			return err
		}

		log.Println("Match:", match.ID().ToString(), name)
		tournament, err := match.Tournament()
		if err != nil {
			return err
		}
		log.Println("   Tournament:", tournament.ID().ToString())
		log.Println("   Sport:", tournament.Sport().ID().ToString())
		status, err := match.Status().MatchStatus()
		if err != nil {
			return err
		}
		log.Println("   Status:", *status.GetDescription())
	}

	return nil
}

func (e *Example) workWithRecovery() error {
	matchURN, err := protocols.ParseURN("od:match:32109")
	if err != nil {
		return err
	}

	producers, err := e.producerManager.ActiveProducersInScope(protocols.LiveProducerScope)
	switch {
	case err != nil:
		return err
	case len(producers) == 0:
		return errors.New("no active producers with LIVE scope")
	}
	var liveProducer protocols.Producer
	for _, p := range producers {
		liveProducer = p
		break
	}

	// explicitly call for a recovery of a match

	requestID, err := e.recoveryManager.InitiateEventOddsMessagesRecovery(liveProducer.ID(), *matchURN)
	if err != nil {
		log.Println(err)
	}
	log.Println("EventOddsMessagesRecovery initiated: ", requestID)

	requestID, err = e.recoveryManager.InitiateEventStatefulMessagesRecovery(liveProducer.ID(), *matchURN)
	if err != nil {
		log.Println(err)
	}
	log.Println("EventStatefulMessagesRecovery initiated: ", requestID)
	return nil
}

func (e *Example) workWithBookmaker() error {
	bookmaker, err := e.feed.BookmakerDetails()
	if err != nil {
		return err
	}
	log.Println("Bookmaker ID: ", bookmaker.BookmakerID())
	log.Println("Expire at: ", bookmaker.ExpireAt())
	log.Println("Virtual host: ", bookmaker.VirtualHost())

	return nil
}

// listen to feed messages
func (e *Example) startFeed() {
	go func() {
		for {
			select {
			case sessionMsg := <-e.sessionChannel:
				if sessionMsg.UnparsableMessage != nil {
					log.Println("unparsed message")
					continue
				}

				requestMsg, ok := sessionMsg.Message.(protocols.RequestMessage)
				if !ok {
					log.Printf("failed to convert message to request message for client - message is %T", sessionMsg.Message)
					continue
				}

				e.handleFeedMessage(sessionMsg, requestMsg.RequestID())

			case feedMsg := <-e.feedChannel:
				if feedMsg.Recovery == nil {
					continue
				}
				e.handleRecoveryMessage(feedMsg.Recovery)

			case <-e.closeCh:
				return
			}
		}
	}()
}

func (e *Example) handleRecoveryMessage(recoveryMsg *protocols.RecoveryMessage) {
	if recoveryMsg.EventRecoveryMessage != nil {
		log.Printf("event recovery message for event %s with requestID %d", recoveryMsg.EventRecoveryMessage.EventID().ToString(),
			recoveryMsg.EventRecoveryMessage.RequestID())
	}

	if recoveryMsg.ProducerStatus != nil {
		if recoveryMsg.ProducerStatus.IsDown() {
			log.Printf("producer %d is down", recoveryMsg.ProducerStatus.Producer().ID())
			return
		}
		log.Printf("producer %d is up", recoveryMsg.ProducerStatus.Producer().ID())
	}
}

func (e *Example) handleFeedMessage(sessionMsg protocols.SessionMessage, requestID *uint) {
	if requestID == nil {
		// if producer is down, message is out of order - not recovered and no request id
		log.Print("message out of order")
	}

	switch msg := sessionMsg.Message.(type) {
	case protocols.OddsChange:
		e.processOddsChange(msg)
	case protocols.FixtureChangeMessage:
		e.processFixtureChange(msg)
	case protocols.BetCancel:
		e.processBetCancel(msg)
	case protocols.BetSettlement:
		e.processBetSettlement(msg)
	default:
		log.Printf("unknown msg type %T", msg)
	}
}

func (e *Example) processOddsChange(msg protocols.OddsChange) {
	match := msg.Event().(protocols.Match)
	log.Printf("odds changed in %s", match.ID().ToString())
	log.Println("raw message:", leftN(string(msg.RawMessage()), 256))

	for _, m := range msg.Markets() {
		name, err := m.LocalizedName(locale)
		if err != nil {
			log.Println(err)
			continue
		}
		log.Printf("odds change market to status %d: %s", m.Status(), *name)
	}
}

func (e *Example) processFixtureChange(msg protocols.FixtureChangeMessage) {
	sportEvent := msg.Event().(protocols.SportEvent)
	name, err := sportEvent.LocalizedName(locale)
	if err != nil {
		log.Println(err)
		return
	}

	sportURN, err := sportEvent.SportID()
	if err != nil {
		log.Println(err)
		return
	}

	log.Printf("fixtureChange received for: %s", *name)
	log.Printf("fixtureChange sport: %s", sportURN.ToString())
}

func (e *Example) processBetCancel(msg protocols.BetCancel) {
	sportEvent := msg.Event().(protocols.SportEvent)
	name, err := sportEvent.LocalizedName(locale)
	if err != nil {
		log.Println(err)
		return
	}

	sportURN, err := sportEvent.SportID()
	if err != nil {
		log.Println(err)
		return
	}

	log.Printf("betCancel received for: %s", *name)
	log.Printf("betCancel sport: %s", sportURN.ToString())
}

func (e *Example) processBetSettlement(msg protocols.BetSettlement) {
	sportEvent := msg.Event().(protocols.SportEvent)
	name, err := sportEvent.LocalizedName(locale)
	if err != nil {
		log.Println(err)
		return
	}

	sportURN, err := sportEvent.SportID()
	if err != nil {
		log.Println(err)
		return
	}

	log.Printf("betSettlement received for: %s", *name)
	log.Printf("betSettlement sport: %s", sportURN.ToString())
}

func leftN(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (e *Example) stop() {
	e.closeCh <- true
}
