package factory

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	feedXML "github.com/oddin-gg/gosdk/internal/feed/xml"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/internal/producer"
	"github.com/oddin-gg/gosdk/types"
)

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type stepClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *stepClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func buildFailMessage(eventType string, eventNum int) *types.FeedMessage {
	id := types.URN{Prefix: "od", Type: eventType, ID: eventNum}
	sport := types.URN{Prefix: "od", Type: "sport", ID: 1}
	return &types.FeedMessage{
		BasicFeedMessage: types.BasicFeedMessage{
			RoutingKey: &types.RoutingKeyInfo{
				FullRoutingKey: "hi.-.live.odds_change.1." + id.ToString(),
				SportID:        &sport,
				EventID:        &id,
			},
			RawMessage: []byte("<odds_change/>"),
		},
		Message: &feedXML.OddsChange{ProductID: 1},
	}
}

// TestBuildMessage_BuildFailureWarnsAreThrottledPerKind: an API outage
// fails the build of every message's event; the warn used to go out once
// per message (thousands of lines in a soak). It is now one line per
// minute per kind (match / tournament, not per event id), carrying the
// number of lines held back.
func TestBuildMessage_BuildFailureWarnsAreThrottledPerKind(t *testing.T) {
	srv := entityFixtureServer(t)
	defer srv.Close()
	ef := newEntityFactory(t, srv)

	sink := &syncBuffer{}
	clk := &stepClock{t: time.Unix(1000, 0)}
	pm := producer.NewManager(minimalCfg{}, nil, log.New(nil))
	f := &FeedMessageFactory{
		entityFactory:         ef,
		producerManager:       pm,
		oddsFeedConfiguration: minimalCfg{},
		logger:                log.New(slog.New(slog.NewTextHandler(sink, nil))),
		throttleNow:           clk.Now,
	}

	// A cancelled context fails the build without any HTTP traffic.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	for i := range 6 {
		// Distinct event ids: the throttle must not be keyed per event.
		_, _ = f.BuildMessage(ctx, buildFailMessage("match", 100+i))
	}
	for i := range 4 {
		_, _ = f.BuildMessage(ctx, buildFailMessage("tournament", 200+i))
	}
	out := sink.String()
	if got := strings.Count(out, "feed message: build match"); got != 1 {
		t.Fatalf("match build-failure lines = %d, want 1\n%s", got, out)
	}
	if got := strings.Count(out, "feed message: build tournament"); got != 1 {
		t.Fatalf("tournament build-failure lines = %d, want 1 (own throttle per kind)\n%s", got, out)
	}

	clk.Advance(61 * time.Second)
	_, _ = f.BuildMessage(ctx, buildFailMessage("match", 300))
	out = sink.String()
	if got := strings.Count(out, "feed message: build match"); got != 2 {
		t.Fatalf("match lines after the interval = %d, want 2\n%s", got, out)
	}
	if !strings.Contains(out, "suppressed=5") {
		t.Fatalf("second match line does not carry suppressed=5\n%s", out)
	}
}

// A failed build of the event of an unparsable message warns through the
// same per-kind throttle as BuildMessage: one line per kind per interval,
// carrying the number of lines held back, however many messages fail.
func TestBuildUnparsableMessage_BuildFailureWarnsAreThrottledPerKind(t *testing.T) {
	srv := entityFixtureServer(t)
	defer srv.Close()
	ef := newEntityFactory(t, srv)

	sink := &syncBuffer{}
	clk := &stepClock{t: time.Unix(1000, 0)}
	pm := producer.NewManager(minimalCfg{}, nil, log.New(nil))
	f := &FeedMessageFactory{
		entityFactory:         ef,
		producerManager:       pm,
		oddsFeedConfiguration: minimalCfg{},
		logger:                log.New(slog.New(slog.NewTextHandler(sink, nil))),
		throttleNow:           clk.Now,
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	count := func(kind string) int {
		return strings.Count(sink.String(), "feed message: build "+kind+" od:")
	}
	for i := range 6 {
		f.BuildUnparsableMessage(ctx, buildFailMessage("match", 100+i))
	}
	for i := range 4 {
		f.BuildUnparsableMessage(ctx, buildFailMessage("tournament", 200+i))
	}
	if got := count("unparsable match"); got != 1 {
		t.Fatalf("unparsable match lines = %d, want 1\n%s", got, sink.String())
	}
	if got := count("unparsable tournament"); got != 1 {
		t.Fatalf("unparsable tournament lines = %d, want 1 (own throttle per kind)\n%s", got, sink.String())
	}

	clk.Advance(61 * time.Second)
	f.BuildUnparsableMessage(ctx, buildFailMessage("match", 300))
	if got := count("unparsable match"); got != 2 {
		t.Fatalf("unparsable match lines after the interval = %d, want 2\n%s", got, sink.String())
	}
	if !strings.Contains(sink.String(), "suppressed=5") {
		t.Fatalf("second unparsable match line does not carry suppressed=5\n%s", sink.String())
	}
	// The kinds of BuildMessage are separate: its throttle was not used up.
	_, _ = f.BuildMessage(ctx, buildFailMessage("match", 400))
	if got := strings.Count(sink.String(), "feed message: build match od:"); got != 1 {
		t.Fatalf("match lines = %d, want 1 (a kind of its own)\n%s", got, sink.String())
	}
}

// The message's timer counts as fired once its deadline has passed, even if
// the context has not reported it yet.
func TestMessageTimerFired(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Hour)
	defer cancel()
	if messageTimerFired(ctx) {
		t.Fatal("a message with time left reads as expired")
	}
	if !messageTimerFired(laggingDeadline{Context: t.Context(), deadline: time.Now().Add(-time.Millisecond)}) {
		t.Fatal("a passed deadline does not read as expired")
	}
	if messageTimerFired(t.Context()) {
		t.Fatal("a context without a deadline reads as expired")
	}
	cancelled, stop := context.WithCancel(t.Context())
	stop()
	if messageTimerFired(cancelled) {
		t.Fatal("a cancelled context without a deadline reads as the message timer")
	}
}

type laggingDeadline struct {
	context.Context
	deadline time.Time
}

func (c laggingDeadline) Deadline() (time.Time, bool) { return c.deadline, true }
