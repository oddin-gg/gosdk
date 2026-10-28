package utils

import (
	"strings"
	"testing"
)

// TestRoutePreview_EscapesAndBounds pins the untrusted routing-key
// logging defense: publisher-controlled keys can carry CR/LF and
// terminal escapes (AMQP shortstr allows arbitrary octets), which must
// render escaped so a crafted key cannot forge log lines for consumers
// using their own slog handler; pathological keys are length-capped.
func TestRoutePreview_EscapesAndBounds(t *testing.T) {
	got := RoutePreview("a\n2026-08-31 INFO forged line\x1b[31m")
	if strings.Contains(got, "\n") || strings.Contains(got, "\x1b") {
		t.Fatalf("RoutePreview = %q, raw control bytes must not survive", got)
	}
	if !strings.Contains(got, `\n`) {
		t.Fatalf("RoutePreview = %q, want the newline visibly escaped", got)
	}
	long := strings.Repeat("k", 300)
	if got := RoutePreview(long); len(got) > 140 {
		t.Fatalf("RoutePreview(long) = %d bytes, want capped", len(got))
	}
}

// TestIdentifierPreview_KeepsShortIdsAndBoundsLongOnes: an ordinary id
// stays readable, a huge or control-laden one is cut to an escaped prefix
// with its full length alongside.
func TestIdentifierPreview_KeepsShortIdsAndBoundsLongOnes(t *testing.T) {
	if got, want := IdentifierPreview("od:match:7"), `"od:match:7" (len=10)`; got != want {
		t.Fatalf("IdentifierPreview(short) = %s, want %s", got, want)
	}
	long := "a\n" + strings.Repeat("b", 1<<20)
	got := IdentifierPreview(long)
	if len(got) > 2*identifierPreviewBytes+32 {
		t.Fatalf("IdentifierPreview is %d bytes for a %d byte id", len(got), len(long))
	}
	if strings.ContainsAny(got, "\n\r") || !strings.Contains(got, `\n`) {
		t.Fatalf("IdentifierPreview = %q, want control bytes escaped", got)
	}
	if !strings.HasSuffix(got, "(len=1048578)") {
		t.Fatalf("IdentifierPreview = %q, want the full length", got)
	}
}

// TestIdentifierPreview_EscapesControlBytes: the value is publisher-
// controlled, so a CR/LF, an ESC (terminal escape sequence) or a NUL in it
// must never reach a log line raw — for consumers that supply their own
// slog handler the escaping is the only defense against forged lines.
func TestIdentifierPreview_EscapesControlBytes(t *testing.T) {
	got := IdentifierPreview("od:match:1\r\n2026-10-28 INFO forged\x1b[31m\x00tail")
	for _, raw := range []string{"\r", "\n", "\x1b", "\x00"} {
		if strings.Contains(got, raw) {
			t.Fatalf("IdentifierPreview = %q carries the raw byte %q", got, raw)
		}
	}
	for _, escaped := range []string{`\r`, `\n`, `\x1b`, `\x00`} {
		if !strings.Contains(got, escaped) {
			t.Fatalf("IdentifierPreview = %q, want %s visibly escaped", got, escaped)
		}
	}
}
