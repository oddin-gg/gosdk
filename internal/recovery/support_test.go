package recovery

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/oddin-gg/gosdk/internal/api"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/internal/producer"
	"github.com/oddin-gg/gosdk/types"
)

// newDiscardLogger builds a recovery logger that drops everything: keeps test
// output clean.
func newDiscardLogger() *log.Logger {
	return log.New(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// minimalCfg satisfies config.Config. The settings the recovery rules read can be
// set; a zero one is the library's default.
type minimalCfg struct {
	apiURL string
	token  string

	maxInactivity    time.Duration
	maxRecovery      time.Duration
	snapshotDeadline time.Duration
	nodeID           *int
}

func (c *minimalCfg) AccessToken() *string        { return &c.token }
func (c *minimalCfg) DefaultLocale() types.Locale { return types.EnLocale }
func (c *minimalCfg) MaxInactivity() time.Duration {
	if c.maxInactivity != 0 {
		return c.maxInactivity
	}
	return 20 * time.Second
}

func (c *minimalCfg) SnapshotCompleteDeadline() time.Duration {
	if c.snapshotDeadline != 0 {
		return c.snapshotDeadline
	}
	return 5 * time.Minute
}

func (c *minimalCfg) MaxRecoveryExecution() time.Duration {
	if c.maxRecovery != 0 {
		return c.maxRecovery
	}
	return 360 * time.Minute
}
func (c *minimalCfg) MessagingPort() int                      { return 5672 }
func (c *minimalCfg) SdkNodeID() *int                         { return c.nodeID }
func (c *minimalCfg) SelectedEnvironment() *types.Environment { return nil }
func (c *minimalCfg) SelectedRegion() types.Region            { return types.RegionDefault }
func (c *minimalCfg) ExchangeName() string                    { return "oddinfeed" }
func (c *minimalCfg) ReplayExchangeName() string              { return "oddinreplay" }
func (c *minimalCfg) ReportExtendedData() bool                { return false }
func (c *minimalCfg) APIURL() (string, error)                 { return c.apiURL, nil }
func (c *minimalCfg) MQURL() (string, error)                  { return "", nil }
func (c *minimalCfg) SportIDPrefix() string                   { return "od:sport:" }

type rewriteTransport struct {
	target string
	base   http.RoundTripper
}

func (rt *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t, _ := url.Parse(rt.target)
	req.URL.Scheme = t.Scheme
	req.URL.Host = t.Host
	base := rt.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// The fixture's producers: 1 is live, 2 prematch, 3 both.
const (
	liveID  = 1
	preID   = 2
	mixedID = 3
)

const producersBody = `<?xml version="1.0"?>
<producers response_code="OK">
  <producer id="1" name="live" description="Live" active="true" api_url="https://x" scope="live" stateful_recovery_window_in_minutes="60"/>
  <producer id="2" name="pre" description="Pre" active="true" api_url="https://x" scope="prematch" stateful_recovery_window_in_minutes="60"/>
  <producer id="3" name="mixed" description="Mixed" active="true" api_url="https://x" scope="live|prematch" stateful_recovery_window_in_minutes="60"/>
</producers>`

const okBody = `<?xml version="1.0"?><response response_code="OK"/>`

// recordedRequest is one recovery request the fixture server took.
type recordedRequest struct {
	producer  string
	requestID int
	// afterMillis is the request's `after=`, 0 for a full snapshot.
	afterMillis int64
	event       string
	stateful    bool
}

// recoveryHits counts and records what the fixture server was asked.
type recoveryHits struct {
	recover         atomic.Int32
	eventRecover    atomic.Int32
	statefulRecover atomic.Int32
	// lastAfterMillis is the `after=` of the most recent snapshot recovery
	// request (0 when it carried none).
	lastAfterMillis atomic.Int64
	// refuse makes the recovery endpoints answer with a rejection.
	refuse atomic.Bool
	// override, when set, answers a request itself: true says it did.
	override atomic.Pointer[func(http.ResponseWriter, *http.Request) bool]

	mu       sync.Mutex
	requests []recordedRequest
}

func (h *recoveryHits) record(r recordedRequest) {
	h.mu.Lock()
	h.requests = append(h.requests, r)
	h.mu.Unlock()
}

// all are the requests so far, snapshot recoveries and event recoveries, in order.
func (h *recoveryHits) all() []recordedRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]recordedRequest(nil), h.requests...)
}

// snapshots are the snapshot recovery requests so far.
func (h *recoveryHits) snapshots() []recordedRequest {
	var out []recordedRequest
	for _, r := range h.all() {
		if r.event == "" {
			out = append(out, r)
		}
	}
	return out
}

// fixtureSrv routes /descriptions/producers and the recovery and event-recovery
// endpoints. Other paths answer 200 so the api.Client considers them successful.
func fixtureSrv(t *testing.T) (*httptest.Server, *recoveryHits) {
	t.Helper()
	hits := &recoveryHits{}
	reply := func(w http.ResponseWriter) {
		if hits.refuse.Load() {
			_, _ = io.WriteString(w, `<?xml version="1.0"?><response response_code="FORBIDDEN"/>`)
			return
		}
		_, _ = io.WriteString(w, okBody)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if serve := hits.override.Load(); serve != nil && (*serve)(w, r) {
			return
		}
		id, _ := strconv.Atoi(r.URL.Query().Get("request_id"))
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		// the producer's name is the path segment before the endpoint's
		producerOf := func(endpoint string) string {
			for i, p := range parts {
				if p == endpoint && i > 0 {
					return parts[i-1]
				}
			}
			return ""
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/descriptions/producers"):
			_, _ = io.WriteString(w, producersBody)
		case strings.Contains(r.URL.Path, "/recovery/initiate_request"):
			ms := int64(0) // 0 = no after= at all: the full-history request
			if after := r.URL.Query().Get("after"); after != "" {
				if parsed, err := strconv.ParseInt(after, 10, 64); err == nil {
					ms = parsed
				}
			}
			hits.lastAfterMillis.Store(ms)
			hits.recover.Add(1)
			hits.record(recordedRequest{producer: producerOf("recovery"), requestID: id, afterMillis: ms})
			reply(w)
		case strings.Contains(r.URL.Path, "/odds/events/"):
			hits.eventRecover.Add(1)
			hits.record(recordedRequest{producer: producerOf("odds"), requestID: id, event: parts[len(parts)-2]})
			reply(w)
		case strings.Contains(r.URL.Path, "/stateful_messages/events/"):
			hits.statefulRecover.Add(1)
			hits.record(recordedRequest{producer: producerOf("stateful_messages"), requestID: id, event: parts[len(parts)-2], stateful: true})
			reply(w)
		default:
			_, _ = io.WriteString(w, okBody)
		}
	}))
	return srv, hits
}

// apiClientFor builds an api.Client talking to srv.
func apiClientFor(srv *httptest.Server, cfg *minimalCfg) *api.Client {
	apiClient := api.New(cfg)
	apiClient.SetHTTPClient(&http.Client{
		Transport: &rewriteTransport{
			target: srv.URL,
			base:   &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		},
		Timeout: 30 * time.Second,
	})
	return apiClient
}

// newTestHTTPClient routes the api.Client at the fixture server.
func newTestHTTPClient(srv *httptest.Server) *http.Client {
	return &http.Client{
		Transport: &rewriteTransport{
			target: srv.URL,
			base:   &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		},
		Timeout: 30 * time.Second,
	}
}

// eventually polls until cond holds, failing the test after a few seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// ---- the fake of what the actor uses of the manager

type completedHandle struct {
	id     int
	status types.RecoveryRequestStatus
	err    error
}

// fakeManagerOps captures what the actor hands the manager, and the sessions it
// is told are in processing.
type fakeManagerOps struct {
	mu          sync.Mutex
	registered  []*Handle
	completed   []completedHandle
	emittedMsgs []types.RecoveryMessage
	processing  map[uuid.UUID]time.Duration
	resetters   map[uuid.UUID]Resetter
	gcs         int
	nextID      atomic.Uint32
}

func newFakeManagerOps() *fakeManagerOps { return &fakeManagerOps{} }

func (f *fakeManagerOps) registerHandle(h *Handle) {
	f.mu.Lock()
	f.registered = append(f.registered, h)
	f.mu.Unlock()
}

func (f *fakeManagerOps) completeHandle(id int, status types.RecoveryRequestStatus, err error) *Handle {
	f.mu.Lock()
	f.completed = append(f.completed, completedHandle{id: id, status: status, err: err})
	var found *Handle
	for _, h := range f.registered {
		if h.RequestID() == id {
			found = h
		}
	}
	f.mu.Unlock()
	if found != nil {
		found.complete(status, err, time.Now())
	}
	return found
}

func (f *fakeManagerOps) nextRequestID() int { return int(f.nextID.Add(1)) }

func (f *fakeManagerOps) LookupHandle(id int) (*Handle, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, h := range f.registered {
		if h.RequestID() == id {
			return h, true
		}
	}
	return nil, false
}

func (f *fakeManagerOps) emitRecoveryMessage(msg types.RecoveryMessage) {
	f.mu.Lock()
	f.emittedMsgs = append(f.emittedMsgs, msg)
	f.mu.Unlock()
}

func (f *fakeManagerOps) resetterOf(session uuid.UUID) Resetter {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resetters[session]
}

func (f *fakeManagerOps) processingSince() map[uuid.UUID]time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[uuid.UUID]time.Duration, len(f.processing))
	for id, since := range f.processing {
		out[id] = since
	}
	return out
}

func (f *fakeManagerOps) gcHandles(time.Time) {
	f.mu.Lock()
	f.gcs++
	f.mu.Unlock()
}

func (f *fakeManagerOps) setProcessing(since map[uuid.UUID]time.Duration) {
	f.mu.Lock()
	f.processing = since
	f.mu.Unlock()
}

func (f *fakeManagerOps) emitted() []types.RecoveryMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]types.RecoveryMessage(nil), f.emittedMsgs...)
}

// ---- a recovery manager over the fixture, on a clock the test moves

// rig is a recovery manager, opened, over the fixture server: the clock is the
// test's, so what the manager stamps and judges moves only as the test says. The
// actor still looks at the clock on its own once a second of real time, which
// changes nothing while the clock stands still.
type rig struct {
	t     *testing.T
	srv   *httptest.Server
	hits  *recoveryHits
	pm    *producer.Manager
	cfg   *minimalCfg
	clock *fakeClock
	mgr   *Manager
	out   <-chan types.RecoveryMessage

	mu       sync.Mutex
	messages []types.RecoveryMessage
}

func newRig(t *testing.T, edit ...func(*minimalCfg)) *rig {
	t.Helper()
	srv, hits := fixtureSrv(t)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	cfg := &minimalCfg{apiURL: u.Host, token: "tok"}
	for _, e := range edit {
		e(cfg)
	}
	apiClient := apiClientFor(srv, cfg)
	pm := producer.NewManager(cfg, apiClient, newDiscardLogger())
	mgr := NewManager(cfg, pm, apiClient, newDiscardLogger(), 0)
	r := &rig{t: t, srv: srv, hits: hits, pm: pm, cfg: cfg, clock: newFakeClock(), mgr: mgr}
	// the clock starts at the real time, since the producer manager checks the
	// recovery start a client sets against it
	r.clock.wall = time.Now().Truncate(time.Millisecond)
	mgr.clk = r.clock
	return r
}

// open opens the manager, which starts the feed, and collects what it streams.
func (r *rig) open() {
	r.t.Helper()
	out, err := r.mgr.Open(r.t.Context())
	if err != nil {
		r.t.Fatalf("Open: %v", err)
	}
	r.out = out
	r.mgr.actor().setBeforeHandle(func(f fact) {
		if b, ok := f.(factBarrier); ok {
			close(b.done)
		}
	})
	go func() {
		for m := range out {
			r.mu.Lock()
			r.messages = append(r.messages, m)
			r.mu.Unlock()
		}
	}()
	r.t.Cleanup(r.mgr.Close)
}

// streamed are the messages the manager published so far.
func (r *rig) streamed() []types.RecoveryMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]types.RecoveryMessage(nil), r.messages...)
}

// statuses are the producer statuses streamed for the producer, in order.
func (r *rig) statuses(producerID int) []types.ProducerStatus {
	var out []types.ProducerStatus
	for _, m := range r.streamed() {
		if m.ProducerStatus != nil && m.ProducerStatus.Producer().ID() == producerID {
			out = append(out, m.ProducerStatus)
		}
	}
	return out
}

// openSession registers a session of the client and reports its queue bound, as
// a session opening does.
func (r *rig) openSession(id uuid.UUID, interest types.MessageInterest) {
	r.mgr.OnClientSessionOpened(id, interest, r.clock.Now())
	r.mgr.OnFeedChannelRestored(id)
}

// alive is a subscribed alive of the producer, generated now, on the alive channel.
func (r *rig) alive(producerID int) { r.aliveOf(producerID, true) }

func (r *rig) aliveOf(producerID int, subscribed bool) {
	now := r.clock.Now()
	r.mgr.OnAliveReceived(producerID, types.MessageTimestamp{Created: now.Wall(), Received: now.Wall()}, now, subscribed)
}

// processed is a live message of the producer, generated now, that the session finished.
func (r *rig) processed(session uuid.UUID, producerID int) time.Time {
	now := r.clock.Now()
	r.mgr.OnMessageProcessed(session, producerID, now.Wall(), now, 0)
	return now.Wall()
}

// snapshotComplete is the session taking the snapshot_complete of the request.
func (r *rig) snapshotComplete(session uuid.UUID, producerID, requestID int) {
	r.t.Helper()
	if err := r.mgr.OnSnapshotCompleteReceived(r.t.Context(), session, producerID, requestID); err != nil {
		r.t.Fatalf("OnSnapshotCompleteReceived: %v", err)
	}
}

// waitSnapshots waits for n snapshot recovery requests to have gone out.
func (r *rig) waitSnapshots(n int) []recordedRequest {
	r.t.Helper()
	eventually(r.t, strconv.Itoa(n)+" snapshot recovery requests", func() bool { return len(r.hits.snapshots()) >= n })
	return r.hits.snapshots()
}

// tick has the actor look at the clock, once.
func (r *rig) tick() {
	r.t.Helper()
	if a := r.mgr.actor(); a != nil {
		a.postEssential(factTick{})
	}
}

// sync waits until the actor has taken everything posted before it, the
// samples and the commands too.
func (r *rig) sync() {
	r.t.Helper()
	a := r.mgr.actor()
	if a == nil {
		r.t.Fatal("the manager is not open")
	}
	barrier := func() {
		done := make(chan struct{})
		a.postEssential(factBarrier{done})
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			r.t.Fatal("the actor did not reach the barrier")
		}
	}
	barrier()
	eventually(r.t, "the actor's lesser queues to empty", func() bool { return len(a.samples) == 0 && len(a.commands) == 0 })
	barrier()
}

func (r *rig) status(producerID int) (types.ProducerStatus, bool) {
	return r.mgr.ProducerStatus(producerID)
}

func (r *rig) isDown(producerID int) bool {
	down, err := r.pm.IsProducerDown(context.Background(), producerID)
	if err != nil {
		r.t.Fatalf("IsProducerDown: %v", err)
	}
	return down
}

func (r *rig) timestampForRecovery(producerID int) time.Time {
	p, err := r.pm.GetProducer(context.Background(), producerID)
	if err != nil {
		r.t.Fatalf("GetProducer: %v", err)
	}
	return p.TimestampForRecovery()
}

// factBarrier is a fact that says when the actor has taken everything posted
// before it. Tests only.
type factBarrier struct{ done chan struct{} }

func (factBarrier) isFact() {}
