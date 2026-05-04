package recovery

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/producer"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

const (
	initialDelay = 60 * time.Second
	tickPeriod   = 10 * time.Second
)

// HandleGCGracePeriod is the default time a completed *Handle stays
// queryable via LookupHandle before being garbage-collected. NEXT.md
// §0 left this as a guess; 5 minutes is generous enough for late
// pollers without growing the map indefinitely.
const HandleGCGracePeriod = 5 * time.Minute

// Manager is the dispatcher that owns the per-producer actor goroutines
// and the cross-actor singletons (output channel, request id generator,
// handles registry).
//
// Phase 5 v2 actor model (NEXT.md §11): per-producer state lives inside
// recoveryActor goroutines. Manager methods are thin — they look up
// the actor and push events to its inbox. The previous mutex-guarded
// producerRecoveryData + central manager-locks are gone.
type Manager struct {
	cfg             types.OddsFeedConfiguration
	producerManager *producer.Manager
	apiClient       *api.Client
	logger          *log.Logger
	sequence        *generator

	// actors holds one recoveryActor per known producer. Populated at
	// Open from ActiveProducers and lazily on first message for
	// previously-unknown producers.
	actorsMu sync.RWMutex
	actors   map[uint]*recoveryActor

	// out is the public RecoveryEvents stream (closed on Close).
	out chan types.RecoveryMessage

	// handles tracks outstanding *Handle objects keyed by request id.
	// Inserted by registerHandle, transitioned to terminal by
	// completeHandle, GC'd by gcCompletedHandles.
	handlesMu sync.RWMutex
	handles   map[uint]*Handle

	// messageProcessingTimes maps sessionID → processing start time so
	// OnMessageProcessingEnded can warn on >1s processing.
	processingMu      sync.Mutex
	processingTimes   map[uuid.UUID]time.Time

	// Manager-lifetime ctx + cancel (set in Open).
	ctx       context.Context
	cancelCtx context.CancelFunc

	// closeOnce guards a single shutdown.
	closeOnce sync.Once
	closeCh   chan struct{}
	ticker    *time.Ticker
}

// NewManager constructs the recovery manager. Open(ctx) must be called
// before any actors are spawned.
func NewManager(cfg types.OddsFeedConfiguration, producerManager *producer.Manager, apiClient *api.Client, logger *log.Logger) *Manager {
	return &Manager{
		cfg:             cfg,
		producerManager: producerManager,
		apiClient:       apiClient,
		logger:          logger,
		sequence:        newGenerator(1),
		actors:          make(map[uint]*recoveryActor),
		handles:         make(map[uint]*Handle),
		processingTimes: make(map[uuid.UUID]time.Time),
	}
}

// Open spawns one actor per active producer and starts the periodic
// inactivity tick. Returns the recovery-events channel.
func (m *Manager) Open(ctx context.Context) (<-chan types.RecoveryMessage, error) {
	if m.out != nil {
		return nil, errors.New("already opened")
	}
	mgrCtx, cancel := context.WithCancel(ctx)
	m.ctx = mgrCtx
	m.cancelCtx = cancel

	activeProducers, err := m.producerManager.ActiveProducers(mgrCtx)
	if err != nil {
		cancel()
		return nil, err
	}
	if len(activeProducers) == 0 {
		m.logger.Warn("no active producers")
	}

	m.out = make(chan types.RecoveryMessage, 1024)
	m.closeCh = make(chan struct{})

	m.actorsMu.Lock()
	for id := range activeProducers {
		a := newRecoveryActor(mgrCtx, id, m.cfg, m.apiClient, m.producerManager, m, m.logger, 256)
		m.actors[id] = a
		go a.run()
	}
	m.actorsMu.Unlock()

	go m.runTickLoop()
	return m.out, nil
}

// Close terminates all actors and the tick loop. Idempotent. Pending
// handles are failed so callers blocked on Done() unblock.
func (m *Manager) Close() {
	m.closeOnce.Do(func() {
		if m.cancelCtx != nil {
			m.cancelCtx()
		}
		if m.ticker != nil {
			m.ticker.Stop()
		}
		if m.closeCh != nil {
			close(m.closeCh)
		}

		// Stop every actor first so they don't push to a closed out chan.
		m.actorsMu.Lock()
		actors := make([]*recoveryActor, 0, len(m.actors))
		for _, a := range m.actors {
			actors = append(actors, a)
		}
		m.actorsMu.Unlock()
		for _, a := range actors {
			a.stop()
		}

		// Unblock any pending handle waiters.
		m.failPendingHandles(errors.New("recovery: manager closed"))

		if m.out != nil {
			close(m.out)
		}
	})
}

// runTickLoop drives the periodic inactivity check by fanning out
// evTick events to every actor. Lossy: if an actor's inbox is full
// (which would only happen if it's blocked on the API), the tick is
// dropped — the next tick arrives in tickPeriod.
func (m *Manager) runTickLoop() {
	select {
	case <-time.After(initialDelay):
	case <-m.closeCh:
		return
	}
	m.ticker = time.NewTicker(tickPeriod)
	for {
		select {
		case <-m.ticker.C:
			now := time.Now()
			m.actorsMu.RLock()
			actors := make([]*recoveryActor, 0, len(m.actors))
			for _, a := range m.actors {
				actors = append(actors, a)
			}
			m.actorsMu.RUnlock()
			for _, a := range actors {
				_ = a.send(evTick{now: now})
			}
			m.gcCompletedHandles(now)
		case <-m.closeCh:
			return
		}
	}
}

// findOrSpawn returns the actor for producerID, lazily spawning one
// if a message arrives for a producer not in the initial set. This
// preserves the legacy findOrMakeProducerRecoveryData semantics.
func (m *Manager) findOrSpawn(producerID uint) *recoveryActor {
	m.actorsMu.RLock()
	a, ok := m.actors[producerID]
	m.actorsMu.RUnlock()
	if ok {
		return a
	}
	m.actorsMu.Lock()
	defer m.actorsMu.Unlock()
	if a, ok = m.actors[producerID]; ok {
		return a
	}
	a = newRecoveryActor(m.ctx, producerID, m.cfg, m.apiClient, m.producerManager, m, m.logger, 256)
	m.actors[producerID] = a
	go a.run()
	return a
}

// --- Inbound feed events (called from session.go on the AMQP path) ---

// OnMessageProcessingStarted records the per-session start time and
// dispatches to the actor.
func (m *Manager) OnMessageProcessingStarted(sessionID uuid.UUID, producerID uint, timestamp time.Time) {
	m.processingMu.Lock()
	m.processingTimes[sessionID] = timestamp
	m.processingMu.Unlock()

	a := m.findOrSpawn(producerID)
	_ = a.send(evMsgProcessingStarted{timestamp: timestamp})
}

// OnMessageProcessingEnded warns on >1s processing and dispatches the
// gen timestamp to the actor.
func (m *Manager) OnMessageProcessingEnded(sessionID uuid.UUID, producerID uint, timestamp time.Time) {
	if !timestamp.IsZero() {
		a := m.findOrSpawn(producerID)
		_ = a.send(evMsgProcessingEnded{timestamp: timestamp})
	}

	m.processingMu.Lock()
	start, ok := m.processingTimes[sessionID]
	delete(m.processingTimes, sessionID)
	m.processingMu.Unlock()

	switch {
	case !ok || start.IsZero():
		m.logger.Warn("message processing ended, but was not started")
	case time.Since(start).Milliseconds() > 1000:
		m.logger.Warnf("processing message took more than 1s - %d ms", time.Since(start).Milliseconds())
	}
}

// OnAliveReceived dispatches to the producer's actor.
func (m *Manager) OnAliveReceived(producerID uint, timestamp types.MessageTimestamp, isSubscribed bool, messageInterest types.MessageInterest) {
	a := m.findOrSpawn(producerID)
	_ = a.send(evAlive{
		timestamp:       timestamp,
		isSubscribed:    isSubscribed,
		messageInterest: messageInterest,
	})
}

// OnSnapshotCompleteReceived dispatches to the producer's actor.
func (m *Manager) OnSnapshotCompleteReceived(producerID uint, requestID uint, messageInterest types.MessageInterest) {
	m.actorsMu.RLock()
	a, ok := m.actors[producerID]
	m.actorsMu.RUnlock()
	if !ok {
		return // unknown producer; nothing to validate
	}
	_ = a.send(evSnapshotComplete{requestID: requestID, messageInterest: messageInterest})
}

// --- Synchronous commands ---

// InitiateEventOddsMessagesRecovery is the legacy uint-returning shape
// kept for types.RecoveryManager interface compatibility.
func (m *Manager) InitiateEventOddsMessagesRecovery(ctx context.Context, producerID uint, eventID types.URN) (uint, error) {
	h, err := m.InitiateEventOddsRecoveryHandle(ctx, producerID, eventID)
	if err != nil {
		return 0, err
	}
	return h.RequestID(), nil
}

// InitiateEventStatefulMessagesRecovery is the legacy uint-returning shape.
func (m *Manager) InitiateEventStatefulMessagesRecovery(ctx context.Context, producerID uint, eventID types.URN) (uint, error) {
	h, err := m.InitiateEventStatefulRecoveryHandle(ctx, producerID, eventID)
	if err != nil {
		return 0, err
	}
	return h.RequestID(), nil
}

// InitiateEventOddsRecoveryHandle is the handle-returning variant.
// Sends a recoverEvent command to the actor and waits for the reply.
func (m *Manager) InitiateEventOddsRecoveryHandle(ctx context.Context, producerID uint, eventID types.URN) (*Handle, error) {
	return m.dispatchRecoverEvent(ctx, producerID, eventID, false)
}

// InitiateEventStatefulRecoveryHandle is the handle-returning variant.
func (m *Manager) InitiateEventStatefulRecoveryHandle(ctx context.Context, producerID uint, eventID types.URN) (*Handle, error) {
	return m.dispatchRecoverEvent(ctx, producerID, eventID, true)
}

func (m *Manager) dispatchRecoverEvent(ctx context.Context, producerID uint, eventID types.URN, stateful bool) (*Handle, error) {
	a := m.findOrSpawn(producerID)
	reply := make(chan recoverEventReply, 1)
	a.sendBlocking(evRecoverEvent{
		ctx:              ctx,
		eventID:          eventID,
		statefulRecovery: stateful,
		reply:            reply,
	})
	select {
	case r := <-reply:
		return r.handle, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// --- Handle registry (called by actors via actorManagerOps) ---

// LookupHandle returns the tracked handle for a request id. The second
// return value is false when the id is unknown (never registered) or
// has been GC'd after the grace period.
func (m *Manager) LookupHandle(requestID uint) (*Handle, bool) {
	m.handlesMu.RLock()
	defer m.handlesMu.RUnlock()
	h, ok := m.handles[requestID]
	return h, ok
}

func (m *Manager) registerHandle(h *Handle) {
	m.handlesMu.Lock()
	if m.handles == nil {
		m.handles = make(map[uint]*Handle)
	}
	m.handles[h.requestID] = h
	m.handlesMu.Unlock()
}

func (m *Manager) completeHandle(requestID uint, status types.RecoveryRequestStatus, err error) *Handle {
	m.handlesMu.RLock()
	h := m.handles[requestID]
	m.handlesMu.RUnlock()
	if h == nil {
		return nil
	}
	h.complete(status, err, time.Now())
	return h
}

func (m *Manager) gcCompletedHandles(now time.Time) {
	m.handlesMu.Lock()
	defer m.handlesMu.Unlock()
	for id, h := range m.handles {
		if !h.IsTerminal() {
			continue
		}
		if now.Sub(h.endedAt) > HandleGCGracePeriod {
			delete(m.handles, id)
		}
	}
}

func (m *Manager) failPendingHandles(err error) {
	m.handlesMu.RLock()
	pending := make([]*Handle, 0, len(m.handles))
	for _, h := range m.handles {
		if !h.IsTerminal() {
			pending = append(pending, h)
		}
	}
	m.handlesMu.RUnlock()
	for _, h := range pending {
		h.complete(types.RecoveryStatusFailed, err, time.Now())
	}
}

// --- actorManagerOps ---

// nextRequestID is the actorManagerOps method backed by the shared
// generator. Generator is internally locked.
func (m *Manager) nextRequestID() uint { return m.sequence.next() }

// emitRecoveryMessage is the actorManagerOps method to publish a
// RecoveryMessage on the public stream. Buffered channel; if full,
// blocks the caller (an actor) — back-pressure on slow consumers,
// but acceptable since the buffer is 1024 and consumers should drain
// promptly.
func (m *Manager) emitRecoveryMessage(msg types.RecoveryMessage) {
	if m.out == nil {
		return
	}
	// Use a non-blocking-with-fallback: if the channel is full the
	// fallback blocks, ensuring the message isn't silently dropped.
	select {
	case m.out <- msg:
	default:
		m.out <- msg
	}
}

// eventRecoveryMessageImpl satisfies types.EventRecoveryMessage —
// the per-event recovery completion event delivered on the recovery
// stream.
type eventRecoveryMessageImpl struct {
	eventID   types.URN
	requestID uint
	producer  types.Producer
	timestamp types.MessageTimestamp
}

func (e eventRecoveryMessageImpl) Producer() types.Producer       { return e.producer }
func (e eventRecoveryMessageImpl) Timestamp() types.MessageTimestamp { return e.timestamp }
func (e eventRecoveryMessageImpl) EventID() types.URN              { return e.eventID }
func (e eventRecoveryMessageImpl) RequestID() uint                     { return e.requestID }
