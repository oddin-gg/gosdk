package log

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// captureLogger builds a Logger writing to an in-memory buffer for
// assertions against the formatted output.
func captureLogger(t *testing.T, level slog.Level) (*Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: level})
	return New(slog.New(h)), &buf
}

func TestNew_NilFallsBackToDefault(t *testing.T) {
	l := New(nil)
	if l == nil || l.s == nil {
		t.Fatalf("New(nil) should produce a usable logger, got %v", l)
	}
}

func TestNew_PreservesSuppliedLogger(t *testing.T) {
	s := slog.Default()
	l := New(s)
	if l.Slog() != s {
		t.Errorf("Slog() = %v, want %v", l.Slog(), s)
	}
}

func TestLogger_FormattedLevels(t *testing.T) {
	cases := []struct {
		name string
		fn   func(l *Logger)
		want string
	}{
		{"Debugf", func(l *Logger) { l.Debugf("d=%d", 1) }, "level=DEBUG msg=\"d=1\""},
		{"Infof", func(l *Logger) { l.Infof("i=%d", 2) }, "level=INFO msg=\"i=2\""},
		{"Warnf", func(l *Logger) { l.Warnf("w=%d", 3) }, "level=WARN msg=\"w=3\""},
		{"Errorf", func(l *Logger) { l.Errorf("e=%d", 4) }, "level=ERROR msg=\"e=4\""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l, buf := captureLogger(t, slog.LevelDebug)
			c.fn(l)
			if !strings.Contains(buf.String(), c.want) {
				t.Errorf("output %q does not contain %q", buf.String(), c.want)
			}
		})
	}
}

func TestLogger_StructuredLevels(t *testing.T) {
	l, buf := captureLogger(t, slog.LevelDebug)
	l.Debug("d", "k", "v")
	l.Info("i", "k", "v")
	l.Warn("w", "k", "v")
	l.Error("e", "k", "v")
	out := buf.String()
	for _, want := range []string{"level=DEBUG msg=d k=v", "level=INFO msg=i k=v", "level=WARN msg=w k=v", "level=ERROR msg=e k=v"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
}

func TestLogger_WithField_AppendsAttribute(t *testing.T) {
	l, buf := captureLogger(t, slog.LevelInfo)
	l.WithField("client_id", 42).Info("hello")
	if !strings.Contains(buf.String(), "client_id=42") {
		t.Errorf("WithField did not include attribute: %s", buf.String())
	}
}

func TestLogger_WithError_UsesErrKey(t *testing.T) {
	l, buf := captureLogger(t, slog.LevelInfo)
	l.WithError(errors.New("boom")).Error("failed")
	out := buf.String()
	if !strings.Contains(out, "err=boom") {
		t.Errorf("WithError did not use err key: %s", out)
	}
}

// TestLogger_WithFieldChainsIndependently verifies each WithField call
// returns a fresh Logger; mutating one doesn't bleed into others.
func TestLogger_WithFieldChainsIndependently(t *testing.T) {
	root, buf := captureLogger(t, slog.LevelInfo)
	a := root.WithField("a", 1)
	b := root.WithField("b", 2)
	a.Info("from-a")
	b.Info("from-b")
	out := buf.String()
	if !strings.Contains(out, "a=1") || !strings.Contains(out, "from-a") {
		t.Errorf("a context missing: %s", out)
	}
	if !strings.Contains(out, "b=2") || !strings.Contains(out, "from-b") {
		t.Errorf("b context missing: %s", out)
	}
	// Cross-pollination check: a's output must not have b=2 nor vice versa.
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.Contains(line, "from-a") && strings.Contains(line, "b=2") {
			t.Errorf("a's output leaked b: %s", line)
		}
		if strings.Contains(line, "from-b") && strings.Contains(line, "a=1") {
			t.Errorf("b's output leaked a: %s", line)
		}
	}
}
