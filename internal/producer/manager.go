package producer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/config"
	log "github.com/oddin-gg/gosdk/internal/log"
	"github.com/oddin-gg/gosdk/types"
)

// Manager owns the catalog of known producers and exposes both
// catalog-style queries (Producers / ActiveProducers / etc.) and
// per-producer mutable-state setters (SetProducerDown,
// SetProducerLastMessageTimestamp, …).
//
// Concurrency: the producer-map itself is guarded by m.mu; per-producer
// mutable fields live on data.mu (see data struct). Setters take the
// data write lock; the producerImpl accessor methods returned by
// AvailableProducers / GetProducer / etc. take the data read lock.
type Manager struct {
	apiClient   *api.Client
	cfg         config.Config
	logger      *log.Logger
	mu          sync.RWMutex
	producerMap map[int]*data
}

func (m *Manager) producers(ctx context.Context) (map[int]*data, error) {
	m.mu.RLock()
	if m.producerMap != nil {
		defer m.mu.RUnlock()
		return m.producerMap, nil
	}
	m.mu.RUnlock()

	if err := m.Open(ctx); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.producerMap, nil
}

func (m *Manager) producer(ctx context.Context, id int) (*data, error) {
	producers, err := m.producers(ctx)
	if err != nil {
		return nil, err
	}
	producer, ok := producers[id]
	if !ok {
		return nil, fmt.Errorf("missing producer %d", id)
	}
	return producer, nil
}

// ErrNotOpened is returned by cached lookups before Open populated the
// catalog. Exported as a sentinel so hot-path callers can distinguish
// "manager has no data yet" from "unknown producer id".
var ErrNotOpened = errors.New("producer manager: not opened (call Open first)")

// producerCached returns a producer by id WITHOUT triggering a lazy Open.
// Callers must have ensured Open was called first (recovery hot path
// guarantees this). Returns ErrNotOpened if Open has not been called yet —
// preferable to a hidden context.Background() lookup that fires HTTP
// from inside the AMQP message-processing path.
func (m *Manager) producerCached(id int) (*data, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.producerMap == nil {
		return nil, ErrNotOpened
	}
	p, ok := m.producerMap[id]
	if !ok {
		return nil, fmt.Errorf("missing producer %d", id)
	}
	return p, nil
}

// GetProducerCached is the no-ctx variant of GetProducer for hot-path
// callers (FeedMessageFactory). Uses only in-memory state — fails if
// Open has not been called. Mirrors the public GetProducer return type.
func (m *Manager) GetProducerCached(id int) (types.Producer, error) {
	d, err := m.producerCached(id)
	switch {
	case errors.Is(err, ErrNotOpened):
		// Not-opened must propagate: fabricating the enabled/active
		// placeholder here made "manager has no data yet" look like a
		// live producer, silently mis-routing messages processed
		// before Open completed.
		return nil, err
	case err != nil:
		// Fall back to the unknown-producer placeholder. This
		// DIVERGES from the public GetProducer (which returns
		// ErrProducerNotFound for unknown ids) on purpose: this is the
		// hot decode path — a message referencing an uncatalogued
		// producer still needs a non-nil producer for diagnostics and
		// unparsable envelopes, not a per-message error.
		return buildProducerImplFromUnknown(id, m.cfg)
	}
	return buildProducerImpl(d)
}

// UnknownProducerPlaceholder builds the unknown-producer placeholder
// without consulting the catalog. For diagnostics paths (unparsable
// messages) that must carry a non-nil producer even when the manager
// was never opened — normal message routing must NOT use this (see
// GetProducerCached's ErrNotOpened contract).
func (m *Manager) UnknownProducerPlaceholder(id int) (types.Producer, error) {
	return buildProducerImplFromUnknown(id, m.cfg)
}

// Open fetches the producer list from the API and populates the in-memory map.
// Safe to call multiple times; subsequent calls refresh the catalog (immutable
// fields like name/description/scope/active) but PRESERVE caller-owned mutable
// state (enabled, recoveryFromTimestamp) and runtime state (flaggedDown,
// lastMessageTimestamp, lastRecoveryInfo, …) from the prior generation.
//
// Without this preservation, a caller who toggled SetProducerEnabled or
// SetProducerRecoveryFromTimestamp BEFORE Connect would silently lose those
// overrides — Connect calls Open as part of ensureNormal, and a fresh
// newData entry would re-derive `enabled` from the API's `active` flag.
//
// Producers that disappear from the API on a subsequent Open are dropped
// from the map (the catalog said they no longer exist).
func (m *Manager) Open(ctx context.Context) error {
	apiProducers, err := m.apiClient.FetchProducers(ctx)
	if err != nil {
		return err
	}

	m.logger.Debugf("fetched producer list - size %d", len(apiProducers))

	// Hold the manager write-lock across the entire carry-over AND
	// install. Pre-v2.25 the lock was released between snapshotting
	// `existing` and assigning `m.producerMap = pm`; a concurrent
	// setter that already lookup'd an old entry could mutate it in
	// the gap, and Open would then drop the mutation by installing
	// a freshly-built map. Setters now hold m.mu.RLock through their
	// data.mu mutation (via mutateProducerByID), so this Lock blocks
	// until any in-flight setter completes — at which point its
	// mutation is visible on the OLD entry and is preserved by the
	// carry-over below.
	//
	// The HTTP fetch (above) ran OUTSIDE the lock; only the
	// in-memory carry-over (microseconds, no I/O) runs under it.
	m.mu.Lock()
	defer m.mu.Unlock()

	pm := make(map[int]*data, len(apiProducers))
	for i := range apiProducers {
		p := apiProducers[i]
		if old, ok := m.producerMap[p.ID]; ok {
			// Reuse the SAME *data object: refresh its catalog fields in
			// place and keep all runtime / caller-owned state. This both
			// preserves caller overrides (enabled, recoveryFrom) AND keeps
			// any producer handle already returned to a caller live —
			// those handles read mutable state through this pointer, so a
			// later SetProducer* / alive update is observed even though the
			// catalog was refreshed. (Previously Open allocated a fresh
			// *data and copied mutable state forward, orphaning existing
			// handles at the old object.)
			old.refreshCatalog(p)
			pm[p.ID] = old
		} else {
			pm[p.ID] = newData(p)
		}
	}
	m.producerMap = pm

	m.logger.Debugf("mapped producer list - %v", apiProducers)
	return nil
}

// mutateProducerByID atomically looks up the producer under
// m.mu.RLock and runs fn with data.mu held for write — releasing
// both in the correct order on return. Holding m.mu.RLock through
// the data.mu mutation pairs with Open's m.mu.Lock so a setter
// cannot mutate an *data that Open is about to orphan.
func (m *Manager) mutateProducerByID(id int, fn func(*data) error) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.producerMap == nil {
		return fmt.Errorf("producer manager: not opened (call Open first)")
	}
	p, ok := m.producerMap[id]
	if !ok {
		// Wrap the sentinel: every by-id operation — getters AND the
		// public setters routed through here (SetProducerState,
		// SetProducerRecoveryFromTimestamp) — must satisfy
		// errors.Is(err, ErrProducerNotFound), matching GetProducer.
		return fmt.Errorf("missing producer %d: %w", id, ErrProducerNotFound)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return fn(p)
}

// mutateProducerByIDCtx is the ctx-aware variant: triggers a lazy
// Open via m.producers(ctx) (which acquires m.mu.Lock briefly via
// Open) before re-acquiring RLock for the mutation. Open MUST run
// before we acquire RLock — running it underneath would deadlock
// on m.mu.
func (m *Manager) mutateProducerByIDCtx(ctx context.Context, id int, fn func(*data) error) error {
	// Honor an already-cancelled ctx BEFORE mutating: the warm-cache
	// path does no I/O, so pre-fix a setter invoked with a dead ctx
	// still mutated producer state and returned nil — "error ⇒ no side
	// effect" ran backwards ("cancelled ⇒ side effect anyway").
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := m.producers(ctx); err != nil {
		return err
	}
	// Re-check after the (possibly lazy-loading) producers call: a ctx
	// that expired during the load must not proceed to mutate.
	if err := ctx.Err(); err != nil {
		return err
	}
	return m.mutateProducerByID(id, fn)
}

// SetProducerDown ...
func (m *Manager) SetProducerDown(id int, flaggedDown bool) error {
	return m.mutateProducerByID(id, func(p *data) error {
		p.flaggedDown = flaggedDown
		return nil
	})
}

// The three "latest observed" cursors below are MONOTONIC: multiple
// subscriptions share one producer manager and their processing
// interleaves, so a slow subscription finishing an OLDER message can
// call a setter after a faster one already stored a NEWER timestamp.
// An unconditional overwrite regressed the freshness signal — the
// recovery tick then read the producer as delayed and could initiate a
// false producer-down/recovery transition. Older-than-stored values are
// silently ignored (they carry no new information; the mutex serializes
// the compare-and-set). SetProducerRecoveryFromTimestamp is deliberately
// NOT monotonic — rewinding the recovery point is its purpose.

// SetProducerLastMessageTimestamp advances the latest-message liveness
// cursor; older timestamps are ignored (see monotonicity note above).
func (m *Manager) SetProducerLastMessageTimestamp(id int, timestamp time.Time) error {
	if timestamp.IsZero() {
		return errors.New("required non zero timestamp")
	}
	return m.mutateProducerByID(id, func(p *data) error {
		if timestamp.After(p.lastMessageTimestamp) {
			p.lastMessageTimestamp = timestamp
		}
		return nil
	})
}

// grosslyFutureGenCursor reports whether a gen-derived cursor timestamp is
// implausibly far in the future (more than recoveryFromMaxSkew ahead of
// now). These cursors advance MONOTONICALLY, so a corrupt feed timestamp
// (e.g. a wrong-epoch / year-3000 message) stored once could never be
// replaced by a later legitimate (smaller) value — leaving the producer
// permanently "delayed" (calculateTiming reads now-cursor via Abs) with no
// self-healing. Bounding future skew here mirrors the allowance the public
// recovery-cursor setter already enforces on caller input. Small skew (<
// recoveryFromMaxSkew) stays accepted so normal feed/host clock drift does
// not stall freshness tracking.
func (m *Manager) grosslyFutureGenCursor(id int, cursor string, timestamp time.Time) bool {
	if time.Until(timestamp) <= recoveryFromMaxSkew {
		return false
	}
	if m.logger != nil {
		m.logger.WithField("producer_id", id).
			WithField("cursor", cursor).
			WithField("timestamp", timestamp).
			Warn("producer: ignoring implausibly-future gen timestamp (corrupt feed data)")
	}
	return true
}

// SetLastProcessedMessageGenTimestamp advances the processed-message
// freshness cursor; older timestamps are ignored (see monotonicity note
// above), and implausibly-future ones are rejected (see
// grosslyFutureGenCursor).
func (m *Manager) SetLastProcessedMessageGenTimestamp(id int, timestamp time.Time) error {
	if m.grosslyFutureGenCursor(id, "last_processed_gen", timestamp) {
		return nil
	}
	return m.mutateProducerByID(id, func(p *data) error {
		if timestamp.After(p.lastProcessedMessageGenTimestamp) {
			p.lastProcessedMessageGenTimestamp = timestamp
		}
		return nil
	})
}

// SetLastAliveReceivedGenTimestamp advances the alive-gen freshness
// cursor; older timestamps are ignored (see monotonicity note above), and
// implausibly-future ones are rejected (see grosslyFutureGenCursor).
func (m *Manager) SetLastAliveReceivedGenTimestamp(id int, timestamp time.Time) error {
	if m.grosslyFutureGenCursor(id, "last_alive_gen", timestamp) {
		return nil
	}
	return m.mutateProducerByID(id, func(p *data) error {
		if timestamp.After(p.lastAliveReceivedGenTimestamp) {
			p.lastAliveReceivedGenTimestamp = timestamp
		}
		return nil
	})
}

// SetProducerRecoveryInfo records the summary of a snapshot recovery. It
// also CONSUMES the one-shot explicit recovery-cursor override (see
// recoveryFromExplicit): a recovery has now been recorded, so subsequent
// recoveries must resume from the freshest alive cursor rather than
// re-rewinding to the same explicit point forever. Consuming on
// successful recording (not on recovery start) keeps the override alive
// across a failed PostRecovery so the rewind still takes effect on retry.
func (m *Manager) SetProducerRecoveryInfo(id int, recoveryInfo types.RecoveryInfo) error {
	return m.mutateProducerByID(id, func(p *data) error {
		p.lastRecoveryInfo = recoveryInfo
		p.recoveryFromExplicit = false
		return nil
	})
}

// AvailableProducers ...
func (m *Manager) AvailableProducers(ctx context.Context) (map[int]types.Producer, error) {
	producers, err := m.producers(ctx)
	if err != nil {
		return nil, err
	}
	res := make(map[int]types.Producer, len(producers))
	for i := range producers {
		d := producers[i]
		res[i], err = buildProducerImpl(d)
		if err != nil {
			return nil, err
		}
	}
	return res, nil
}

// ActiveProducers ...
func (m *Manager) ActiveProducers(ctx context.Context) (map[int]types.Producer, error) {
	producers, err := m.producers(ctx)
	if err != nil {
		return nil, err
	}
	res := make(map[int]types.Producer, len(producers))
	for i := range producers {
		// Snapshot under one lock, THEN decide: `active` is read from the
		// locked snapshot (no race with refreshCatalog, which producers()
		// no longer serializes since it released m.mu), and inactive
		// producers are skipped BEFORE parsing scopes — so an inactive
		// producer with an empty/unknown scope doesn't fail the query.
		s := snapshotProducer(producers[i])
		if !s.active {
			continue
		}
		p, err := s.build()
		if err != nil {
			return nil, err
		}
		res[i] = p
	}
	return res, nil
}

// ActiveProducersInScope ...
func (m *Manager) ActiveProducersInScope(ctx context.Context, scope types.ProducerScope) (map[int]types.Producer, error) {
	producers, err := m.producers(ctx)
	if err != nil {
		return nil, err
	}
	res := make(map[int]types.Producer, len(producers))
	for i := range producers {
		// Same snapshot-then-filter as ActiveProducers: skip inactive
		// producers before parsing their scope.
		s := snapshotProducer(producers[i])
		if !s.active {
			continue
		}
		p, err := s.build()
		if err != nil {
			return nil, err
		}
		if types.ProducerHasScope(p, scope) {
			res[i] = p
		}
	}
	return res, nil
}

// GetProducer ...
func (m *Manager) GetProducer(ctx context.Context, id int) (types.Producer, error) {
	producers, err := m.producers(ctx)
	if err != nil {
		return nil, err
	}
	p, ok := producers[id]
	if !ok {
		// Classifiable not-found, NOT the synthetic placeholder: the
		// placeholder is active/enabled/both-scopes, so a public
		// Producer(ctx, typoID) fabricated a plausible catalog member a
		// caller could initiate recovery against. The placeholder stays
		// reserved for feed diagnostics (UnknownProducerPlaceholder).
		return nil, fmt.Errorf("producer %d: %w", id, ErrProducerNotFound)
	}
	return buildProducerImpl(p)
}

// ErrProducerNotFound is returned by GetProducer for ids absent from
// the producer catalog. Surfaced publicly as gosdk.ErrProducerNotFound.
var ErrProducerNotFound = errors.New("producer not found in catalog")

// SetProducerState ...
func (m *Manager) SetProducerState(ctx context.Context, id int, enabled bool) error {
	return m.mutateProducerByIDCtx(ctx, id, func(p *data) error {
		p.enabled = enabled
		return nil
	})
}

// recoveryFromMaxSkew is the clock-skew allowance for recovery cursors:
// a cursor this far into the future is accepted (NTP drift between the
// consumer's host and the SDK host), anything beyond is rejected.
const recoveryFromMaxSkew = time.Minute

// SetProducerRecoveryFromTimestamp ...
func (m *Manager) SetProducerRecoveryFromTimestamp(ctx context.Context, id int, timestamp time.Time) error {
	return m.mutateProducerByIDCtx(ctx, id, func(p *data) error {
		// statefulRecoveryWindowInMinutes is immutable after
		// construction; safe to read under the data write lock.
		maxRequestMinutes := p.statefulRecoveryWindowInMinutes
		switch {
		case timestamp.IsZero():
			break
		case time.Since(timestamp).Minutes() > float64(maxRequestMinutes):
			return errors.New("last received message timestamp can not be so long in past")
		case time.Until(timestamp) > recoveryFromMaxSkew:
			// A FUTURE cursor passed the too-old check (time.Since is
			// negative) and was serialized as the recovery `after`
			// value — silently omitting every currently-missing
			// message. A units/clock mistake (ms vs s epoch, wrong
			// zone) must fail loudly instead; small skew is tolerated.
			return errors.New("recovery timestamp is in the future (check clock/units)")
		}
		p.recoveryFromTimestamp = timestamp
		// Mark it an explicit override so TimestampForRecovery honors it
		// over any alive cursor for the next recovery (rewind intent). A
		// zero timestamp clears the override (revert to alive precedence).
		p.recoveryFromExplicit = !timestamp.IsZero()
		return nil
	})
}

// IsProducerEnabled ...
func (m *Manager) IsProducerEnabled(ctx context.Context, id int) (bool, error) {
	producer, err := m.producer(ctx, id)
	if err != nil {
		return false, err
	}
	producer.mu.RLock()
	defer producer.mu.RUnlock()
	return producer.enabled, nil
}

// IsProducerDown ...
func (m *Manager) IsProducerDown(ctx context.Context, id int) (bool, error) {
	producer, err := m.producer(ctx, id)
	if err != nil {
		return false, err
	}
	producer.mu.RLock()
	defer producer.mu.RUnlock()
	return producer.flaggedDown, nil
}

// NewManager ...
func NewManager(cfg config.Config, apiClient *api.Client, logger *log.Logger) *Manager {
	return &Manager{
		apiClient: apiClient,
		cfg:       cfg,
		logger:    logger,
	}
}
