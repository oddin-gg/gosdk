// Package log is a thin wrapper around log/slog that preserves the
// printf-style call sites the SDK inherited from logrus.
//
// This exists so the Phase 6 migration could drop github.com/sirupsen/logrus
// from go.mod without restructuring ~50 individual call sites. Future work
// can move call sites to native slog (slog.String / slog.Int / etc.) one
// package at a time; the wrapper keeps the existing surface stable in the
// meantime.
package log

import (
	"fmt"
	"log/slog"
	"os"
)

// Logger is the SDK's internal logger. Methods mirror the most common
// logrus.Entry methods; everything is backed by *slog.Logger.
type Logger struct {
	s *slog.Logger
}

// New constructs a Logger. Pass nil for slog.Default()-derived logger
// configured at info level on stderr.
func New(s *slog.Logger) *Logger {
	if s == nil {
		s = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	return &Logger{s: s}
}

// Slog returns the underlying *slog.Logger for native-slog call sites.
func (l *Logger) Slog() *slog.Logger { return l.s }

// WithField returns a child logger with an additional structured attr.
func (l *Logger) WithField(key string, val any) *Logger {
	return &Logger{s: l.s.With(key, val)}
}

// WithError attaches an error under the conventional `err` key.
func (l *Logger) WithError(err error) *Logger {
	return &Logger{s: l.s.With("err", err)}
}

// Errorf logs a formatted message at error level.
func (l *Logger) Errorf(format string, args ...any) {
	l.s.Error(fmt.Sprintf(format, args...))
}

// Warnf logs a formatted message at warn level.
func (l *Logger) Warnf(format string, args ...any) {
	l.s.Warn(fmt.Sprintf(format, args...))
}

// Infof logs a formatted message at info level.
func (l *Logger) Infof(format string, args ...any) {
	l.s.Info(fmt.Sprintf(format, args...))
}

// Debugf logs a formatted message at debug level.
func (l *Logger) Debugf(format string, args ...any) {
	l.s.Debug(fmt.Sprintf(format, args...))
}

// Error logs a structured message at error level. args are slog-style
// alternating key/value pairs.
func (l *Logger) Error(msg string, args ...any) { l.s.Error(msg, args...) }

// Warn logs a structured message at warn level.
func (l *Logger) Warn(msg string, args ...any) { l.s.Warn(msg, args...) }

// Info logs a structured message at info level.
func (l *Logger) Info(msg string, args ...any) { l.s.Info(msg, args...) }

// Debug logs a structured message at debug level.
func (l *Logger) Debug(msg string, args ...any) { l.s.Debug(msg, args...) }
