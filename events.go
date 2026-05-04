package gosdk

import (
	"time"

	"github.com/oddin-gg/gosdk/types"
)

// ConnectionEventKind enumerates the AMQP-level connection state transitions
// the Client emits on ConnectionEvents().
type ConnectionEventKind int

const (
	// ConnectionConnected is emitted after the first successful dial and
	// after every successful reconnect.
	ConnectionConnected ConnectionEventKind = iota

	// ConnectionDisconnected is emitted when the broker drops the connection
	// (NotifyClose). Err is populated.
	ConnectionDisconnected

	// ConnectionReconnecting is emitted once when the reconnect loop starts;
	// per-attempt detail goes to slog at debug level (NEXT.md §19.3).
	ConnectionReconnecting

	// ConnectionClosed is emitted after Client.Close completes the shutdown
	// sequence. Terminal.
	ConnectionClosed
)

// String returns a stable human-readable label.
func (k ConnectionEventKind) String() string {
	switch k {
	case ConnectionConnected:
		return "connected"
	case ConnectionDisconnected:
		return "disconnected"
	case ConnectionReconnecting:
		return "reconnecting"
	case ConnectionClosed:
		return "closed"
	default:
		return "unknown"
	}
}

// ConnectionEvent is delivered on Client.ConnectionEvents().
type ConnectionEvent struct {
	Kind ConnectionEventKind
	Err  error
	At   time.Time
}

// ConnectionState is the current state snapshot returned by
// Client.ConnectionState() — the polling escape hatch when the lossy event
// channel may have dropped a transition.
type ConnectionState int

const (
	// ConnectionStateNotConnected: AMQP has never been opened (or was closed).
	ConnectionStateNotConnected ConnectionState = iota
	// ConnectionStateConnecting: a dial or reconnect attempt is in flight.
	ConnectionStateConnecting
	// ConnectionStateConnected: AMQP connection is up.
	ConnectionStateConnected
	// ConnectionStateClosed: Client.Close has completed.
	ConnectionStateClosed
)

// String returns a stable human-readable label.
func (s ConnectionState) String() string {
	switch s {
	case ConnectionStateNotConnected:
		return "not_connected"
	case ConnectionStateConnecting:
		return "connecting"
	case ConnectionStateConnected:
		return "connected"
	case ConnectionStateClosed:
		return "closed"
	default:
		return "unknown"
	}
}

// APIEvent describes a single REST API call attempt the SDK made. One
// event per HTTP attempt — retries produce multiple events with
// incrementing Attempt numbers.
//
// Emitted only on Client.APIEvents() when WithAPICallLogging is set
// above APILogOff. URL is redacted to scheme://host/path (no query
// string); the access-token header is never emitted. Body bytes are
// captured according to APILogLevel and clamped at WithAPICallBodyLimit;
// Truncated is true when bytes were dropped.
type APIEvent struct {
	At        time.Time
	Method    string
	URL       string
	Status    int // 0 on transport-level failures (no HTTP response)
	Latency   time.Duration
	Attempt   int
	Locale    *types.Locale
	Request   []byte
	Response  []byte
	Truncated bool
	Err       error
}

