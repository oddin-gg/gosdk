package api

import (
	"net/url"
	"time"

	"github.com/oddin-gg/gosdk/types"
)

// Response is the per-API-call payload published to the registered
// OnAPIResponse listeners (currently the cache layer's MatchStatusCache).
// Pre-v2.32 this lived in types.Response; relocated here because the
// shape is purely internal-facing — consumers never receive it
// (they get the higher-level APIEvent on the APIEvents() channel).
type Response struct {
	Data   interface{}
	URL    *url.URL
	Locale *types.Locale
	// StartedAt is when the fetch producing this response BEGAN.
	// Observer-driven caches compare it against their clear tombstones:
	// data from a fetch that started before an invalidation must not
	// re-populate the cleared entry — regardless of WHICH code path
	// initiated the fetch (the status cache's own loader, the match
	// cache's summary load, ...). Carrying the origin on the response
	// makes the tombstone airtight across foreign fetchers.
	StartedAt time.Time
}

// ResponseWithCode is the internal seam used by the API client to
// extract structured error envelopes from XML responses (e.g.
// `<response response_code="FORBIDDEN">`). Pre-v2.32 this lived in
// types.ResponseWithCode; the consumer-facing ResponseCode enum
// stays in types/.
type ResponseWithCode interface {
	Code() types.ResponseCode
}
