package whoami

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/config"
	"github.com/oddin-gg/gosdk/types"
)

type bookmakerDetailImpl struct {
	expireAt    time.Time
	bookmakerID int
	virtualHost string
}

func (b bookmakerDetailImpl) ExpireAt() time.Time {
	return b.expireAt
}

func (b bookmakerDetailImpl) BookmakerID() int {
	return b.bookmakerID
}

func (b bookmakerDetailImpl) VirtualHost() string {
	return b.virtualHost
}

// Manager ...
//
// Concurrency: read-side fast-path uses RWMutex.RLock to check the
// cached value. Slow-path (first fetch + any later miss) goes through
// a singleflight.Group keyed on a single fixed key — only one
// FetchWhoAmI HTTP call runs at a time, but each caller's ctx is
// honored independently via singleflight.DoChan + select. The shared
// fetch runs under context.WithoutCancel(firstCallerCtx) so a quick
// first-caller deadline can't cancel the HTTP request out from under
// later waiters; per-call HTTP timeouts come from the API client.
// Pre-fix the slow path held a plain Mutex across the HTTP call,
// blocking every concurrent caller without checking their ctx — so
// caller B's 5s timeout was silently extended to whatever caller A's
// 30s call took.
type Manager struct {
	mu        sync.RWMutex
	cached    types.BookmakerDetail
	cfg       config.Config
	apiClient *api.Client
	logger    *slog.Logger
	sf        singleflight.Group
	// lifetime, when non-nil, is the detach root for the shared fetch:
	// the owning client cancels it on teardown/Close so a who-am-i
	// probe abandoned by its caller cannot outlive the component that
	// started it. Nil falls back to WithoutCancel(callerCtx).
	lifetime context.Context
}

// NewManager constructs a *Manager. The concrete return type
// satisfies gosdk's whoAmIManager interface structurally —
// internal/whoami can't import gosdk (cycle) so the interface lives
// at the consumer end (the gosdk root package).
func NewManager(cfg config.Config, client *api.Client) *Manager {
	return &Manager{
		cfg:       cfg,
		apiClient: client,
		logger:    slog.Default(),
		// lifetime left nil: loads fully detach from callers (bounded
		// by the api.Client's own HTTP timeout).
	}
}

// NewManagerWithLogger constructs a *Manager with a caller-supplied
// lifetime ctx (nil = loads fully detach from callers) and slog.Logger
// (nil = slog.Default()). See NewManager for the "concrete return
// type" rationale.
func NewManagerWithLogger(lifetime context.Context, cfg config.Config, client *api.Client, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		cfg:       cfg,
		apiClient: client,
		logger:    logger,
		lifetime:  lifetime,
	}
}

// BookmakerDetails returns cached bookmaker details, fetching once
// on first call.
//
// Each caller's ctx independently bounds their wait. Concurrent
// callers share a single in-flight FetchWhoAmI via singleflight, so
// the upstream API sees at most one round-trip per cache miss. The
// shared fetch runs under context.WithoutCancel(ctx) — a quick first
// caller deadline cannot cancel the HTTP request mid-flight for
// later waiters; the api.Client's HTTP timeout still bounds the
// fetch. Each waiter independently selects on its own ctx.Done().
func (m *Manager) BookmakerDetails(ctx context.Context) (types.BookmakerDetail, error) {
	// Fast path: cached.
	m.mu.RLock()
	cached := m.cached
	m.mu.RUnlock()
	if cached != nil {
		return cached, nil
	}

	// If the caller's ctx is already done, fail fast without kicking off
	// a detached HTTP fetch the caller will never wait for. Without this
	// guard, an expired ctx still triggers an upstream request through
	// WithoutCancel below.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Detach the shared fetch from this caller's cancellation so a
	// short-deadline first caller can't kill the HTTP request for
	// later waiters who selected the same in-flight singleflight. The
	// detach root is the owner's lifetime ctx when configured, so a
	// teardown (failed construction, Close) DOES cancel the fetch.
	fetchCtx := m.lifetime //nolint:contextcheck // deliberate detach: the owner's lifetime ctx, not the caller's, is the correct cancellation root for the shared singleflight fetch
	if fetchCtx == nil {
		fetchCtx = context.WithoutCancel(ctx)
	}

	// Slow path: single in-flight fetch, ctx-bounded wait per caller.
	ch := m.sf.DoChan("whoami", func() (interface{}, error) {
		// Re-check inside the singleflight critical region in case
		// another caller's Do completed and cached while we were
		// transitioning into the slow path.
		m.mu.RLock()
		c := m.cached
		m.mu.RUnlock()
		if c != nil {
			return c, nil
		}

		details, err := m.apiClient.FetchWhoAmI(fetchCtx)
		if err != nil {
			return nil, err
		}

		exp := time.Time(details.ExpireAt)
		// Validate BEFORE caching: the XML fields are zero-value
		// scalars, so an otherwise-OK empty <bookmaker_details/>
		// decoded "successfully" — New then succeeded while every later
		// AMQP dial used an empty virtual host.
		if details.BookmakerID == 0 || details.VirtualHost == "" || exp.IsZero() {
			return nil, fmt.Errorf("whoami: incomplete bookmaker details (bookmaker_id=%d, virtual_host=%q, expire_at=%v)", details.BookmakerID, details.VirtualHost, exp)
		}
		// An expire_at at or before now means the server itself reports
		// the credentials as already expired — caching them as valid let
		// gosdk.New succeed and authentication fail later, mid-broker /
		// mid-API work, far from the cause. Fail construction instead.
		// No skew tolerance: exp comes from the server, so accepting a
		// past timestamp only ever trades a clear startup error for a
		// deferred auth failure.
		if !exp.After(time.Now()) {
			return nil, fmt.Errorf("whoami: access token already expired at %v", exp)
		}
		// Warn when the token expires within the next week.
		if time.Until(exp) < 7*24*time.Hour {
			m.logger.Warn("api: access token expires soon", slog.Time("expire_at", exp))
		}

		// Close-gate: don't cache after the owner's lifetime ended — a
		// fetch that completed just before Close could otherwise pause,
		// outlive a successful Close, then resume and store (see the
		// equivalent re-check in lru.EventCache's admission).
		if fetchCtx.Err() != nil {
			return nil, fmt.Errorf("whoami: client closed during fetch: %w", fetchCtx.Err())
		}
		impl := bookmakerDetailImpl{
			expireAt:    exp,
			bookmakerID: details.BookmakerID,
			virtualHost: details.VirtualHost,
		}
		m.mu.Lock()
		m.cached = impl
		m.mu.Unlock()
		return impl, nil
	})

	select {
	case r := <-ch:
		if r.Err != nil {
			return nil, r.Err
		}
		return r.Val.(types.BookmakerDetail), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
