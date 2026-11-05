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
	m := &Manager{
		logger:                 debugLogger(sink),
		processingTimes:        map[uuid.UUID]time.Time{},
		slowProcessingThrottle: log.NewThrottle(slowProcessingWarnInterval, clk.Now),
	}

	slowMessage := func() {
		sid := uuid.New()
		m.processingTimes[sid] = time.Now().Add(-2 * time.Second)
		// A zero gen timestamp skips the actor dispatch; only the
		// duration check is under test.
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

// TestActor_SnapshotResultLostOnShutdownIsNotAWarn: closing the client
// while a snapshot recovery POST is in flight is ordinary; the "result
// lost" line used to be a WARN there. Every exit of sendCtx (ctx
// cancelled, shutdown before admission, shutdown after admission, ctx
// done while waiting, shutdown while waiting) is a shutdown, so a close
// logs it at DEBUG only.
func TestActor_SnapshotResultLostOnShutdownIsNotAWarn(t *testing.T) {
	srv, hits := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActor(t, srv, newFakeManagerOps())
	sink := &lockedBuffer{}
	a.logger = debugLogger(sink)

	// The actor is shutting down: the detached POST finishes and its
	// result cannot be delivered.
	close(a.shutdown)
	if err := a.makeSnapshotRecovery(time.Time{}); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 1)
	waitDetachedDone(t, a, 3*time.Second)

	out := sink.String()
	if !strings.Contains(out, "snapshot recovery API result lost") {
		t.Fatalf("expected the result-lost line at debug level\n%s", out)
	}
	for line := range strings.SplitSeq(out, "\n") {
		if strings.Contains(line, "snapshot recovery API result lost") && !strings.Contains(line, "level=DEBUG") {
			t.Fatalf("result-lost line is not DEBUG on shutdown: %s", line)
		}
	}
}
