Oddin.gg Golang SDK
-------------------

Rest API and Streaming API for connecting to Oddin.gg services and odds Feed.

### Installing 

```shell
go get github.com/oddin-gg/gosdk
```

### Example Feed client

see [example feed and api client](example/main.go) with full working demo of how to connect to Oddin.gg Feed

### How to start

Prepare your configuration:
```go
const (
	token  = "YOUR TOKEN"
	env    = protocols.IntegrationEnvironment
	nodeID = 2
)
```

Initialize Feed:
```go
cfg := gosdk.NewConfiguration(token, env, nodeID, false)
feed := gosdk.NewOddsFeed(cfg)
```

Retrieve any manager you may need:
```go
producerManager, err := feed.ProducerManager()
recoveryManager, err := feed.RecoveryManager()
sportsManager, err := feed.SportsInfoManager()
marketManager, err := feed.MarketDescriptionManager()
replyManager, err := feed.ReplayManager()
```

Build session and open the Feed:
```go
sessionBuilder, err := feed.SessionBuilder()
sessionChannel, err := sessionBuilder.SetMessageInterest(protocols.AllMessageInterest).Build()
globalChannel, err := feed.Open()
```

Start listening to messages:
```go
for {
    select {
    case sessionMsg := <-sessionChannel:
        if sessionMsg.UnparsableMessage != nil {
            log.Println("unparsed message")
            continue
        }

        requestMsg, ok := sessionMsg.Message.(protocols.RequestMessage)
        if !ok {
            log.Printf("failed to convert message to request message for client - message is %T", sessionMsg.Message)
            continue
        }

        handleFeedMessage(sessionMsg, requestMsg.RequestID())

    case feedMsg := <-feedChannel:
        if feedMsg.Recovery == nil {
            continue
        }
        handleRecoveryMessage(feedMsg.Recovery)

    case <-closeCh:
        return
    }
}
```

Handle Recovery messages:
```go
func handleRecoveryMessage(recoveryMsg *protocols.RecoveryMessage) {
	if recoveryMsg.ProducerStatus != nil {
		if recoveryMsg.ProducerStatus.IsDown() {
			log.Printf("producer %d is down", recoveryMsg.ProducerStatus.Producer().ID())
			return
		}
		log.Printf("producer %d is up", recoveryMsg.ProducerStatus.Producer().ID())
	}
}
```

Handle Feed messages:
```go
func handleFeedMessage(sessionMsg protocols.SessionMessage, requestID *uint) {
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
```

You should start receiving event odds via provided listeners.
