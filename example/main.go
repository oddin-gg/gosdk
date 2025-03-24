package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/oddin-gg/gosdk"
	"github.com/oddin-gg/gosdk/protocols"
)

const defaultNodeID = 1

// Demo constants
var (
	token  = "YOUR TOKEN"
	env    = protocols.IntegrationEnvironment
	region = protocols.DefaulRegion
	nodeID = defaultNodeID
	locale = protocols.EnLocale
)

// Sample demo working with Oddin.gg Api and Feed
func main() {

	// replace config with env variables, if provided
	initEnv()

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

// initEnv replaces config with env variables, if provided
func initEnv() {
	if v := os.Getenv("TOKEN"); len(v) > 0 {
		token = v
	}

	if v := strings.ToLower(os.Getenv("ENV")); len(v) > 0 {
		switch v {
		case "integration":
			env = protocols.IntegrationEnvironment
		case "production":
			env = protocols.ProductionEnvironment
		case "test":
			env = protocols.TestEnvironment
		default:
			log.Printf("ENV environment variable has invalid value %s, using default", v)
		}
	}

	if v := strings.ToLower(os.Getenv("REGION")); len(v) > 0 {
		switch v {
		case "ap":
			region = protocols.APSouthEast1
		case "eu":
			region = protocols.DefaulRegion
		default:
			log.Printf("REGION environment variable has invalid value %s, using default", v)
		}
	}

	if v := os.Getenv("NODE"); len(v) > 0 {
		var err error
		nodeID, err = strconv.Atoi(v)
		if err != nil || nodeID <= 0 {
			log.Printf("NODE environment variable has invalid value %s, using default", v)
		} else {
			nodeID = defaultNodeID
		}
	}
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
	cfg := gosdk.
		NewConfiguration(token, env, nodeID, false).
		SetRegion(region)

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
		if err := e.workWithRaceSports(); err != nil {
			log.Println(err)
		}
		if err := e.workWithBookmaker(); err != nil {
			log.Println(err)
		}
	}()
}

func (e *Example) workWithMarketManager() error {
	// display markets
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

	// display void reasons
	voidReasons, err := e.marketManager.MarketVoidReasons()
	if err != nil {
		return err
	}

	for _, v := range voidReasons {
		description := "(nil)"
		if v.Description() != nil {
			description = *v.Description()
		}
		template := "(nil)"
		if v.Template() != nil {
			template = *v.Template()
		}
		log.Println("Void Reason:", v.ID(), "|", v.Name(), "|", description, "|", template, "|", v.Params())
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

	// Competitor Players
	competitorURN, err := protocols.ParseURN("od:competitor:2976")
	if err != nil {
		return err
	}
	competitor, err := e.sportsManager.Competitor(*competitorURN)
	if err != nil {
		return err
	}
	players, err := competitor.LocalizedPlayers(locale)
	if err != nil {
		return err
	}
	log.Println("Competitor Players:")
	for _, player := range players {
		localizedName, err := player.LocalizedName()
		if err != nil {
			return err
		}

		log.Println("    Localized name:", *localizedName)
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

		home, err := match.HomeCompetitor()
		if err != nil {
			return err
		}

		log.Println("    Home players:")
		homePlayers, err := home.Players()
		if err != nil {
			return err
		}
		for _, localizedPlayers := range homePlayers {
			for _, localizedPlayer := range localizedPlayers {
				localizedName, err := localizedPlayer.LocalizedName()
				if err != nil {
					return err
				}

				log.Println("        Localized name:", *localizedName)
			}
		}
	}

	return nil
}

func (e *Example) workWithRaceSports() error {
	raceURN, err := protocols.ParseURN("od:match:6516")
	if err != nil {
		return err
	}

	m, err := e.sportsManager.Match(*raceURN)
	if err != nil {
		return err
	}

	raceName, err := m.LocalizedName(locale)
	if err != nil {
		return err
	}

	if raceName != nil {
		log.Println("Race name:", *raceName)
	}

	sportFormat, err := m.SportFormat()
	if err != nil {
		return err
	}
	log.Println("Sport format:", sportFormat)

	competitors, err := m.Competitors()
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

	if liveProducer == nil {
		return fmt.Errorf("no active producers with LIVE scope")
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
	case protocols.RollbackBetSettlement:
		e.processRollbackBetSettlement(msg)
	case protocols.RollbackBetCancel:
		e.processRollbackBetCancel(msg)
	default:
		log.Printf("unknown msg type %T", msg)
	}
}

func (e *Example) processOddsChange(msg protocols.OddsChange) {
	match, ok := msg.Event().(protocols.Match)
	if !ok {
		return
	}

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

	// Scoreboard
	if ok, err := match.Status().IsScoreboardAvailable(); ok && err == nil {
		if scoreboard, err := match.Status().Scoreboard(); scoreboard != nil && err == nil {
			if scoreboard.HomeGoals() != nil {
				log.Printf("HomeGoals: %d\n", *scoreboard.HomeGoals())
			}
			if scoreboard.AwayGoals() != nil {
				log.Printf("AwayGoals: %d\n", *scoreboard.AwayGoals())
			}
			if scoreboard.Time() != nil {
				log.Printf("Time: %d\n", *scoreboard.Time())
			}
			if scoreboard.GameTime() != nil {
				log.Printf("GameTime: %d\n", *scoreboard.GameTime())
			}
		}
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
	log.Printf("betCancel sport: %s\n", sportURN.ToString())
	if startTime := msg.StartTime(); startTime == nil {
		log.Println("betCancel start time: Not set")
	} else {
		log.Println("betCancel start time:", startTime.Format(time.RFC3339))
	}
	if endTime := msg.EndTime(); endTime == nil {
		log.Println("betCancel end time: Not set")
	} else {
		log.Println("betCancel end time:", endTime.Format(time.RFC3339))
	}

	for _, m := range msg.Markets() {
		marketName, err := m.LocalizedName(locale)
		if err != nil {
			log.Println(err)
			return
		}
		voidReasonID := uint(0)
		if m.VoidReasonID() != nil {
			voidReasonID = *m.VoidReasonID()
		}
		voidReasonParams := "(nil)"
		if m.VoidReasonParams() != nil {
			voidReasonParams = *m.VoidReasonParams()
		}
		log.Printf("Canceled market: '%v'; VoidReasonID: %d; VoidReasonParams: %v\n", *marketName, voidReasonID, voidReasonParams)
	}
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

	for _, m := range msg.Markets() {
		for _, o := range m.OutcomeSettlements() {
			if o.VoidFactor() != nil {
				log.Printf("outcome with void factor %f", *o.VoidFactor())
			}
		}
	}
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

func (e *Example) processRollbackBetSettlement(msg protocols.RollbackBetSettlement) {
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

	log.Printf("rollbackBetSettlement received for: %s", *name)
	log.Printf("rollbackBetSettlement sport: %s\n", sportURN.ToString())

	for _, m := range msg.RolledBackSettledMarkets() {
		marketName, err := m.LocalizedName(locale)
		if err != nil {
			log.Println(err)
			return
		}
		log.Printf("Rollback Bet Settlement: '%v'\n", *marketName)
	}
}

func (e *Example) processRollbackBetCancel(msg protocols.RollbackBetCancel) {
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

	log.Printf("processRollbackBetCancel received for: %s", *name)
	log.Printf("processRollbackBetCancel sport: %s\n", sportURN.ToString())
	if startTime := msg.StartTime(); startTime == nil {
		log.Println("processRollbackBetCancel start time: Not set")
	} else {
		log.Println("processRollbackBetCancel start time:", startTime.Format(time.RFC3339))
	}
	if endTime := msg.EndTime(); endTime == nil {
		log.Println("processRollbackBetCancel end time: Not set")
	} else {
		log.Println("processRollbackBetCancel end time:", endTime.Format(time.RFC3339))
	}

	for _, m := range msg.RolledBackCanceledMarkets() {
		marketName, err := m.LocalizedName(locale)
		if err != nil {
			log.Println(err)
			return
		}
		log.Printf("Rollback Bet Cancel: '%v'\n", *marketName)
	}
}
