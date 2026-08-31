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
