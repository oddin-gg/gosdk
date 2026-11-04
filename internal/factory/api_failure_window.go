package factory

import (
	"sync"
	"time"

	"github.com/oddin-gg/gosdk/types"
)

const (
	// apiFailureWindowTTL is how long an event whose message-path build
	// failed skips the API.
	apiFailureWindowTTL = 10 * time.Second
	// apiFailureWindowMax bounds the number of remembered events.
	apiFailureWindowMax = 4096
)

// apiFailureWindow remembers the events whose message-path build just
// failed against the API. For a short time (apiFailureWindowTTL) a message
// for such an event is built without asking the API again: during an outage
// the first message of an event pays one short attempt and the rest of that
// event's messages go straight out (event == nil), instead of every message
// paying the full retry cycle on the session goroutine.
//
// The whole window ends the moment the API answers successfully again, from
// any call: entries carry the API's success counter as read before the
// failed attempt, and an entry is only honoured while the counter still has
// that value. Nothing stays refused once the API is back.
//
// The zero value is ready to use.
type apiFailureWindow struct {
	mu      sync.Mutex
	entries map[types.URN]apiFailureEntry
	// now is the clock; time.Now when nil.
	now func() time.Time
}

type apiFailureEntry struct {
	until time.Time
	count uint64
}

func (w *apiFailureWindow) clock() time.Time {
	if w.now != nil {
		return w.now()
	}
	return time.Now()
}

// blocked reports whether id failed recently and the API has not answered
// successfully since (count is the current success counter).
func (w *apiFailureWindow) blocked(id types.URN, count uint64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	e, ok := w.entries[id]
	if !ok {
		return false
	}
	if e.count != count || !w.clock().Before(e.until) {
		delete(w.entries, id)
		return false
	}
	return true
}

// record remembers that building id failed; count is the success counter
// read BEFORE the failed attempt, so a success that landed while it was in
// flight already voids the entry.
func (w *apiFailureWindow) record(id types.URN, count uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.entries == nil {
		w.entries = make(map[types.URN]apiFailureEntry)
	}
	now := w.clock()
	if _, exists := w.entries[id]; !exists && len(w.entries) >= apiFailureWindowMax {
		// Full: drop what is no longer honoured; if that frees nothing,
		// make room by dropping one arbitrary entry (the set is a
		// best-effort shield, not a ledger).
		for k, e := range w.entries {
			if e.count != count || !now.Before(e.until) {
				delete(w.entries, k)
			}
		}
		if len(w.entries) >= apiFailureWindowMax {
			for k := range w.entries {
				delete(w.entries, k)
				break
			}
		}
	}
	w.entries[id] = apiFailureEntry{until: now.Add(apiFailureWindowTTL), count: count}
}

// len is the number of remembered events (tests).
func (w *apiFailureWindow) len() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.entries)
}
