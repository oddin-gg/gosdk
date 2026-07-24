package api

import (
	"fmt"

	"github.com/oddin-gg/gosdk/types"
)

// Error is the typed error the API client returns for failed calls:
// non-2xx HTTP responses, and 2xx responses whose decoded envelope
// carries a non-OK response_code. Callers branch on it with errors.As
// instead of parsing error strings:
//
//	var apiErr *api.Error
//	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound { ... }
type Error struct {
	Method string
	// Path is the request path as passed to do(), including any query
	// string. Never embed it in observability streams without going
	// through redactEventErr.
	Path   string
	Status int
	// Code is the envelope response_code; set only when the failure is
	// a decoded non-OK envelope on an otherwise-successful response.
	Code types.ResponseCode
	// Message is the server-supplied error message, when the error
	// body decoded.
	Message string
}

func (e *Error) Error() string {
	// A 2xx HTTP status carrying a non-OK envelope code is a payload-level
	// rejection with no meaningful HTTP error status — lead with the code.
	// GATED on 2xx: Code is now also populated on non-2xx errors (so
	// consumers can classify the envelope code via errors.As), and those
	// must keep the status-first format rather than fall into this branch
	// and drop the HTTP status.
	if e.Code != "" && e.Status >= 200 && e.Status < 300 {
		return fmt.Sprintf("api: not acceptable response code from %s: %s", e.Path, e.Code)
	}
	if e.Message != "" {
		return fmt.Sprintf("api: %s %s: status %d: %s", e.Method, e.Path, e.Status, e.Message)
	}
	return fmt.Sprintf("api: %s %s: status %d", e.Method, e.Path, e.Status)
}

// redactedError is the observability-safe wrapper attached to
// APIEvent.Err: its message has the access token and the request's
// query string scrubbed, and its Unwrap chain contains ONLY a
// sanitized stand-in (see sanitizedCause) — never the original error.
// Retaining the raw cause would defeat the redaction for any consumer
// that unwraps: errors.As could recover an *Error whose Path carries
// the query string and whose Message carries the unredacted server
// text, or a *url.Error rendering the full URL.
type redactedError struct {
	msg   string
	cause error // sanitized; nil when no secret-free classification exists
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.cause }
