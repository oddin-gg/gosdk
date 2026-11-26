package recovery

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	log "github.com/oddin-gg/gosdk/internal/log"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// debugLogger returns a logger that records everything down to DEBUG.
func debugLogger(sink *lockedBuffer) *log.Logger {
	return log.New(slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})))
}

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// TestManager_SlowProcessingWarnIsThrottled: a stalled consumer made
// every message slow, and the "took more than 1s" warn went out once per
// message (thousands of lines). It is now one line per interval, the
// first at once and the next carrying the number held back.
func TestManager_SlowProcessingWarnIsThrottled(t *testing.T) {
	sink := &lockedBuffer{}
	clk := &testClock{t: time.Unix(5000, 0)}
	now := newFakeClock()
	m := &Manager{
		logger:                 debugLogger(sink),
		processing:             map[uuid.UUID]Stamp{},
		slowProcessingThrottle: log.NewThrottle(slowProcessingWarnInterval, clk.Now),
		clk:                    now,
	}

	slowMessage := func() {
		sid := uuid.New()
		started := now.Now()
		m.processing[sid] = started
		now.advance(2 * time.Second)
		// A zero gen timestamp moves no timestamp; only the duration check is
		// under test.
		m.OnMessageProcessingEnded(sid, 1, time.Time{})
	}

	for range 7 {
		slowMessage()
	}
	if got := strings.Count(sink.String(), "processing message took more than 1s"); got != 1 {
		t.Fatalf("slow-processing lines = %d, want 1\n%s", got, sink.String())
	}

	clk.Advance(slowProcessingWarnInterval + time.Second)
	slowMessage()
	out := sink.String()
	if got := strings.Count(out, "processing message took more than 1s"); got != 2 {
		t.Fatalf("slow-processing lines after the interval = %d, want 2\n%s", got, out)
	}
	if !strings.Contains(out, "suppressed=6") {
		t.Fatalf("second line does not carry suppressed=6\n%s", out)
	}
}
