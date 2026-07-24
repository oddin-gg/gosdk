package feed

import (
	"strings"
	"testing"
)

// TestSanitizeVHostForError pins the dial-error vhost sanitization
// (Codex P3): the virtual host is server text validated only as
// non-empty, so it must be token-redacted, length-capped, and quoted
// before it is embedded in caller-facing errors.
func TestSanitizeVHostForError(t *testing.T) {
	const token = "super-secret-token"

	t.Run("token redacted", func(t *testing.T) {
		out := sanitizeVHostForError("/vhost-"+token+"-tail", token)
		if strings.Contains(out, token) {
			t.Errorf("sanitized vhost leaks the access token: %s", out)
		}
		if !strings.Contains(out, "[REDACTED]") {
			t.Errorf("sanitized vhost carries no redaction marker: %s", out)
		}
	})

	t.Run("length capped", func(t *testing.T) {
		long := strings.Repeat("x", 10_000)
		out := sanitizeVHostForError(long, token)
		if len(out) > vhostErrorMaxLen+16 { // quotes + ellipsis headroom
			t.Errorf("sanitized vhost not capped: len=%d", len(out))
		}
	})

	t.Run("control characters escaped", func(t *testing.T) {
		out := sanitizeVHostForError("/vh\x1b[2Jost\r\ninjected", token)
		for _, r := range out {
			if r < 0x20 {
				t.Fatalf("sanitized vhost contains a raw control character: %q", out)
			}
		}
	})

	t.Run("ordinary vhost readable", func(t *testing.T) {
		if out := sanitizeVHostForError("/vhost", token); out != `"/vhost"` {
			t.Errorf("ordinary vhost mangled: %s", out)
		}
	})

	t.Run("empty token no redaction panic", func(t *testing.T) {
		if out := sanitizeVHostForError("/vhost", ""); out != `"/vhost"` {
			t.Errorf("empty-token path mangled vhost: %s", out)
		}
	})
}
