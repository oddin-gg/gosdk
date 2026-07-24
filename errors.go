package gosdk

import (
	"errors"

	"github.com/oddin-gg/gosdk/internal/api"
	"github.com/oddin-gg/gosdk/internal/cache"
	"github.com/oddin-gg/gosdk/internal/producer"
	"github.com/oddin-gg/gosdk/internal/sdkerr"
)

// APIError is the typed error surfaced (wrapped) by catalog / entity /
// recovery methods when an HTTP API call fails: non-2xx responses, and
// 2xx responses whose decoded envelope carries a non-OK response_code.
// Branch on it with errors.As instead of parsing error strings:
//
//	var apiErr *gosdk.APIError
//	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound { ... }
//	if errors.As(err, &apiErr) && apiErr.Code != "" { /* envelope code */ }
//
// Type ALIAS to the internal implementation (same pattern as
// RecoveryHandle): the concrete type lives in internal/api, which
// consumers cannot import — without this alias the advertised errors.As
// pattern was unusable outside the SDK. Its exported field set
// (Method, Path, Status, Code, Message) is public v1.0.0 contract.
type APIError = api.Error

// Exported sentinel errors (NEXT.md §10). Match with errors.Is.
var (
	// ErrAlreadyClosed is returned when an operation is attempted on a
	// Client that is closing or closed — Connect, Subscribe, Recover*,
	// and the replay broker-open path all report it — and it is the
	// terminal cause on Subscription.Err() when the parent Client's
	// Close aborted the subscription. Catalog/entity methods that hit
	// the HTTP layer after Close surface it too (the internal API
	// client's closed-error wraps this sentinel); reads served entirely
	// from a warm cache may still SUCCEED after Close — cached data
	// needs no I/O and remains valid.
	ErrAlreadyClosed = sdkerr.ErrClosed

	// ErrInvalidConfig is returned (wrapped, with detail) by New when
	// the supplied Config cannot possibly work: missing access token,
	// or an environment/region combination that resolves to no API or
	// MQ endpoint.
	ErrInvalidConfig = errors.New("gosdk: invalid configuration")

	// ErrProducerNotFound is returned by Client.Producer (and the
	// other by-id producer operations) when the id is not in the
	// producer catalog. Pre-fix an unknown id fabricated a synthetic
	// active/enabled producer — indistinguishable from a real catalog
	// member. Match with errors.Is.
	ErrProducerNotFound = producer.ErrProducerNotFound

	// ErrManagerNotOpen is returned by feed-dependent operations
	// (RecoverEventOdds, RecoverEventStateful) invoked before the feed
	// pipeline is up — call Connect first, or let Subscribe lazy-connect
	// (see doc.go "API-only mode"). Match with errors.Is; retryable
	// once the client is connected.
	ErrManagerNotOpen = errors.New("gosdk: recovery manager not open (call Connect first)")

	// ErrItemNotFound is the "definitive absence" sentinel on catalog /
	// entity lookups (Sport, Match, Tournament, MarketDescription,
	// player, fixture and match-status reads, …): the lookup — and any
	// API fetch
	// it triggered — completed without a transport failure, but the
	// requested entity does not exist upstream. Distinguishes "API said
	// no such item" (not retryable) from API/network errors (see
	// APIError; often retryable). Match with errors.Is. Alias of the
	// internal cache sentinel — internal wrap sites promised errors.Is
	// classification, but the sentinel lived in internal/cache, which
	// consumers cannot import to name it.
	ErrItemNotFound = cache.ErrItemNotFoundInCache

	// ErrMarketLocaleIncomplete is returned (wrapped) by market-
	// description reads when the requested locales were LOADED but the
	// upstream catalog carries no (or malformed, skipped-at-decode)
	// entry for this market in some requested locale. Unlike a
	// transient failure, retrying will not help until the upstream
	// catalog itself changes. Match with errors.Is.
	ErrMarketLocaleIncomplete = cache.ErrMarketLocaleIncomplete

	// ErrSportLocaleIncomplete is returned (wrapped) by Sport when the
	// requested locales were LOADED but the sport catalog carries no
	// name/abbreviation for that sport in some requested locale (the
	// sport is present in a subset of the requested locales only). The
	// sport-catalog analogue of ErrMarketLocaleIncomplete; Sports omits
	// such sports rather than returning partially-localized entries.
	// Retrying will not help until the upstream catalog itself changes.
	// Match with errors.Is.
	ErrSportLocaleIncomplete = cache.ErrSportLocaleIncomplete

	// ErrLocaleNotLoaded signals that a localized lookup was asked for a
	// locale that was not requested/preloaded. Public entity VALUE
	// accessors no longer return it directly — they expose per-locale data
	// as Optional[T] (a missing locale is None, not an error). It is
	// surfaced (wrapped) on the internal enrichment / feed-processing
	// paths and, most visibly, as the terminal cause when a subscription
	// using the throw-on-missing locale strategy hits an unloaded locale.
	// Distinct from ErrItemNotFound: the entity exists, the locale was
	// simply not loaded. Match with errors.Is.
	ErrLocaleNotLoaded = cache.ErrLocaleNotLoaded
)
