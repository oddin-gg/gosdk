package gosdk

import (
	"context"
	"testing"
	"time"

	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	"github.com/oddin-gg/gosdk/types"
)

// TestClient_BuildMessage_ResolvesMarketAndOutcomeNames drives an
// odds_change through the client's real message factory and caches, the
// way the session does, and checks the composed names a consumer reads
// off the message: the market's catalog name, and the home/away
// placeholder outcomes replaced by the match's competitor names.
//
// It is the end-to-end guard for CORE-4213, which moved name resolution
// off the Snapshot() projection onto direct reads of the live cache
// entry: whatever the read path, THIS is the contract.
func TestClient_BuildMessage_ResolvesMarketAndOutcomeNames(t *testing.T) {
	c := newCatalogClient(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// The producer catalog is loaded on Open, not New; BuildMessage's
	// cached producer read needs it.
	if err := c.producerManager.Open(ctx); err != nil {
		t.Fatalf("producer manager open: %v", err)
	}

	eventID, err := types.ParseURN("od:match:42")
	if err != nil {
		t.Fatal(err)
	}
	sportID, err := types.ParseURN("od:sport:1")
	if err != nil {
		t.Fatal(err)
	}

	active := 1
	market := &feedXML.MarketWithOutcome{}
	market.ID = 1
	market.Outcomes = []feedXML.Outcome{{ID: "1", Active: &active}, {ID: "2", Active: &active}}
	msg := &feedXML.OddsChange{EventID: "od:match:42", ProductID: 1}
	msg.Odds.Markets = []*feedXML.MarketWithOutcome{market}

	built, err := c.feedMessageFactory.BuildMessage(ctx, &types.FeedMessage{
		BasicFeedMessage: types.BasicFeedMessage{
			RawMessage: []byte(`<odds_change/>`),
			RoutingKey: &types.RoutingKeyInfo{
				FullRoutingKey: "hi.live.-.odds_change.1.od:match.42.-.-",
				SportID:        sportID,
				EventID:        eventID,
			},
		},
		Message: msg,
	})
	if err != nil {
		t.Fatalf("BuildMessage: %v", err)
	}
	oc, ok := built.(types.OddsChange)
	if !ok {
		t.Fatalf("BuildMessage returned %T, want types.OddsChange", built)
	}
	markets := oc.Markets()
	if len(markets) != 1 || len(markets[0].OutcomeOdds) != 2 {
		t.Fatalf("markets = %+v", markets)
	}

	if got := markets[0].Name(types.EnLocale).ValueOr("<none>"); got != "1x2" {
		t.Fatalf("market name = %q, want 1x2", got)
	}
	// Catalog labels are "home"/"away" (client_catalog_test fixture);
	// the match's competitors are "Home"/"Away".
	if got := markets[0].OutcomeOdds[0].Name(types.EnLocale).ValueOr("<none>"); got != "Home" {
		t.Fatalf("outcome 1 name = %q, want the home competitor's name", got)
	}
	if got := markets[0].OutcomeOdds[1].Name(types.EnLocale).ValueOr("<none>"); got != "Away" {
		t.Fatalf("outcome 2 name = %q, want the away competitor's name", got)
	}
	// Not preloaded → None, not "" and not an error.
	if _, ok := markets[0].Name(types.RuLocale).Get(); ok {
		t.Fatal("ru market name = Some, want None (locale not configured)")
	}
}
