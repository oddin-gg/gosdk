package recovery

import (
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/config"
	"github.com/oddin-gg/gosdk/internal/producer"
	"github.com/oddin-gg/gosdk/types"
)

// deadlineCfg is minimalCfg with a configurable recovery cap and
// snapshot-complete deadline.
type deadlineCfg struct {
	*minimalCfg
	maxRecovery time.Duration
	deadline    time.Duration
}

func (c *deadlineCfg) MaxRecoveryExecution() time.Duration     { return c.maxRecovery }
func (c *deadlineCfg) SnapshotCompleteDeadline() time.Duration { return c.deadline }

func newWiredActorWithCfg(t *testing.T, srv *httptest.Server, fake *fakeManagerOps, producerID int, mk func(*minimalCfg) config.Config) *recoveryActor {
	t.Helper()
	pm := newProducerManagerFor(t, srv)
	u, _ := url.Parse(srv.URL)
	cfg := mk(&minimalCfg{apiURL: u.Host, token: "tok"})
	apiClient := api.New(cfg)
	apiClient.SetHTTPClient(&http.Client{
		Transport: &rewriteTransport{
			target: srv.URL,
			base:   &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		},
		Timeout: 2 * time.Second,
	})
	return newRecoveryActor(t.Context(), producerID, cfg, apiClient, pm, fake, newDiscardLogger(), 32, 0)
}

// startRecovery starts a snapshot recovery from `from` on the actor and
// waits for its POST. Returns the request id.
func startRecovery(t *testing.T, a *recoveryActor, hits *recoveryHits, from time.Time, wantPosts int32) int {
	t.Helper()
	if err := a.makeSnapshotRecovery(from); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, wantPosts)
	return a.currentRecovery.recoveryID
}

// TestActor_SnapshotCompleteDeadline_ReissuesWithANewRequestID: a snapshot
// recovery whose snapshot_complete never comes (or whose request was lost)
// used to wait for MaxRecoveryExecution, 6 h by default, with the producer
// down. After the snapshot-complete deadline the actor now re-requests it
// from the same recover-from point under a NEW request id, the producer
// stays down, and the abandoned request's late snapshot_complete does not
// complete the new one.
func TestActor_SnapshotCompleteDeadline_ReissuesWithANewRequestID(t *testing.T) {
	fake := newFakeManagerOps()
	srv, hits := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActor(t, srv, fake)

	from := time.Now().Add(-10 * time.Minute).Truncate(time.Millisecond)
	oldID := startRecovery(t, a, hits, from, 1)
	if got := hits.lastAfterMillis.Load(); got != from.UnixMilli() {
		t.Fatalf("first request after=%d, want %d", got, from.UnixMilli())
	}
	started := a.currentRecovery.recoveryStartedAt

	// Inside the deadline: nothing happens.
	a.onTick(started.Add(4*time.Minute), false)
	if a.currentRecovery.recoveryID != oldID || hits.recover.Load() != 1 {
		t.Fatalf("re-issued before the deadline: id=%d posts=%d", a.currentRecovery.recoveryID, hits.recover.Load())
	}

	// Past the deadline: a new request, same recover-from point.
	a.onTick(started.Add(6*time.Minute), false)
	newID := a.currentRecovery.recoveryID
	if newID == oldID {
		t.Fatal("deadline passed but the recovery kept its request id")
	}
	if a.recoveryState != types.StartedRecoveryState {
		t.Fatalf("state after re-issue = %v, want Started", a.recoveryState)
	}
	if !a.currentRecovery.recoverFrom.Equal(from) {
		t.Fatalf("re-issued recoverFrom = %v, want %v", a.currentRecovery.recoverFrom, from)
	}
	waitRecoverHits(t, hits, 2)
	if got := hits.lastAfterMillis.Load(); got != from.UnixMilli() {
		t.Fatalf("second request after=%d, want the same %d", got, from.UnixMilli())
	}
	if !a.isFlaggedDown() {
		t.Fatal("producer must stay down while the recovery is re-requested")
	}

	// The abandoned request's late snapshot_complete must not complete the
	// new recovery.
	a.onSnapshotComplete(evSnapshotComplete{requestID: oldID, messageInterest: types.LiveOnlyMessageInterest})
	if a.recoveryState != types.StartedRecoveryState || a.currentRecovery.recoveryID != newID || !a.isFlaggedDown() {
		t.Fatalf("late snapshot_complete of the old id changed state: state=%v id=%d down=%v",
			a.recoveryState, a.currentRecovery.recoveryID, a.isFlaggedDown())
	}
	// The new one does.
	a.onSnapshotComplete(evSnapshotComplete{requestID: newID, messageInterest: types.LiveOnlyMessageInterest})
	if a.recoveryState != types.CompletedRecoveryState || a.isFlaggedDown() {
		t.Fatalf("snapshot_complete of the new id: state=%v down=%v", a.recoveryState, a.isFlaggedDown())
	}
	waitDetachedDone(t, a, 3*time.Second)
}

// TestActor_SnapshotCompleteDeadline_WaitsWhileProcessingLags: with a
// consumer backlog longer than the deadline the replay sits unprocessed and
// the recovery looks silent, so it used to be re-issued every deadline,
// adding a replay to the queue it was stuck in. While processing lags (or
// cannot be read) no re-issue happens; the deadline counts again from the
// moment processing is current.
func TestActor_SnapshotCompleteDeadline_WaitsWhileProcessingLags(t *testing.T) {
	fake := newFakeManagerOps()
	srv, hits := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActor(t, srv, fake)

	from := time.Now().Add(-10 * time.Minute).Truncate(time.Millisecond)
	oldID := startRecovery(t, a, hits, from, 1)
	started := a.currentRecovery.recoveryStartedAt
	p := holdProcessing(a)
	// The backlog: a message is taken up and stays in processing.
	a.onMessageProcessingStarted(time.Time{})

	// A backlog that outlasts two deadlines: no re-issue.
	for _, after := range []time.Duration{6 * time.Minute, 12 * time.Minute} {
		p.set(started.Add(after).Add(-time.Minute))
		a.onTick(started.Add(after), false)
	}
	time.Sleep(50 * time.Millisecond)
	if a.currentRecovery.recoveryID != oldID || hits.recover.Load() != 1 {
		t.Fatalf("re-issued while processing lagged: id=%d (was %d) posts=%d", a.currentRecovery.recoveryID, oldID, hits.recover.Load())
	}

	// A lookup that fails is no evidence of silence either.
	lookup := a.lastProcessedFn
	a.lastProcessedFn = func() (time.Time, error) { return time.Time{}, errors.New("lookup failed") }
	a.onTick(started.Add(18*time.Minute), false)
	a.lastProcessedFn = lookup
	if a.currentRecovery.recoveryID != oldID || hits.recover.Load() != 1 {
		t.Fatalf("re-issued on a failed lookup: id=%d posts=%d", a.currentRecovery.recoveryID, hits.recover.Load())
	}

	// Processing is current again, the replay still has not shown up: the
	// deadline counts from here.
	p.set(started.Add(19 * time.Minute))
	a.onTick(started.Add(19*time.Minute), false)
	if a.currentRecovery.recoveryID != oldID {
		t.Fatal("re-issued right after the backlog drained, before a full deadline of silence")
	}
	p.set(started.Add(25 * time.Minute))
	a.onTick(started.Add(25*time.Minute), false)
	if a.currentRecovery.recoveryID == oldID {
		t.Fatal("silent for the deadline with processing current, but not re-issued")
	}
	waitRecoverHits(t, hits, 2)
	waitDetachedDone(t, a, 3*time.Second)
}

// TestActor_SnapshotCompleteDeadline_WaitsForAFirstMessageStillInProcessing:
// on a first recovery nothing has been processed yet, so there is no
// last-processed timestamp and the timing counts as fresh. The first message
// can stay in a stalled client callback longer than the deadline; the replay
// then sits behind it, and every deadline used to queue another snapshot
// onto the same stalled consumer. Until a message has ended the silence is
// no evidence; once one has, and the silence goes on, it is re-issued.
func TestActor_SnapshotCompleteDeadline_WaitsForAFirstMessageStillInProcessing(t *testing.T) {
	srv, hits := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActor(t, srv, newFakeManagerOps())

	oldID := startRecovery(t, a, hits, time.Now().Add(-10*time.Minute).Truncate(time.Millisecond), 1)
	started := a.currentRecovery.recoveryStartedAt

	// A message starts processing and never ends; the baseline stays zero.
	a.onMessageProcessingStarted(started.Add(time.Second))
	if ts, _ := a.lastProcessedMessageGenTimestamp(); !ts.IsZero() {
		t.Fatalf("test setup: last processed = %v, want zero", ts)
	}
	for _, after := range []time.Duration{6 * time.Minute, 12 * time.Minute, 18 * time.Minute} {
		a.onTick(started.Add(after), false)
	}
	time.Sleep(50 * time.Millisecond)
	if a.currentRecovery.recoveryID != oldID || hits.recover.Load() != 1 {
		t.Fatalf("re-issued while the first message was still in processing: id=%d (was %d) posts=%d",
			a.currentRecovery.recoveryID, oldID, hits.recover.Load())
	}

	// The message ends; the deadline counts from the next tick, then the
	// silence is evidence as before.
	// (The producer manager rejects timestamps this far ahead of the wall
	// clock, so the processing state is held through the test seam.)
	p := holdProcessing(a)
	end := started.Add(19 * time.Minute)
	p.set(end)
	a.onTick(end, false)
	if a.currentRecovery.recoveryID != oldID {
		t.Fatal("re-issued right after the message ended, before a full deadline of silence")
	}
	// Processing is current (messages keep ending), the replay is still silent.
	p.set(end.Add(6 * time.Minute))
	a.onTick(end.Add(6*time.Minute), false)
	if a.currentRecovery.recoveryID == oldID {
		t.Fatal("silent for the deadline with processing current, but not re-issued")
	}
	waitRecoverHits(t, hits, 2)
	waitDetachedDone(t, a, 3*time.Second)
}

// TestActor_SnapshotCompleteDeadline_RecoveryMessagesRenewIt: a recovery
// that keeps replaying messages (they carry its request id) is alive and
// must not be re-requested, however long it runs; the deadline counts from
// the last sign of life.
func TestActor_SnapshotCompleteDeadline_RecoveryMessagesRenewIt(t *testing.T) {
	srv, hits := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActor(t, srv, newFakeManagerOps())

	id := startRecovery(t, a, hits, time.Time{}, 1)
	started := a.currentRecovery.recoveryStartedAt

	// A message of this recovery at +4m, another at +8m.
	a.noteRecoveryMessage(id, started.Add(4*time.Minute))
	a.onTick(started.Add(8*time.Minute), false) // 4m since the message: alive
	a.noteRecoveryMessage(id, started.Add(8*time.Minute))
	a.onTick(started.Add(12*time.Minute), false) // 4m since the message: alive
	if a.currentRecovery.recoveryID != id || hits.recover.Load() != 1 {
		t.Fatalf("a recovery that keeps replaying was re-requested: id=%d posts=%d", a.currentRecovery.recoveryID, hits.recover.Load())
	}

	// A message of ANOTHER request id renews nothing.
	a.noteRecoveryMessage(id+100, started.Add(13*time.Minute))
	a.onTick(started.Add(14*time.Minute), false) // 6m since the last message of this id
	if a.currentRecovery.recoveryID == id {
		t.Fatal("a message of a different request id renewed the deadline")
	}
	waitDetachedDone(t, a, 3*time.Second)
}

// A partial snapshot_complete (one scope of a mixed producer) is a sign of
// life too.
func TestActor_SnapshotCompleteDeadline_PartialSnapshotCompleteRenewsIt(t *testing.T) {
	srv, hits := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActorForProducer(t, srv, newFakeManagerOps(), 3) // live|prematch

	id := startRecovery(t, a, hits, time.Time{}, 1)
	// The request went out 10 minutes ago and nothing was heard since.
	a.currentRecovery.recoveryStartedAt = time.Now().Add(-10 * time.Minute)

	// The live scope reports in: the recovery is alive (and not complete,
	// the prematch scope is missing).
	a.onSnapshotComplete(evSnapshotComplete{requestID: id, messageInterest: types.LiveOnlyMessageInterest})
	if a.recoveryState != types.StartedRecoveryState {
		t.Fatalf("one scope of a mixed producer completed the recovery: %v", a.recoveryState)
	}
	a.onTick(time.Now().Add(time.Minute), false)
	if a.currentRecovery.recoveryID != id {
		t.Fatal("a partial snapshot_complete did not renew the deadline")
	}
	// Without further signs of life it is re-requested after a full deadline.
	a.onTick(time.Now().Add(6*time.Minute), false)
	if a.currentRecovery.recoveryID == id {
		t.Fatal("not re-requested 6m after the last sign of life")
	}
	waitDetachedDone(t, a, 3*time.Second)
}

// MaxRecoveryExecution still caps the TOTAL: re-issues after the deadline
// do not restart the clock of the whole attempt.
func TestActor_SnapshotCompleteDeadline_MaxRecoveryExecutionStillCapsTheTotal(t *testing.T) {
	srv, hits := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActorWithCfg(t, srv, newFakeManagerOps(), 1, func(m *minimalCfg) config.Config {
		return &deadlineCfg{minimalCfg: m, maxRecovery: 12 * time.Minute, deadline: 5 * time.Minute}
	})

	id1 := startRecovery(t, a, hits, time.Time{}, 1)
	start := a.currentRecovery.recoveryStartedAt

	// makeSnapshotRecovery stamps a re-issue with the real clock; move
	// each one to the simulated tick time, as a real run would have it.
	t1 := start.Add(5*time.Minute + time.Second)
	a.onTick(t1, false) // re-issue #1
	id2 := a.currentRecovery.recoveryID
	a.currentRecovery.recoveryStartedAt = t1
	t2 := start.Add(10*time.Minute + 2*time.Second)
	a.onTick(t2, false) // re-issue #2
	id3 := a.currentRecovery.recoveryID
	a.currentRecovery.recoveryStartedAt = t2
	if id1 == id2 || id2 == id3 {
		t.Fatalf("expected two re-issues, ids %d %d %d", id1, id2, id3)
	}
	if !a.currentRecovery.chainStartedAt.Equal(start) {
		t.Fatalf("chain start moved to %v, want the first request's %v", a.currentRecovery.chainStartedAt, start)
	}

	// 12m30s after the FIRST request, only 2m28s after the last one.
	a.onTick(start.Add(12*time.Minute+30*time.Second), false)
	if a.currentRecovery != nil || a.recoveryState != types.ErrorRecoveryState {
		t.Fatalf("MaxRecoveryExecution did not cap the chain: state=%v current=%v", a.recoveryState, a.currentRecovery)
	}
	waitDetachedDone(t, a, 3*time.Second)
}

// The alive path caps the chain by the same original start as the tick path:
// a recovery re-issued by the deadline keeps the clock of its first request.
// A chain past MaxRecoveryExecution is abandoned (a new recovery under a new
// request id replaces it) although the last request is still young.
func TestActor_SnapshotCompleteDeadline_MaxRecoveryExecutionCapsTheTotalOnTheAlivePath(t *testing.T) {
	srv, hits := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActorWithCfg(t, srv, newFakeManagerOps(), 1, func(m *minimalCfg) config.Config {
		return &deadlineCfg{minimalCfg: m, maxRecovery: 12 * time.Minute, deadline: 5 * time.Minute}
	})

	id1 := startRecovery(t, a, hits, time.Time{}, 1)
	start := a.currentRecovery.recoveryStartedAt
	a.onTick(start.Add(5*time.Minute+time.Second), false) // re-issue by the deadline
	id2 := a.currentRecovery.recoveryID
	if id1 == id2 {
		t.Fatal("expected a re-issue")
	}
	waitRecoverHits(t, hits, 2)

	// The first request is 13 minutes old, the last one 1 minute.
	now := time.Now()
	a.currentRecovery.chainStartedAt = now.Add(-13 * time.Minute)
	a.currentRecovery.recoveryStartedAt = now.Add(-time.Minute)

	ts := types.MessageTimestamp{Created: now, Received: now}
	if err := a.systemAliveReceived(ts, true); err != nil {
		t.Fatal(err)
	}
	if a.currentRecovery == nil || a.currentRecovery.recoveryID == id2 {
		t.Fatalf("the alive path did not abandon the chain past MaxRecoveryExecution (id %d)", id2)
	}
	waitRecoverHits(t, hits, 3)
	if age := time.Since(a.currentRecovery.chainStartedAt); age > time.Minute {
		t.Fatalf("the replacement recovery carries the old chain start (%v ago)", age)
	}
}

// pastTheCap starts a recovery from `from` and ages its chain beyond
// MaxRecoveryExecution, as a request that never completed would be.
func pastTheCap(t *testing.T, from time.Time) (*recoveryActor, *recoveryHits) {
	t.Helper()
	srv, hits := fixtureSrv(t)
	t.Cleanup(srv.Close)
	a := newWiredActorWithCfg(t, srv, newFakeManagerOps(), 1, func(m *minimalCfg) config.Config {
		return &deadlineCfg{minimalCfg: m, maxRecovery: 12 * time.Minute, deadline: 5 * time.Minute}
	})
	startRecovery(t, a, hits, from, 1)
	a.currentRecovery.chainStartedAt = time.Now().Add(-13 * time.Minute)
	return a, hits
}

// afterTheCap checks the request that follows a recovery dropped at the cap
// reaches back to `want` (zero: the full history, no `after`).
func afterTheCap(t *testing.T, hits *recoveryHits, want time.Time) {
	t.Helper()
	waitRecoverHits(t, hits, 2)
	got := hits.lastAfterMillis.Load()
	if want.IsZero() {
		if got != 0 {
			t.Fatalf("next recovery: after=%d (%v), want none (the full history)", got, time.UnixMilli(got))
		}
		return
	}
	if got != want.UnixMilli() {
		t.Fatalf("next recovery: after=%d (%v), want the original %v", got, time.UnixMilli(got), want)
	}
}

// The tick drops a recovery that has outlived MaxRecoveryExecution; the
// request after it must still reach back to where it started, not to a
// cursor clamped against a later "now".
func TestActor_RecoveryCapOnTickKeepsTheOriginalPoint(t *testing.T) {
	from := time.Now().Add(-10 * time.Minute).Truncate(time.Millisecond)
	a, hits := pastTheCap(t, from)

	a.onTick(time.Now(), false)
	if a.currentRecovery != nil || a.recoveryState != types.ErrorRecoveryState {
		t.Fatalf("setup: the tick left recovery=%v state=%v", a.currentRecovery, a.recoveryState)
	}
	if err := a.makeSnapshotRecovery(time.Now().Add(-time.Minute).Truncate(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	afterTheCap(t, hits, from)
	waitDetachedDone(t, a, 3*time.Second)
}

func TestActor_RecoveryCapOnTickKeepsTheFullHistory(t *testing.T) {
	a, hits := pastTheCap(t, time.Time{})

	a.onTick(time.Now(), false)
	if a.currentRecovery != nil || a.recoveryState != types.ErrorRecoveryState {
		t.Fatalf("setup: the tick left recovery=%v state=%v", a.currentRecovery, a.recoveryState)
	}
	if err := a.makeSnapshotRecovery(time.Now().Add(-time.Minute).Truncate(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	afterTheCap(t, hits, time.Time{})
	waitDetachedDone(t, a, 3*time.Second)
}

// The alive path drops the same recovery and starts the next one itself,
// from the producer's cursor: it too must keep the original point.
func TestActor_RecoveryCapOnAliveKeepsTheOriginalPoint(t *testing.T) {
	from := time.Now().Add(-10 * time.Minute).Truncate(time.Millisecond)
	a, hits := pastTheCap(t, from)

	// The producer's cursor is a later point than the dropped request's.
	now := time.Now()
	if err := a.pm.SetProducerRecoveryFromTimestamp(t.Context(), 1, now.Add(-time.Minute).Truncate(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := a.systemAliveReceived(types.MessageTimestamp{Created: now, Received: now}, true); err != nil {
		t.Fatal(err)
	}
	afterTheCap(t, hits, from)
	waitDetachedDone(t, a, 3*time.Second)
}

func TestActor_RecoveryCapOnAliveKeepsTheFullHistory(t *testing.T) {
	a, hits := pastTheCap(t, time.Time{})

	now := time.Now()
	if err := a.pm.SetProducerRecoveryFromTimestamp(t.Context(), 1, now.Add(-time.Minute).Truncate(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := a.systemAliveReceived(types.MessageTimestamp{Created: now, Received: now}, true); err != nil {
		t.Fatal(err)
	}
	afterTheCap(t, hits, time.Time{})
	waitDetachedDone(t, a, 3*time.Second)
}

// A producer disabled in the middle of a recovery is not asked for its
// snapshot again when the deadline passes.
func TestActor_SnapshotCompleteDeadline_DisabledProducerIsNotReissued(t *testing.T) {
	srv, hits := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActor(t, srv, newFakeManagerOps())

	id := startRecovery(t, a, hits, time.Time{}, 1)
	start := a.currentRecovery.recoveryStartedAt
	if err := a.pm.SetProducerState(t.Context(), 1, false); err != nil {
		t.Fatalf("SetProducerState: %v", err)
	}

	a.onTick(start.Add(6*time.Minute), false)
	a.onTick(start.Add(12*time.Minute), false)
	if a.currentRecovery == nil || a.currentRecovery.recoveryID != id {
		t.Fatal("a disabled producer's recovery was re-issued after the deadline")
	}
	time.Sleep(50 * time.Millisecond)
	if got := hits.recover.Load(); got != 1 {
		t.Fatalf("snapshot POSTs = %d, want 1", got)
	}
}

// A re-issue held back by a lost session that has not re-bound its queue
// waits (the replay would go nowhere); it happens once the rebind lands.
func TestActor_SnapshotCompleteDeadline_WaitsForPendingRebind(t *testing.T) {
	fake := newFakeManagerOps()
	srv, hits := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActor(t, srv, fake)

	id := startRecovery(t, a, hits, time.Time{}, 1)
	start := a.currentRecovery.recoveryStartedAt

	fake.pendingRebind.Store(true)
	a.onTick(start.Add(6*time.Minute), false)
	if a.currentRecovery.recoveryID != id {
		t.Fatal("re-issued while a lost session in scope has no queue")
	}
	fake.pendingRebind.Store(false)
	a.onTick(start.Add(7*time.Minute), false)
	if a.currentRecovery.recoveryID == id {
		t.Fatal("not re-issued after the rebind")
	}
	waitDetachedDone(t, a, 3*time.Second)
}

// The effective deadline never exceeds MaxRecoveryExecution, even for a
// config that does not enforce the ordering (internal wiring).
func TestActor_SnapshotCompleteDeadline_CappedByMaxRecoveryExecution(t *testing.T) {
	srv, _ := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActorWithCfg(t, srv, newFakeManagerOps(), 1, func(m *minimalCfg) config.Config {
		return &deadlineCfg{minimalCfg: m, maxRecovery: time.Minute, deadline: time.Hour}
	})
	if got := a.snapshotCompleteDeadline(); got != time.Minute {
		t.Fatalf("effective deadline = %v, want capped to 1m", got)
	}
}

// TestManager_OnRecoveryMessageReceived_RenewsTheActor: the session-facing
// hook reaches the producer's actor.
func TestManager_OnRecoveryMessageReceived_RenewsTheActor(t *testing.T) {
	m, _ := openedManagerWithActors(t, 1)
	actorOf(t, m, 1).publishSnapshotRequest(77) // the actor's current snapshot request
	m.OnRecoveryMessageReceived(1, 77)
	mark := actorOf(t, m, 1).recoveryMark.Load()
	if mark == nil || mark.requestID != 77 || mark.seenAt().IsZero() {
		t.Fatalf("actor mark = %+v, want request 77 stamped", mark)
	}
}

// noteRecoveryMessage is called from every session goroutine while the
// actor reads the mark on its tick: the latest observation of a request id
// wins and an older one never replaces it.
func TestActor_NoteRecoveryMessage_ConcurrentLatestWins(t *testing.T) {
	a := newTestActor(t, newFakeManagerOps())
	a.publishSnapshotRequest(5)
	base := time.Unix(10_000, 0)

	const writers, perWriter = 8, 200
	done := make(chan struct{})
	for w := range writers {
		go func() {
			defer func() { done <- struct{}{} }()
			for i := range perWriter {
				a.noteRecoveryMessage(5, base.Add(time.Duration(w*perWriter+i)*time.Millisecond))
				_ = a.recoveryMark.Load()
			}
		}()
	}
	for range writers {
		<-done
	}
	want := base.Add(time.Duration(writers*perWriter-1) * time.Millisecond)
	if m := a.recoveryMark.Load(); m == nil || m.requestID != 5 || !m.seenAt().Equal(want) {
		t.Fatalf("mark = %+v, want request 5 at %v", m, want)
	}
	// An older observation of the same request never replaces it.
	a.noteRecoveryMessage(5, base)
	if m := a.recoveryMark.Load(); !m.seenAt().Equal(want) {
		t.Fatalf("an older message replaced the mark: %v", m.seenAt())
	}
}

// Only the CURRENT snapshot request's messages count. A message of another
// request id (an event recovery's replay, say) arriving shortly after a
// current-request message used to replace the single mark, so the next tick
// found the current recovery silent and re-issued one that was still
// delivering.
func TestActor_SnapshotCompleteDeadline_OtherRequestIDsDoNotEraseTheMark(t *testing.T) {
	srv, hits := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActor(t, srv, newFakeManagerOps())

	id := startRecovery(t, a, hits, time.Time{}, 1)
	started := a.currentRecovery.recoveryStartedAt

	a.noteRecoveryMessage(id, started.Add(time.Minute))
	a.noteRecoveryMessage(id+100, started.Add(2*time.Minute))  // another request's message, later
	a.onTick(started.Add(5*time.Minute+50*time.Second), false) // 4m50s since the current one's message
	if a.currentRecovery.recoveryID != id || hits.recover.Load() != 1 {
		t.Fatalf("a live recovery was re-issued after another request's message: id=%d posts=%d", a.currentRecovery.recoveryID, hits.recover.Load())
	}
	if m := a.recoveryMark.Load(); m == nil || m.requestID != id || !m.seenAt().Equal(started.Add(time.Minute)) {
		t.Fatalf("mark = %+v, want the current request's, stamped by its own message", m)
	}
}

// After a re-issue a late message of the abandoned request must not erase
// the new request's mark.
func TestActor_SnapshotCompleteDeadline_LateMessageOfAbandonedRequestDoesNotEraseTheNewMark(t *testing.T) {
	srv, hits := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActor(t, srv, newFakeManagerOps())

	oldID := startRecovery(t, a, hits, time.Time{}, 1)
	started := a.currentRecovery.recoveryStartedAt
	a.onTick(started.Add(6*time.Minute), false) // re-issue
	newID := a.currentRecovery.recoveryID
	if newID == oldID {
		t.Fatal("setup: no re-issue")
	}
	waitRecoverHits(t, hits, 2)
	s2 := a.currentRecovery.recoveryStartedAt

	a.noteRecoveryMessage(newID, s2.Add(time.Minute))
	a.noteRecoveryMessage(oldID, s2.Add(2*time.Minute)) // late message of the abandoned request
	a.onTick(s2.Add(5*time.Minute+50*time.Second), false)
	if a.currentRecovery.recoveryID != newID || hits.recover.Load() != 2 {
		t.Fatalf("new recovery re-issued after a late old message: id=%d posts=%d", a.currentRecovery.recoveryID, hits.recover.Load())
	}
	waitDetachedDone(t, a, 3*time.Second)
}

// A caller of the abandoned request can pass the id check and pause while the
// actor installs the replacement. Its write must land in the abandoned
// request's own mark: it used to be able to overwrite the new request's mark,
// which made a live recovery look silent.
func TestActor_NoteRecoveryMessage_PausedCallerOfAnOldRequestCannotTouchTheNewMark(t *testing.T) {
	srv, hits := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActor(t, srv, newFakeManagerOps())

	oldID := startRecovery(t, a, hits, time.Time{}, 1)
	started := a.currentRecovery.recoveryStartedAt

	// The caller has passed the id check; before it writes, the deadline
	// re-issues the recovery and the new request's first message arrives.
	var newMarkAt time.Time
	a.noteGateHook = func() {
		a.noteGateHook = nil
		a.onTick(started.Add(6*time.Minute), false)
		if a.currentRecovery.recoveryID == oldID {
			t.Error("setup: no re-issue")
			return
		}
		newMarkAt = a.currentRecovery.recoveryStartedAt.Add(time.Minute)
		a.noteRecoveryMessage(a.currentRecovery.recoveryID, newMarkAt)
	}
	late := started.Add(30 * time.Minute)
	a.noteRecoveryMessage(oldID, late) // paused in the hook, then writes

	m := a.recoveryMark.Load()
	if m == nil || m.requestID != a.currentRecovery.recoveryID || !m.seenAt().Equal(newMarkAt) {
		t.Fatalf("new request's mark = %+v, want its own stamp %v untouched by the old caller", m, newMarkAt)
	}
	waitRecoverHits(t, hits, 2)
	waitDetachedDone(t, a, 3*time.Second)
}

// A re-issue while the full-history floor stands asks for the full history,
// whatever the original request's point was: the floor is the strongest one
// and a timestamp cannot express it.
func TestActor_SnapshotCompleteDeadline_ReissueUnderTheFullHistoryFloorHasNoAfter(t *testing.T) {
	srv, hits := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActor(t, srv, newFakeManagerOps())

	from := time.Now().Add(-10 * time.Minute).Truncate(time.Millisecond)
	oldID := startRecovery(t, a, hits, from, 1)
	if got := hits.lastAfterMillis.Load(); got != from.UnixMilli() {
		t.Fatalf("setup: first request after=%d, want %d", got, from.UnixMilli())
	}
	started := a.currentRecovery.recoveryStartedAt

	a.recoveryFloorFull = true // e.g. left by an interrupted full-history recovery
	a.onTick(started.Add(6*time.Minute), false)
	if a.currentRecovery.recoveryID == oldID {
		t.Fatal("setup: no re-issue")
	}
	waitRecoverHits(t, hits, 2)
	if got := hits.lastAfterMillis.Load(); got != 0 {
		t.Fatalf("re-issue after=%d (%v), want none (the full history)", got, time.UnixMilli(got))
	}
	if !a.currentRecovery.recoverFrom.IsZero() {
		t.Fatalf("re-issued recoverFrom = %v, want zero", a.currentRecovery.recoverFrom)
	}
	waitDetachedDone(t, a, 3*time.Second)
}

// A re-issue that fails to go out leaves the old recovery as it was (its id,
// its chain start, its state) and the next tick tries again.
func TestActor_SnapshotCompleteDeadline_FailedReissueLeavesTheOldRecoveryAndRetries(t *testing.T) {
	srv, hits := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActor(t, srv, newFakeManagerOps())

	from := time.Now().Add(-10 * time.Minute).Truncate(time.Millisecond)
	oldID := startRecovery(t, a, hits, from, 1)
	old := a.currentRecovery
	started := old.recoveryStartedAt
	chain := old.chainStartedAt

	// The request cannot be made: the producer's name is unknown.
	a.lastProcessedFn = func() (time.Time, error) { return time.Time{}, nil }
	realID := a.producerID
	a.producerID = 9999
	a.onTick(started.Add(6*time.Minute), false)
	if a.currentRecovery != old || a.currentRecovery.recoveryID != oldID || !a.currentRecovery.chainStartedAt.Equal(chain) ||
		a.recoveryState != types.StartedRecoveryState {
		t.Fatalf("a failed re-issue changed the recovery: same=%v id=%d chain=%v state=%v",
			a.currentRecovery == old, a.currentRecovery.recoveryID, a.currentRecovery.chainStartedAt, a.recoveryState)
	}
	if hits.recover.Load() != 1 {
		t.Fatalf("recovery POSTs = %d, want 1", hits.recover.Load())
	}

	// The next tick retries, and the replacement keeps the chain start.
	a.producerID = realID
	a.onTick(started.Add(6*time.Minute+10*time.Second), false)
	if a.currentRecovery.recoveryID == oldID {
		t.Fatal("the next tick did not retry the re-issue")
	}
	if !a.currentRecovery.chainStartedAt.Equal(chain) {
		t.Fatalf("chain start = %v, want the original %v", a.currentRecovery.chainStartedAt, chain)
	}
	waitRecoverHits(t, hits, 2)
	waitDetachedDone(t, a, 3*time.Second)
}

// waitUntilOlderThan blocks until ts is more than d in the past by the real
// clock. The lookback limit moves with the clock, so these tests have to let
// real time pass; waiting for the condition itself (instead of sleeping a
// fixed time against a margin) keeps them independent of scheduling speed.
func waitUntilOlderThan(t *testing.T, ts time.Time, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d + 10*time.Second)
	for time.Since(ts) <= d {
		if time.Now().After(deadline) {
			t.Fatal("clock did not advance")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// lookbackActor is a wired actor with a 2 s lookback limit and a 1 s
// snapshot-complete deadline, plus a first recovery started from a cursor
// inside the limit. The expectations below derive from what that first
// request actually RESOLVED, not from what was asked for.
func lookbackActor(t *testing.T) (*recoveryActor, *recoveryHits, time.Time, time.Time, int) {
	t.Helper()
	srv, hits := fixtureSrv(t)
	t.Cleanup(srv.Close)
	a := newWiredActorWithCfg(t, srv, newFakeManagerOps(), 1, func(m *minimalCfg) config.Config {
		return &deadlineCfg{minimalCfg: m, maxRecovery: 2 * time.Second, deadline: time.Second}
	})
	asked := time.Now().Add(-time.Second).Truncate(time.Millisecond)
	id := startRecovery(t, a, hits, asked, 1)
	resolved := a.currentRecovery.recoverFrom
	if resolved.IsZero() || resolved.After(asked.Add(time.Second)) {
		t.Fatalf("setup: resolved recoverFrom = %v", resolved)
	}
	return a, hits, resolved, a.currentRecovery.recoveryStartedAt, id
}

// The re-issue keeps the ORIGINAL resolved recover-from point. Clamping it
// again to now-MaxRecoveryExecution (a moving limit) would start the
// replacement later than the request it replaces and skip the messages in
// between, whose replay the abandoned request can no longer deliver.
func TestActor_SnapshotCompleteDeadline_ReissueKeepsTheOriginalRecoverFromAtTheLookbackLimit(t *testing.T) {
	a, hits, resolved, started, id := lookbackActor(t)

	// Real time moves on until a fresh clamp WOULD move the same cursor.
	waitUntilOlderThan(t, resolved, 2*time.Second)
	a.onTick(started.Add(1500*time.Millisecond), false)
	if a.currentRecovery.recoveryID == id {
		t.Fatal("setup: no re-issue")
	}
	waitRecoverHits(t, hits, 2)
	if got := a.currentRecovery.recoverFrom; !got.Equal(resolved) {
		t.Fatalf("re-issued recoverFrom = %v, want the original %v (moved forward by %v)", got, resolved, got.Sub(resolved))
	}
	if got := hits.lastAfterMillis.Load(); got != resolved.UnixMilli() {
		t.Fatalf("re-issued request after=%d, want %d", got, resolved.UnixMilli())
	}
	waitDetachedDone(t, a, 3*time.Second)
}

// A floor lowered since the original request, and OLDER than the lookback
// limit, is clamped against the clock when the re-issue is built; by then
// that clamped point can lie after the original cursor. The re-issue must
// never start later than the original.
func TestActor_SnapshotCompleteDeadline_ReissueWithAFloorBeyondTheLimitNeverStartsLaterThanTheOriginal(t *testing.T) {
	a, hits, resolved, started, id := lookbackActor(t)

	a.lowerRecoveryFloor(resolved.Add(-time.Hour)) // far beyond the limit
	waitUntilOlderThan(t, resolved, 2*time.Second)
	a.onTick(started.Add(1500*time.Millisecond), false)
	if a.currentRecovery.recoveryID == id {
		t.Fatal("setup: no re-issue")
	}
	waitRecoverHits(t, hits, 2)
	// The floor, clamped against the clock now, lies after the original
	// cursor: the original is what the re-issue starts from.
	if got := a.currentRecovery.recoverFrom; !got.Equal(resolved) {
		t.Fatalf("re-issued recoverFrom = %v, want the original %v (later by %v)", got, resolved, got.Sub(resolved))
	}
	if got := hits.lastAfterMillis.Load(); got != resolved.UnixMilli() {
		t.Fatalf("re-issued request after=%d, want %d", got, resolved.UnixMilli())
	}
	waitDetachedDone(t, a, 3*time.Second)
}

// A floor lowered since the original request still pulls the re-issue back.
func TestActor_SnapshotCompleteDeadline_ReissueHonoursALowerFloor(t *testing.T) {
	srv, hits := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActor(t, srv, newFakeManagerOps())

	from := time.Now().Add(-10 * time.Minute).Truncate(time.Millisecond)
	startRecovery(t, a, hits, from, 1)
	floor := from.Add(-5 * time.Minute)
	a.lowerRecoveryFloor(floor)
	a.onTick(a.currentRecovery.recoveryStartedAt.Add(6*time.Minute), false)
	waitRecoverHits(t, hits, 2)
	if got := hits.lastAfterMillis.Load(); got != floor.UnixMilli() {
		t.Fatalf("re-issue after=%d, want the lowered floor %d", got, floor.UnixMilli())
	}
	waitDetachedDone(t, a, 3*time.Second)
}

// A deadline re-issue of an INTERRUPTED recovery (a subscribed=false alive
// marked it: its replay may be lost) keeps it interrupted: the replacement
// completes through the interrupted path, which issues the fresh recovery the
// interruption requires before the producer is reported up. Resetting it to
// Started would report the producer up on a replay that may be missing its
// head.
func TestActor_SnapshotCompleteDeadline_ReissueKeepsAnInterruptedRecoveryInterrupted(t *testing.T) {
	fake := newFakeManagerOps()
	srv, hits := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActor(t, srv, fake)

	r1 := startRecovery(t, a, hits, time.Time{}, 1)
	if err := a.systemAliveReceived(aliveAt(time.Now()), false); err != nil {
		t.Fatal(err)
	}
	if a.recoveryState != types.InterruptedRecoveryState {
		t.Fatalf("setup: state = %v, want Interrupted", a.recoveryState)
	}
	started := a.currentRecovery.recoveryStartedAt

	a.onTick(started.Add(6*time.Minute), false)
	r2 := a.currentRecovery.recoveryID
	if r2 == r1 {
		t.Fatal("setup: no re-issue")
	}
	if a.recoveryState != types.InterruptedRecoveryState {
		t.Fatalf("state after the re-issue = %v, want Interrupted kept", a.recoveryState)
	}
	waitRecoverHits(t, hits, 2)

	// Its completion takes the interrupted path: a fresh recovery, producer
	// still down.
	eventsBefore := len(producerStatusEvents(fake))
	if err := a.snapshotRecoveryFinished(r2); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 3)
	if r3 := a.currentRecovery.recoveryID; r3 == r2 || a.recoveryState != types.StartedRecoveryState {
		t.Fatalf("after R2: id=%d state=%v, want a fresh Started recovery", r3, a.recoveryState)
	}
	if !a.isFlaggedDown() || len(producerStatusEvents(fake)) != eventsBefore {
		t.Fatal("producer reported up before the fresh recovery completed")
	}
	waitDetachedDone(t, a, 3*time.Second)
}

// procClock replaces the producer manager's last processed timestamp (the
// lookup seam of the actor) with one a test sets, so that "processing lags"
// and "processing is current" do not depend on the monotonic cursor of the
// manager or on the wall clock a completion reads.
type procClock struct{ v atomic.Int64 }

func (p *procClock) set(t time.Time) { p.v.Store(t.UnixNano()) }

// holdProcessing installs a procClock on the actor, behind by a minute.
func holdProcessing(a *recoveryActor) *procClock {
	p := &procClock{}
	p.set(time.Now().Add(-time.Minute))
	a.lastProcessedFn = func() (time.Time, error) { return time.Unix(0, p.v.Load()), nil }
	return p
}

// TestActor_SnapshotCompleteDeadline_AFirstMessageThatEndedWithoutATimestampIsNotInProcessing:
// the session ends a message whose build or downstream send failed with a
// ZERO timestamp. Nothing is processed, so there is still no last-processed
// timestamp, but the message is out of processing: the silence of a lost
// snapshot request is evidence again, and the request is re-issued at the
// deadline instead of waiting for the recovery cap. Driven through the
// manager's callbacks, the way the session calls them.
func TestActor_SnapshotCompleteDeadline_AFirstMessageThatEndedWithoutATimestampIsNotInProcessing(t *testing.T) {
	srv, hits := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActor(t, srv, newFakeManagerOps())
	m := &Manager{
		logger:          newDiscardLogger(),
		actors:          map[int]*recoveryActor{a.producerID: a},
		processingTimes: map[uuid.UUID]time.Time{},
	}
	m.state.Store(mgrStateOpen)

	oldID := startRecovery(t, a, hits, time.Now().Add(-10*time.Minute).Truncate(time.Millisecond), 1)
	started := a.currentRecovery.recoveryStartedAt

	sid := uuid.New()
	m.OnMessageProcessingStarted(sid, a.producerID, time.Now())
	// While the message is in processing the silence is no evidence.
	a.onTick(started.Add(6*time.Minute), false)
	time.Sleep(50 * time.Millisecond)
	if a.currentRecovery.recoveryID != oldID || hits.recover.Load() != 1 {
		t.Fatalf("re-issued while the first message was in processing: id=%d posts=%d", a.currentRecovery.recoveryID, hits.recover.Load())
	}

	// The message's build fails: it ends with a zero timestamp.
	m.OnMessageProcessingEnded(sid, a.producerID, time.Time{})
	if ts, _ := a.lastProcessedMessageGenTimestamp(); !ts.IsZero() {
		t.Fatalf("test setup: last processed = %v, want zero", ts)
	}
	// The deadline counts from the next tick, then the silence is evidence.
	// (The tick in processing moved the mark to +6m.)
	a.onTick(started.Add(8*time.Minute), false)
	if a.currentRecovery.recoveryID != oldID {
		t.Fatal("re-issued right after the message ended, before a full deadline of silence")
	}
	a.onTick(started.Add(12*time.Minute), false)
	if a.currentRecovery.recoveryID == oldID {
		t.Fatal("the first message ended without a timestamp and the silence went on, but the request was not re-issued")
	}
	waitRecoverHits(t, hits, 2)
	waitDetachedDone(t, a, 3*time.Second)
}

// TestActor_SnapshotCompleteDeadline_AStaleCursorWithoutProcessingIsNoBacklog:
// an old last-processed timestamp alone is not evidence of a backlog. With
// nothing in processing and no message started recently, a lost request is
// re-issued at the deadline instead of waiting for MaxRecoveryExecution.
func TestActor_SnapshotCompleteDeadline_AStaleCursorWithoutProcessingIsNoBacklog(t *testing.T) {
	srv, hits := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActor(t, srv, newFakeManagerOps())

	oldID := startRecovery(t, a, hits, time.Now().Add(-10*time.Minute).Truncate(time.Millisecond), 1)
	holdProcessing(a) // the cursor stays a minute behind: stale
	a.onTick(a.currentRecovery.recoveryStartedAt.Add(6*time.Minute), false)
	if a.currentRecovery.recoveryID == oldID {
		t.Fatal("stale cursor but no processing activity, silent past the deadline: not re-issued")
	}
	waitRecoverHits(t, hits, 2)
	waitDetachedDone(t, a, 3*time.Second)
}

// TestActor_SnapshotCompleteDeadline_AStaleCursorWithMessagesStartingIsABacklog:
// the same stale cursor while messages keep starting (each one ends at once,
// so none is in processing at the tick) is a backlog being worked off: no
// re-issue. Once the starts stop for longer than MaxInactivity the silence
// counts again.
func TestActor_SnapshotCompleteDeadline_AStaleCursorWithMessagesStartingIsABacklog(t *testing.T) {
	srv, hits := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActor(t, srv, newFakeManagerOps())

	oldID := startRecovery(t, a, hits, time.Now().Add(-10*time.Minute).Truncate(time.Millisecond), 1)
	holdProcessing(a)
	// The recovery has been silent for longer than the deadline.
	a.currentRecovery.recoveryStartedAt = time.Now().Add(-6 * time.Minute)

	a.onMessageProcessingStarted(time.Time{})
	a.onMessageProcessingEnded(time.Time{})
	tick := time.Now()
	a.onTick(tick, false)
	time.Sleep(50 * time.Millisecond)
	if a.currentRecovery.recoveryID != oldID || hits.recover.Load() != 1 {
		t.Fatalf("re-issued while messages were starting: id=%d (was %d) posts=%d", a.currentRecovery.recoveryID, oldID, hits.recover.Load())
	}

	// The starts stopped; a full deadline later the silence is evidence.
	a.onTick(tick.Add(6*time.Minute), false)
	if a.currentRecovery.recoveryID == oldID {
		t.Fatal("no message started for a deadline, but not re-issued")
	}
	waitRecoverHits(t, hits, 2)
	waitDetachedDone(t, a, 3*time.Second)
}

// TestActor_SnapshotCompleteDeadline_InProcessingCountsAcrossSessions: the
// guard counts messages per producer over all sessions. One session ending
// its message (with a zero timestamp) does not hide another's message that
// is still in processing; the request is re-issued only after the last one
// ended and a further deadline of silence passed. An end without a start
// leaves the count at zero, not negative, so a later start counts as one.
// Driven through the manager's callbacks, the way the sessions call them.
func TestActor_SnapshotCompleteDeadline_InProcessingCountsAcrossSessions(t *testing.T) {
	srv, hits := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActor(t, srv, newFakeManagerOps())
	m := &Manager{
		logger:          newDiscardLogger(),
		actors:          map[int]*recoveryActor{a.producerID: a},
		processingTimes: map[uuid.UUID]time.Time{},
	}
	m.state.Store(mgrStateOpen)

	oldID := startRecovery(t, a, hits, time.Now().Add(-10*time.Minute).Truncate(time.Millisecond), 1)
	started := a.currentRecovery.recoveryStartedAt

	first, second := uuid.New(), uuid.New()
	m.OnMessageProcessingStarted(first, a.producerID, time.Now())
	m.OnMessageProcessingStarted(second, a.producerID, time.Now())
	if ts, _ := a.lastProcessedMessageGenTimestamp(); !ts.IsZero() {
		t.Fatalf("test setup: last processed = %v, want zero", ts)
	}
	if got := a.inProcessing.Load(); got != 2 {
		t.Fatalf("in processing = %d, want 2", got)
	}

	notReissued := func(at time.Duration, why string) {
		t.Helper()
		a.onTick(started.Add(at), false)
		time.Sleep(50 * time.Millisecond)
		if a.currentRecovery.recoveryID != oldID || hits.recover.Load() != 1 {
			t.Fatalf("re-issued %s: id=%d (was %d) posts=%d", why, a.currentRecovery.recoveryID, oldID, hits.recover.Load())
		}
	}
	notReissued(6*time.Minute, "with two messages in processing")

	// One session ends its message without a timestamp; the other's is
	// still in processing, so the silence is still no evidence.
	m.OnMessageProcessingEnded(first, a.producerID, time.Time{})
	if got := a.inProcessing.Load(); got != 1 {
		t.Fatalf("in processing = %d after one end, want 1", got)
	}
	notReissued(12*time.Minute, "while the second session's message was in processing")

	// The second ends too: the deadline counts from the next tick.
	m.OnMessageProcessingEnded(second, a.producerID, time.Time{})
	if got := a.inProcessing.Load(); got != 0 {
		t.Fatalf("in processing = %d after both ended, want 0", got)
	}
	notReissued(14*time.Minute, "right after the last message ended, before a full deadline of silence")
	a.onTick(started.Add(20*time.Minute), false)
	if a.currentRecovery.recoveryID == oldID {
		t.Fatal("nothing in processing and a further deadline of silence, but not re-issued")
	}
	waitRecoverHits(t, hits, 2)
	waitDetachedDone(t, a, 3*time.Second)

	// An extra end is clamped at zero, never negative.
	m.OnMessageProcessingEnded(uuid.New(), a.producerID, time.Time{})
	if got := a.inProcessing.Load(); got != 0 {
		t.Fatalf("in processing = %d after an end without a start, want 0", got)
	}
	m.OnMessageProcessingStarted(uuid.New(), a.producerID, time.Now())
	if got := a.inProcessing.Load(); got != 1 {
		t.Fatalf("in processing = %d after a start following an extra end, want 1", got)
	}
}

// TestActor_SnapshotCompleteDeadline_OnlySystemAlivesIsNoClientProcessing pins
// the actor's side of the alive-only session: an open client session has the
// producer in scope but processes nothing, while system alives (what the
// SDK's own alive-only session delivers) keep arriving. Such alives are not
// client processing: they leave the in-processing count and the processing
// guard idle, so the silence of a lost request is evidence and it is
// re-issued at the deadline. The other side, that the session does not
// report system alives as processing in the first place, is pinned by the
// session test TestSession_ProcessFeedMessage_SystemAliveSessionIsNotClientProcessing;
// this test calls onAlive directly and does not go through the session.
func TestActor_SnapshotCompleteDeadline_OnlySystemAlivesIsNoClientProcessing(t *testing.T) {
	srv, hits := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActor(t, srv, newFakeManagerOps())
	// An open client session that has this producer in scope but has not
	// processed anything: the replay can reach it, so the deadline applies.
	a.mgr = clientScopeOps(a, types.LiveOnlyMessageInterest)

	oldID := startRecovery(t, a, hits, time.Now().Add(-10*time.Minute).Truncate(time.Millisecond), 1)
	started := a.currentRecovery.recoveryStartedAt
	// The last user-session alive is a minute old; the processed cursor too.
	a.lastUserSessionAlive = started.Add(-time.Minute)
	holdProcessing(a)

	// System alives keep arriving; they are not processing.
	a.onAlive(evAlive{
		timestamp:       types.MessageTimestamp{Created: started.Add(5 * time.Minute), Received: started.Add(5 * time.Minute)},
		isSubscribed:    true,
		messageInterest: types.SystemAliveOnly,
	})
	if a.inProcessing.Load() != 0 || a.processingActive(started.Add(6*time.Minute)) {
		t.Fatal("system alives made the client look busy")
	}

	a.onTick(started.Add(6*time.Minute), false)
	if a.currentRecovery.recoveryID == oldID {
		t.Fatal("no client processing and the user alive stale, silent past the deadline: not re-issued")
	}
	waitRecoverHits(t, hits, 2)
	waitDetachedDone(t, a, 3*time.Second)
}

// TestActor_SnapshotCompleteDeadline_LateSnapshotCompleteOfTheOldIDRenewsNothing:
// only a snapshot_complete of the CURRENT request is a sign of life. The late
// one of an abandoned request arrives just before a tick; it must not renew
// the deadline of the new request, which has been silent for longer than it.
func TestActor_SnapshotCompleteDeadline_LateSnapshotCompleteOfTheOldIDRenewsNothing(t *testing.T) {
	srv, hits := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActor(t, srv, newFakeManagerOps())

	oldID := startRecovery(t, a, hits, time.Now().Add(-10*time.Minute).Truncate(time.Millisecond), 1)
	a.onTick(a.currentRecovery.recoveryStartedAt.Add(6*time.Minute), false)
	newID := a.currentRecovery.recoveryID
	if newID == oldID {
		t.Fatal("setup: the request was not re-issued")
	}
	waitRecoverHits(t, hits, 2)
	waitDetachedDone(t, a, 3*time.Second)
	// The new request went out 6 minutes ago and nothing was heard since.
	a.currentRecovery.recoveryStartedAt = time.Now().Add(-6 * time.Minute)

	a.onSnapshotComplete(evSnapshotComplete{requestID: oldID, messageInterest: types.LiveOnlyMessageInterest})
	if !a.currentRecovery.renewedAt.IsZero() {
		t.Fatalf("a snapshot_complete of the old id renewed the new request: %v", a.currentRecovery.renewedAt)
	}
	a.onTick(time.Now().Add(time.Second), false)
	if a.currentRecovery.recoveryID == newID {
		t.Fatal("the late snapshot_complete of the old id kept the silent new request from being re-issued")
	}
	waitRecoverHits(t, hits, 3)
	waitDetachedDone(t, a, 3*time.Second)
}

// failedReissue starts a recovery from `from`, lets the deadline re-issue it
// and then delivers the error of the re-issue's detached POST, as the actor
// loop would when the request fails after its retries. Returns the actor and
// the hit counter of the fixture server.
func failedReissue(t *testing.T, from time.Time) (*recoveryActor, *recoveryHits) {
	t.Helper()
	srv, hits := fixtureSrv(t)
	t.Cleanup(srv.Close)
	a := newWiredActor(t, srv, newFakeManagerOps())

	oldID := startRecovery(t, a, hits, from, 1)
	a.onTick(a.currentRecovery.recoveryStartedAt.Add(6*time.Minute), false)
	newID := a.currentRecovery.recoveryID
	if newID == oldID {
		t.Fatal("setup: the request was not re-issued")
	}
	waitRecoverHits(t, hits, 2)
	waitDetachedDone(t, a, 3*time.Second)

	a.onSnapshotRecoveryAPICompleted(evSnapshotRecoveryAPICompleted{
		requestID: newID,
		err:       errors.New("recovery API unavailable"),
	})
	if a.currentRecovery != nil || a.recoveryState != types.ErrorRecoveryState {
		t.Fatalf("setup: failed POST left recovery=%v state=%v", a.currentRecovery, a.recoveryState)
	}
	return a, hits
}

// A re-issue whose POST fails later than the re-issue itself clears the
// recovery; the next alive's request must still start at the point the
// chain began with, not at a cursor clamped against a later "now". Here the
// cursor of the next alive is newer than the original point.
func TestActor_SnapshotCompleteDeadline_FailedReissuePostKeepsTheOriginalPoint(t *testing.T) {
	from := time.Now().Add(-10 * time.Minute).Truncate(time.Millisecond)
	a, hits := failedReissue(t, from)

	if err := a.makeSnapshotRecovery(time.Now().Add(-time.Minute).Truncate(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 3)
	if got := hits.lastAfterMillis.Load(); got != from.UnixMilli() {
		t.Fatalf("recovery after the failed re-issue: after=%d (%v), want the original %v",
			got, time.UnixMilli(got), from)
	}
	waitDetachedDone(t, a, 3*time.Second)
}

// The same for a request that covered the full history: no timestamp
// expresses it, so the next request is the full history again.
func TestActor_SnapshotCompleteDeadline_FailedReissuePostKeepsTheFullHistory(t *testing.T) {
	a, hits := failedReissue(t, time.Time{})

	if err := a.makeSnapshotRecovery(time.Now().Add(-time.Minute).Truncate(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	waitRecoverHits(t, hits, 3)
	if got := hits.lastAfterMillis.Load(); got != 0 {
		t.Fatalf("recovery after the failed re-issue: after=%d (%v), want none (the full history)",
			got, time.UnixMilli(got))
	}
	waitDetachedDone(t, a, 3*time.Second)
}

// scopeOps answers the client-session scope question from a real Manager
// registry, the rest from the fake.
type scopeOps struct {
	*fakeManagerOps
	m *Manager
}

func (s scopeOps) clientSessionsCanComplete(id int) bool { return s.m.clientSessionsCanComplete(id) }

func (s scopeOps) clientProcessingStalled(id int, now time.Time, after time.Duration) bool {
	return s.m.clientProcessingStalled(id, now, after)
}

// clientScopeOps makes the actor's manager report the given open client
// sessions, as the session registry of a real Manager would. The producer
// catalog is the actor's own, loaded by its first request.
func clientScopeOps(a *recoveryActor, interests ...types.MessageInterest) actorManagerOps {
	m := &Manager{producerManager: a.pm}
	for _, mi := range interests {
		m.OnClientSessionOpened(uuid.New(), mi)
	}
	// The actor's own fake keeps the request ids counting on.
	return scopeOps{fakeManagerOps: a.mgr.(*fakeManagerOps), m: m}
}

// reissuedAt ticks the actor at started+at and reports whether the request
// was replaced.
func reissuedAt(a *recoveryActor, started time.Time, at time.Duration) bool {
	id := a.currentRecovery.recoveryID
	a.onTick(started.Add(at), false)
	return a.currentRecovery.recoveryID != id
}

// A LiveOnly client drops a prematch producer's messages, snapshot_complete
// included, so its replay can never renew or complete the recovery. The
// deadline must not ask the recovery API again for it, deadline after
// deadline: such a recovery waits for MaxRecoveryExecution as it always did.
func TestActor_SnapshotCompleteDeadline_NoReissueForAProducerOutOfEveryClientScope(t *testing.T) {
	srv, hits := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActorForProducer(t, srv, newFakeManagerOps(), 2) // prematch
	oldID := startRecovery(t, a, hits, time.Now().Add(-10*time.Minute).Truncate(time.Millisecond), 1)
	a.mgr = clientScopeOps(a, types.LiveOnlyMessageInterest)
	started := a.currentRecovery.recoveryStartedAt

	for _, at := range []time.Duration{6 * time.Minute, 12 * time.Minute, 18 * time.Minute} {
		if reissuedAt(a, started, at) {
			t.Fatalf("re-issued at +%v for a producer no client session has in scope", at)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if a.currentRecovery.recoveryID != oldID || hits.recover.Load() != 1 {
		t.Fatalf("recovery POSTs = %d, id %d (was %d); want one request, untouched", hits.recover.Load(), a.currentRecovery.recoveryID, oldID)
	}
	if a.recoveryState != types.StartedRecoveryState {
		t.Fatalf("state = %v, want Started", a.recoveryState)
	}
}

// A client that connected and never subscribed has only the SDK's own
// alive-only session; no snapshot_complete reaches it either.
func TestActor_SnapshotCompleteDeadline_NoReissueWithoutAnyClientSession(t *testing.T) {
	srv, hits := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActor(t, srv, newFakeManagerOps())
	startRecovery(t, a, hits, time.Now().Add(-10*time.Minute).Truncate(time.Millisecond), 1)
	a.mgr = clientScopeOps(a) // no client session
	started := a.currentRecovery.recoveryStartedAt

	for _, at := range []time.Duration{6 * time.Minute, 12 * time.Minute} {
		if reissuedAt(a, started, at) {
			t.Fatalf("re-issued at +%v with no client session open", at)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if hits.recover.Load() != 1 {
		t.Fatalf("recovery POSTs = %d, want 1", hits.recover.Load())
	}
}

// A mixed live|prematch producer completes only with a snapshot_complete of
// each scope, so a LiveOnly session alone never finishes it: its replay
// arrives, the prematch half is dropped, and a re-request changes nothing.
func TestActor_SnapshotCompleteDeadline_NoReissueForAMixedProducerWithOnlyOneScopeOpen(t *testing.T) {
	for _, only := range []types.MessageInterest{types.LiveOnlyMessageInterest, types.PrematchOnlyMessageInterest} {
		srv, hits := fixtureSrv(t)
		a := newWiredActorForProducer(t, srv, newFakeManagerOps(), 3) // live|prematch
		oldID := startRecovery(t, a, hits, time.Now().Add(-10*time.Minute).Truncate(time.Millisecond), 1)
		a.mgr = clientScopeOps(a, only)
		started := a.currentRecovery.recoveryStartedAt

		for _, at := range []time.Duration{6 * time.Minute, 12 * time.Minute} {
			if reissuedAt(a, started, at) {
				t.Fatalf("%s only: re-issued at +%v for a recovery no open session can complete", only, at)
			}
		}
		time.Sleep(50 * time.Millisecond)
		if hits.recover.Load() != 1 || a.currentRecovery.recoveryID != oldID {
			t.Fatalf("%s only: recovery POSTs = %d, want 1", only, hits.recover.Load())
		}
		srv.Close()
	}
}

// The mixed producer is completable, and its deadline counts, once the
// sessions cover both scopes or one session takes every message.
func TestActor_SnapshotCompleteDeadline_ReissuesForAMixedProducerTheSessionsCanComplete(t *testing.T) {
	cases := map[string][]types.MessageInterest{
		"live and prematch": {types.LiveOnlyMessageInterest, types.PrematchOnlyMessageInterest},
		"all":               {types.AllMessageInterest},
		"live and hi":       {types.LiveOnlyMessageInterest, types.HiPriorityOnlyMessageInterest},
	}
	for name, interests := range cases {
		srv, hits := fixtureSrv(t)
		a := newWiredActorForProducer(t, srv, newFakeManagerOps(), 3)
		startRecovery(t, a, hits, time.Now().Add(-10*time.Minute).Truncate(time.Millisecond), 1)
		a.mgr = clientScopeOps(a, interests...)
		started := a.currentRecovery.recoveryStartedAt

		if !reissuedAt(a, started, 6*time.Minute) {
			t.Fatalf("%s: not re-issued although the sessions can complete the recovery", name)
		}
		waitRecoverHits(t, hits, 2)
		waitDetachedDone(t, a, 3*time.Second)
		srv.Close()
	}
}

// A session that covers the producer's scope keeps the deadline as before:
// a LiveOnly session and a live producer, then a mixed producer.
func TestActor_SnapshotCompleteDeadline_ReissuesForAProducerInAClientScope(t *testing.T) {
	for _, id := range []int{1, 3} {
		srv, hits := fixtureSrv(t)
		a := newWiredActorForProducer(t, srv, newFakeManagerOps(), id)
		startRecovery(t, a, hits, time.Now().Add(-10*time.Minute).Truncate(time.Millisecond), 1)
		a.mgr = clientScopeOps(a, types.PrematchOnlyMessageInterest, types.LiveOnlyMessageInterest)
		started := a.currentRecovery.recoveryStartedAt

		if !reissuedAt(a, started, 6*time.Minute) {
			t.Fatalf("producer %d: not re-issued although a session has it in scope", id)
		}
		waitRecoverHits(t, hits, 2)
		waitDetachedDone(t, a, 3*time.Second)
		srv.Close()
	}
}

// The registry the deadline reads: sessions are added when they open and
// removed when their consumer is gone; the SDK's own alive-only session is
// never one of them; a producer the catalog cannot describe counts as in
// scope.
func TestManager_ClientSessionInScope_TracksOpenSessions(t *testing.T) {
	m, _ := openedManagerWithActors(t, 1, 2, 3)
	if m.clientSessionsCanComplete(1) {
		t.Fatal("no client session is open, nothing is in scope")
	}
	m.OnClientSessionOpened(uuid.New(), types.SystemAliveOnly)
	if m.clientSessionsCanComplete(1) {
		t.Fatal("the SDK's alive-only session must not count as a client session")
	}
	live := uuid.New()
	m.OnClientSessionOpened(live, types.LiveOnlyMessageInterest)
	if !m.clientSessionsCanComplete(1) || m.clientSessionsCanComplete(3) || m.clientSessionsCanComplete(2) {
		t.Fatalf("LiveOnly session: completable 1=%v 2=%v 3=%v, want true/false/false (3 needs both scopes)",
			m.clientSessionsCanComplete(1), m.clientSessionsCanComplete(2), m.clientSessionsCanComplete(3))
	}
	if m.clientSessionsCanComplete(4242) {
		t.Fatal("an uncatalogued producer is described by the placeholder of both scopes: LiveOnly alone cannot complete it")
	}
	both := uuid.New()
	m.OnClientSessionOpened(both, types.PrematchOnlyMessageInterest)
	if !m.clientSessionsCanComplete(2) {
		t.Fatal("a prematch session must bring the prematch producer into scope")
	}
	if !m.clientSessionsCanComplete(3) {
		t.Fatal("LiveOnly and PrematchOnly sessions together must complete the mixed producer")
	}
	if !m.clientSessionsCanComplete(4242) {
		t.Fatal("both scopes open complete the placeholder producer too")
	}
	m.OnFeedSessionGone(uuid.New()) // unknown session: no effect
	m.OnFeedSessionGone(live)
	m.OnFeedSessionGone(both)
	if m.clientSessionsCanComplete(1) || m.clientSessionsCanComplete(2) {
		t.Fatal("sessions that are gone must leave the registry")
	}
}

// A catalog that cannot be read at all (not opened) cannot say what a
// producer needs: the deadline stays on, the conservative answer.
func TestManager_ClientSessionsCanComplete_UnreadableCatalogCounts(t *testing.T) {
	cfg := &minimalCfg{apiURL: "127.0.0.1:1", token: "tok"}
	pm := producer.NewManager(cfg, api.New(cfg), newDiscardLogger())
	m := &Manager{producerManager: pm}
	if m.clientSessionsCanComplete(1) {
		t.Fatal("no session is open")
	}
	m.OnClientSessionOpened(uuid.New(), types.LiveOnlyMessageInterest)
	if !m.clientSessionsCanComplete(1) {
		t.Fatal("a catalog that cannot be read must count as completable")
	}
}

// The guards of the deadline are per producer, but a session queue is shared:
// with the client handler stuck on a message of producer 2, producer 1's
// replay sits behind it although nothing of producer 1 is in processing.
// Re-issuing then only queues a second replay there, so the deadline is held
// while any client message is in processing and counts again once it ended.
// Driven through the manager's callbacks, the way the sessions call them.
func TestActor_SnapshotCompleteDeadline_HeldWhileAnyClientMessageIsInProcessing(t *testing.T) {
	srv, hits := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActor(t, srv, newFakeManagerOps()) // producer 1
	oldID := startRecovery(t, a, hits, time.Now().Add(-10*time.Minute).Truncate(time.Millisecond), 1)
	ops := clientScopeOps(a, types.LiveOnlyMessageInterest).(scopeOps)
	ops.m.processingTimes = map[uuid.UUID]time.Time{}
	ops.m.logger = newDiscardLogger()
	a.mgr = ops
	started := a.currentRecovery.recoveryStartedAt

	// Producer 2's message is stuck in the client's handler. Nothing of
	// producer 1 is in processing, and a processed cursor exists.
	session := uuid.New()
	const other = 2
	ops.m.OnMessageProcessingStarted(session, other, time.Now())
	a.lastProcessedFn = holdProcessingTS(time.Now().Add(-time.Hour))
	if a.inProcessing.Load() != 0 {
		t.Fatal("setup: producer 1 has a message in processing")
	}

	if reissuedAt(a, started, 6*time.Minute) {
		t.Fatalf("re-issued while a client message of producer %d was in processing", other)
	}
	if reissuedAt(a, started, 12*time.Minute) {
		t.Fatal("re-issued at a later tick while the message was still in processing")
	}
	time.Sleep(50 * time.Millisecond)
	if hits.recover.Load() != 1 || a.currentRecovery.recoveryID != oldID {
		t.Fatalf("recovery POSTs = %d, want 1", hits.recover.Load())
	}

	// The message ends; the deadline counts from the last tick that held it.
	ops.m.OnMessageProcessingEnded(session, other, time.Time{})
	if reissuedAt(a, started, 13*time.Minute) {
		t.Fatal("re-issued right after the message ended, before a full deadline of silence")
	}
	if !reissuedAt(a, started, 19*time.Minute) {
		t.Fatal("not re-issued after the message ended and a further deadline passed")
	}
	waitRecoverHits(t, hits, 2)
	waitDetachedDone(t, a, 3*time.Second)
}

// A busy client always has some message in processing; only one that has
// stayed in processing longer than MaxInactivity says the queue is stuck.
// A client whose messages each start and end within moments must not hold
// the deadline, or a lost snapshot_complete would wait for
// MaxRecoveryExecution on every busy client.
func TestActor_SnapshotCompleteDeadline_NotHeldByABusyClient(t *testing.T) {
	srv, hits := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActor(t, srv, newFakeManagerOps()) // producer 1
	startRecovery(t, a, hits, time.Now().Add(-10*time.Minute).Truncate(time.Millisecond), 1)
	ops := clientScopeOps(a, types.LiveOnlyMessageInterest).(scopeOps)
	ops.m.processingTimes = map[uuid.UUID]time.Time{}
	ops.m.logger = newDiscardLogger()
	a.mgr = ops
	started := a.currentRecovery.recoveryStartedAt
	a.lastProcessedFn = holdProcessingTS(time.Now().Add(-time.Hour))

	// At the tick a message of producer 2 is in processing, taken up a
	// second before: the client is busy, not stuck.
	const other = 2
	ops.m.OnMessageProcessingStarted(uuid.New(), other, started.Add(6*time.Minute-time.Second))
	if !reissuedAt(a, started, 6*time.Minute) {
		t.Fatal("not re-issued past the deadline although the message in processing had just started")
	}
	waitRecoverHits(t, hits, 2)
	waitDetachedDone(t, a, 3*time.Second)
}

// sessionScopeOps registers a LiveOnly and a PrematchOnly client session
// under known ids, so a test can leave a message of either in processing.
func sessionScopeOps(a *recoveryActor) (ops scopeOps, live, prematch uuid.UUID) {
	live, prematch = uuid.New(), uuid.New()
	ops = clientScopeOps(a).(scopeOps)
	ops.m.processingTimes = map[uuid.UUID]time.Time{}
	ops.m.logger = newDiscardLogger()
	ops.m.OnClientSessionOpened(live, types.LiveOnlyMessageInterest)
	ops.m.OnClientSessionOpened(prematch, types.PrematchOnlyMessageInterest)
	a.mgr = ops
	a.lastProcessedFn = holdProcessingTS(time.Now().Add(-time.Hour))
	return ops, live, prematch
}

// The queue of a session is its own: a LiveOnly session stuck on a live
// message does not hold back the replay of a prematch producer, which goes
// to the healthy PrematchOnly session. Holding on it would delay the
// re-request for as long as the unrelated session stays stuck.
func TestActor_SnapshotCompleteDeadline_NotHeldByAStalledSessionOutOfTheProducersScope(t *testing.T) {
	srv, hits := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActorForProducer(t, srv, newFakeManagerOps(), 2) // prematch
	startRecovery(t, a, hits, time.Now().Add(-10*time.Minute).Truncate(time.Millisecond), 1)
	ops, live, _ := sessionScopeOps(a)
	started := a.currentRecovery.recoveryStartedAt

	ops.m.OnMessageProcessingStarted(live, 1, started)
	if !reissuedAt(a, started, 6*time.Minute) {
		t.Fatal("held by a stalled LiveOnly session although the producer is prematch only")
	}
	waitRecoverHits(t, hits, 2)
	waitDetachedDone(t, a, 3*time.Second)
}

// A stalled session that can receive the producer still holds the deadline:
// the replay sits behind its stuck message.
func TestActor_SnapshotCompleteDeadline_HeldByAStalledSessionInTheProducersScope(t *testing.T) {
	srv, hits := fixtureSrv(t)
	defer srv.Close()
	a := newWiredActorForProducer(t, srv, newFakeManagerOps(), 2) // prematch
	oldID := startRecovery(t, a, hits, time.Now().Add(-10*time.Minute).Truncate(time.Millisecond), 1)
	ops, _, prematch := sessionScopeOps(a)
	started := a.currentRecovery.recoveryStartedAt

	ops.m.OnMessageProcessingStarted(prematch, 2, started)
	if reissuedAt(a, started, 6*time.Minute) {
		t.Fatal("re-issued while a session that receives the producer was stalled")
	}
	time.Sleep(50 * time.Millisecond)
	if hits.recover.Load() != 1 || a.currentRecovery.recoveryID != oldID {
		t.Fatalf("recovery POSTs = %d, want 1", hits.recover.Load())
	}
}

// A stalled session the registry does not know has no interest to compare:
// it counts, rather than let a re-issue pile onto a queue that may be it.
func TestManager_ClientProcessingStalled_UnregisteredSessionCounts(t *testing.T) {
	m, _ := openedManagerWithActors(t, 1, 2)
	now := time.Now()
	m.OnClientSessionOpened(uuid.New(), types.LiveOnlyMessageInterest)
	m.OnMessageProcessingStarted(uuid.New(), 1, now.Add(-time.Hour))
	if !m.clientProcessingStalled(2, now, time.Minute) {
		t.Fatal("a stalled session missing from the registry must count")
	}
}

func holdProcessingTS(ts time.Time) func() (time.Time, error) {
	return func() (time.Time, error) { return ts, nil }
}
