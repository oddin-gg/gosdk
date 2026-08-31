package utils

import (
	"bytes"
	"crypto/sha256"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// payloadPreviewBytes bounds the escaped payload prefix included in
// diagnostic logs for malformed feed bodies.
const payloadPreviewBytes = 256

// PayloadPreview renders a BOUNDED diagnostic form of an untrusted
// payload for logging: total length, a SHA-256 prefix (correlate
// occurrences without retaining content), and an escaped, truncated
// prefix. Malformed feed bodies are attacker-/upstream-controlled and
// unbounded — logging them whole (pre-fix `string(d.Body)` / `%v` of
// the message struct) risked huge transient allocations, log-I/O
// pressure and disk exhaustion under a malformed-message flood, and
// wholesale disclosure of proprietary feed payloads in log storage.
// strconv.Quote escapes control bytes so a crafted payload cannot
// inject log lines or terminal escapes.
func PayloadPreview(b []byte) string {
	sum := sha256.Sum256(b)
	preview := b
	truncated := ""
	if len(preview) > payloadPreviewBytes {
		preview = preview[:payloadPreviewBytes]
		truncated = "…"
	}
	return fmt.Sprintf("len=%d sha256=%x preview=%s%s", len(b), sum[:8], strconv.Quote(string(preview)), truncated)
}

// routePreviewBytes bounds the escaped routing-key form included in
// diagnostic logs. AMQP shortstr allows up to 255 bytes; the bound is
// belt-and-braces against pathological keys.
const routePreviewBytes = 128

// RoutePreview renders an untrusted AMQP routing key for logging:
// escaped via strconv.Quote (so a publisher-controlled key carrying
// CR/LF or terminal escapes cannot forge log lines — the same defense
// PayloadPreview applies to the delivery body) and length-capped.
// slog's stock handlers escape the message themselves; this protects
// consumers that supply their own handler via WithLogger.
func RoutePreview(route string) string {
	truncated := ""
	if len(route) > routePreviewBytes {
		route = route[:routePreviewBytes]
		truncated = "…"
	}
	return strconv.Quote(route) + truncated
}

// EnsureXMLEOF consumes dec's remaining token stream, permitting only
// trailing whitespace, comments, processing instructions, and
// directives. Both XML decode paths (feed messages, API responses)
// call it after decoding the root element: without the check, a body
// carrying a second document after the first root decoded
// "successfully" while the trailer was silently discarded.
func EnsureXMLEOF(dec *xml.Decoder) error {
	for {
		tok, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		switch t := tok.(type) {
		case xml.CharData:
			if len(bytes.TrimSpace(t)) != 0 {
				return fmt.Errorf("trailing content after document root")
			}
		case xml.Comment, xml.ProcInst, xml.Directive:
			// benign trailer
		default:
			return fmt.Errorf("trailing document/content after root (%T)", tok)
		}
	}
}
