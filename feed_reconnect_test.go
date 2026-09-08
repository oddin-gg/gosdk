package gosdk

import (
	"errors"
	"testing"

	"github.com/oddin-gg/gosdk/internal/feed"
)

// TestClient_FeedReconnect_NotifiesRecovery pins the client-side hook:
// a Connected event that FOLLOWS a drop reaches the recovery manager
// (which flags producers down so the next alive recovers the gap); the
// first Connected of a session does not.
func TestClient_FeedReconnect_NotifiesRecovery(t *testing.T) {
	c := newCatalogClient(t)
	rmgr := c.recoveryManager.Load()
	if rmgr == nil {
		t.Fatal("recovery manager not constructed by New")
	}
	c.connectState.Store(int32(ConnectionStateConnected))

	c.onFeedEvent(feed.Event{Kind: feed.EventConnected})
	if got := rmgr.FeedReconnectCount(); got != 0 {
		t.Fatalf("first connect counted as reconnect: %d", got)
	}

	c.onFeedEvent(feed.Event{Kind: feed.EventDisconnected, Err: errors.New("broker drop")})
	c.onFeedEvent(feed.Event{Kind: feed.EventReconnecting})
	c.onFeedEvent(feed.Event{Kind: feed.EventConnected})
	if got := rmgr.FeedReconnectCount(); got != 1 {
		t.Fatalf("reconnect not signalled to recovery: count = %d", got)
	}

	// A second Connected without a drop in between is not a reconnect.
	c.onFeedEvent(feed.Event{Kind: feed.EventConnected})
	if got := rmgr.FeedReconnectCount(); got != 1 {
		t.Fatalf("spurious reconnect signal: count = %d", got)
	}
}
