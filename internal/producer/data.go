package producer

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/oddin-gg/gosdk/internal/api/xml"
	"github.com/oddin-gg/gosdk/internal/config"
	"github.com/oddin-gg/gosdk/types"
)

// data carries the per-producer state. ALL fields are guarded by mu.
// The manager reuses the SAME *data across catalog refreshes (Open),
// mutating the catalog fields in place via refreshCatalog rather than
// allocating a fresh object — so a producer handle already returned to
// a caller keeps observing subsequent state changes (its accessors read
// through this pointer). id is the map key and never changes.
//
// Readers (producerImpl accessors, buildProducerImpl) hold mu.RLock;
// writers (the manager's setters and refreshCatalog) hold mu.Lock.
type data struct {
	mu sync.RWMutex

	// Catalog fields — refreshed in place by refreshCatalog on Open.
	id                              int
	name                            xml.MQSubscriptionTypeName
	description                     string
	active                          bool
	apiEndpoint                     string
	producerScope                   xml.Scope
	statefulRecoveryWindowInMinutes int

	// Runtime / caller-owned mutable fields.
	lastMessageTimestamp             time.Time
	enabled                          bool
	flaggedDown                      bool
	lastProcessedMessageGenTimestamp time.Time
	lastAliveReceivedGenTimestamp    time.Time
	recoveryFromTimestamp            time.Time
	// recoveryFromExplicit marks recoveryFromTimestamp as an explicit
	// caller override (SetProducerRecoveryFromTimestamp) that must win
	// over lastAliveReceivedGenTimestamp for the NEXT snapshot recovery —
	// the setter's documented purpose is REWINDING the recovery point,
	// which a live alive timestamp would otherwise defeat. One-shot:
	// cleared once a recovery is successfully recorded (clearRecoveryFrom-
	// Override), so subsequent recoveries resume from the freshest alive
	// cursor rather than re-rewinding to the same point forever.
	recoveryFromExplicit bool
	lastRecoveryInfo     types.RecoveryInfo
}

// refreshCatalog updates the catalog fields from a fresh API payload
// under mu, leaving all runtime / caller-owned state untouched. Reusing
// the *data (instead of replacing it) is what keeps previously returned
// producer handles live across an Open — see the manager's Open.
func (d *data) refreshCatalog(p xml.Producer) {
	d.mu.Lock()
	d.name = p.Name
	d.description = p.Description
	d.active = p.Active
	d.apiEndpoint = p.APIEndpoint
	d.producerScope = p.Scope
	d.statefulRecoveryWindowInMinutes = p.RecoveryWindow
	d.mu.Unlock()
}

func newData(producer xml.Producer) *data {
	return &data{
		id:                              producer.ID,
		name:                            producer.Name,
		description:                     producer.Description,
		active:                          producer.Active,
		apiEndpoint:                     producer.APIEndpoint,
		producerScope:                   producer.Scope,
		statefulRecoveryWindowInMinutes: producer.RecoveryWindow,
		enabled:                         producer.Active,
		flaggedDown:                     true,
	}
}

const statefulRecoveryMinutes = 4320

// producerImpl is the types.Producer handle. The value fields are the
// construction-time snapshot; they are authoritative ONLY when
// producerData is nil (the synthetic unknown-producer handle, which has
// no live catalog entry). Every accessor with a live producerData reads
// through it under RLock — the manager's Open refreshes the SAME *data
// in place (refreshCatalog), so a handle obtained before Connect must
// observe the refreshed catalog (name, availability, endpoint, scopes,
// recovery window), not its construction-time values.
type producerImpl struct {
	id                              int
	active                          bool
	name                            string
	description                     string
	enabled                         bool
	apiEndpoint                     string
	producerScopes                  []types.ProducerScope
	statefulRecoveryWindowInMinutes int
	producerData                    *data
}

func (p producerImpl) ID() int {
	return p.id
}

func (p producerImpl) Name() string {
	if p.producerData == nil {
		return p.name
	}
	p.producerData.mu.RLock()
	defer p.producerData.mu.RUnlock()
	return string(p.producerData.name)
}

func (p producerImpl) Description() string {
	if p.producerData == nil {
		return p.description
	}
	p.producerData.mu.RLock()
	defer p.producerData.mu.RUnlock()
	return p.producerData.description
}

func (p producerImpl) LastMessageTimestamp() time.Time {
	if p.producerData == nil {
		return time.Time{}
	}
	p.producerData.mu.RLock()
	defer p.producerData.mu.RUnlock()
	return p.producerData.lastMessageTimestamp
}

func (p producerImpl) IsAvailable() bool {
	if p.producerData == nil {
		return p.active
	}
	p.producerData.mu.RLock()
	defer p.producerData.mu.RUnlock()
	return p.producerData.active
}

// IsEnabled returns the current enabled flag. The producerImpl's own
// `enabled` snapshot is captured at construction time and may be
// stale; the authoritative reading is on producerData under lock.
func (p producerImpl) IsEnabled() bool {
	if p.producerData == nil {
		return p.enabled
	}
	p.producerData.mu.RLock()
	defer p.producerData.mu.RUnlock()
	return p.producerData.enabled
}

func (p producerImpl) IsFlaggedDown() bool {
	if p.producerData == nil {
		return true
	}
	p.producerData.mu.RLock()
	defer p.producerData.mu.RUnlock()
	return p.producerData.flaggedDown
}

func (p producerImpl) APIEndpoint() string {
	if p.producerData == nil {
		return p.apiEndpoint
	}
	p.producerData.mu.RLock()
	defer p.producerData.mu.RUnlock()
	return p.producerData.apiEndpoint
}

func (p producerImpl) ProducerScopes() []types.ProducerScope {
	if p.producerData == nil {
		// Fresh slice, matching every other branch: returning the
		// internal backing slice let a caller mutate subsequent
		// ProducerScopes()/ProducerHasScope results.
		return slices.Clone(p.producerScopes)
	}
	p.producerData.mu.RLock()
	raw := p.producerData.producerScope
	p.producerData.mu.RUnlock()
	scopes, err := parseProducerScopes(raw)
	if err != nil {
		// A refreshed catalog carrying an unparseable scope shouldn't
		// nuke an accessor with no error slot — fall back to the
		// construction-time scopes (which passed validation when the
		// handle was built). Fresh slice either way: callers can't
		// mutate shared state.
		out := make([]types.ProducerScope, len(p.producerScopes))
		copy(out, p.producerScopes)
		return out
	}
	return scopes
}

func (p producerImpl) LastProcessedMessageGenTimestamp() time.Time {
	if p.producerData == nil {
		return time.Time{}
	}
	p.producerData.mu.RLock()
	defer p.producerData.mu.RUnlock()
	return p.producerData.lastProcessedMessageGenTimestamp
}

func (p producerImpl) ProcessingQueDelay() time.Duration {
	return time.Since(p.LastProcessedMessageGenTimestamp())
}

func (p producerImpl) TimestampForRecovery() time.Time {
	if p.producerData == nil {
		return time.Time{}
	}
	p.producerData.mu.RLock()
	defer p.producerData.mu.RUnlock()
	// An explicit caller override wins over the alive-derived cursor (its
	// purpose is rewinding the recovery point — see recoveryFromExplicit).
	// Absent an override, prefer the freshest known-good cursor
	// (lastAliveReceivedGenTimestamp) to avoid re-recovering already-
	// processed data, falling back to any pre-connect recoveryFromTimestamp.
	if p.producerData.recoveryFromExplicit {
		return p.producerData.recoveryFromTimestamp
	}
	if !p.producerData.lastAliveReceivedGenTimestamp.IsZero() {
		return p.producerData.lastAliveReceivedGenTimestamp
	}
	return p.producerData.recoveryFromTimestamp
}

func (p producerImpl) StatefulRecoveryWindowInMinutes() int {
	if p.producerData == nil {
		return p.statefulRecoveryWindowInMinutes
	}
	p.producerData.mu.RLock()
	defer p.producerData.mu.RUnlock()
	return p.producerData.statefulRecoveryWindowInMinutes
}

// RecoveryInfo returns a snapshot of the most recent recovery summary
// the manager recorded for this producer, or nil if no recovery info
// has been recorded yet. Callers can rely on `if p.RecoveryInfo() != nil`
// as a presence check.
//
// The returned pointer-to-interface shape is a legacy quirk of the
// types.Producer surface; consumers typically dereference once to call
// the RecoveryInfo accessor methods.
func (p producerImpl) RecoveryInfo() *types.RecoveryInfo {
	if p.producerData == nil {
		return nil
	}
	p.producerData.mu.RLock()
	defer p.producerData.mu.RUnlock()
	if p.producerData.lastRecoveryInfo == nil {
		return nil
	}
	out := p.producerData.lastRecoveryInfo
	return &out
}

// producerSnapshot is a consistent, lock-free copy of a *data's fields,
// taken under one RLock. Separating the snapshot from the scope parse
// lets callers inspect `active` and skip inactive producers BEFORE
// parsing scopes — only the active set needs valid scopes, so an
// inactive producer with an empty/unknown scope must not fail the
// active-set queries.
type producerSnapshot struct {
	id        int
	active    bool
	name      string
	desc      string
	endpoint  string
	scopeRaw  xml.Scope
	recWindow int
	enabled   bool
	src       *data
}

// snapshotProducer copies every field under one RLock. Catalog fields
// are now mutable (refreshCatalog rewrites them on Open), so they can no
// longer be read without the lock. Live state (enabled, timestamps, …)
// is still re-read through src under lock by the accessors.
func snapshotProducer(producerData *data) producerSnapshot {
	producerData.mu.RLock()
	defer producerData.mu.RUnlock()
	return producerSnapshot{
		id:        producerData.id,
		active:    producerData.active,
		name:      string(producerData.name),
		desc:      producerData.description,
		endpoint:  producerData.apiEndpoint,
		scopeRaw:  producerData.producerScope,
		recWindow: producerData.statefulRecoveryWindowInMinutes,
		enabled:   producerData.enabled,
		src:       producerData,
	}
}

// build parses the snapshot's scope string and assembles the
// producerImpl. Returns an error only when the scope set is empty or
// unknown.
func (s producerSnapshot) build() (*producerImpl, error) {
	producerScopes, err := parseProducerScopes(s.scopeRaw)
	if err != nil {
		return nil, err
	}
	return &producerImpl{
		id:                              s.id,
		active:                          s.active,
		name:                            s.name,
		description:                     s.desc,
		enabled:                         s.enabled,
		apiEndpoint:                     s.endpoint,
		producerScopes:                  producerScopes,
		statefulRecoveryWindowInMinutes: s.recWindow,
		producerData:                    s.src,
	}, nil
}

func buildProducerImpl(producerData *data) (*producerImpl, error) {
	return snapshotProducer(producerData).build()
}

// parseProducerScopes maps the "|"-delimited wire scope string to the
// typed slice, rejecting unknown or empty scope sets.
func parseProducerScopes(raw xml.Scope) ([]types.ProducerScope, error) {
	var scopes []types.ProducerScope
	for _, scope := range strings.Split(string(raw), "|") {
		switch xml.Scope(scope) {
		case xml.ScopeLive:
			scopes = append(scopes, types.LiveProducerScope)
		case xml.ScopePrematch:
			scopes = append(scopes, types.PrematchProducerScope)
		default:
			return nil, fmt.Errorf("unknown producer scope %s", raw)
		}
	}
	if len(scopes) == 0 {
		return nil, fmt.Errorf("unknown producer scopes %s", raw)
	}
	return scopes, nil
}

func buildProducerImplFromUnknown(unknownProducerID int, cfg config.Config) (*producerImpl, error) {
	apiURL, err := cfg.APIURL()
	if err != nil {
		return nil, err
	}
	return &producerImpl{
		id:                              unknownProducerID,
		active:                          true,
		name:                            "unknown",
		description:                     "unknown producer",
		enabled:                         true,
		apiEndpoint:                     apiURL,
		producerScopes:                  []types.ProducerScope{types.LiveProducerScope, types.PrematchProducerScope},
		statefulRecoveryWindowInMinutes: statefulRecoveryMinutes,
	}, nil
}
