// Package logging is a tiny slog-backed shim that preserves the call-site API
// the ibkr package was originally written against (Component(name).Debugf/Infof/...).
//
// The package writes through the standard library's log/slog so consumers can
// install any slog handler (text, JSON, custom) before the ibkr package starts
// emitting events. SetDefault(nil) silences output entirely.
package logging

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
)

// Level mirrors the legacy four-level scheme.
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

var (
	currentLevel atomic.Int32
	logger       atomic.Pointer[slog.Logger]
)

func init() {
	currentLevel.Store(int32(LevelInfo))
	logger.Store(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
}

// Configure sets the active level by string ("debug"|"info"|"warn"|"error").
// Unknown values default to info.
func Configure(level string) {
	currentLevel.Store(int32(parseLevel(level)))
	lv := slog.LevelInfo
	switch parseLevel(level) {
	case LevelDebug:
		lv = slog.LevelDebug
	case LevelWarn:
		lv = slog.LevelWarn
	case LevelError:
		lv = slog.LevelError
	}
	logger.Store(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lv})))
}

// SetLogger installs a custom slog.Logger. Pass nil to discard all output.
func SetLogger(l *slog.Logger) {
	if l == nil {
		l = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	logger.Store(l)
}

// LevelEnabled reports whether the given level would produce output.
func LevelEnabled(l Level) bool {
	return int32(l) >= currentLevel.Load()
}

func parseLevel(s string) Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return LevelDebug
	case "warn", "warning":
		return LevelWarn
	case "error":
		return LevelError
	default:
		return LevelInfo
	}
}

func emit(l Level, prefix, format string, args ...any) {
	if !LevelEnabled(l) {
		return
	}
	msg := fmt.Sprintf(format, args...)
	lg := logger.Load()
	if lg == nil {
		return
	}
	attrs := []any{}
	if prefix != "" {
		attrs = append(attrs, slog.String("component", prefix))
	}
	switch l {
	case LevelDebug:
		lg.LogAttrs(context.Background(), slog.LevelDebug, msg, asAttrs(attrs)...)
	case LevelInfo:
		lg.LogAttrs(context.Background(), slog.LevelInfo, msg, asAttrs(attrs)...)
	case LevelWarn:
		lg.LogAttrs(context.Background(), slog.LevelWarn, msg, asAttrs(attrs)...)
	case LevelError:
		lg.LogAttrs(context.Background(), slog.LevelError, msg, asAttrs(attrs)...)
	}
}

func asAttrs(in []any) []slog.Attr {
	out := make([]slog.Attr, 0, len(in))
	for _, v := range in {
		if a, ok := v.(slog.Attr); ok {
			out = append(out, a)
		}
	}
	return out
}

// Entry tags log lines with a component name.
type Entry struct{ prefix string }

// Component returns a logger entry tagged with the component name.
func Component(name string) Entry { return Entry{prefix: name} }

func (e Entry) Debugf(format string, args ...any)     { emit(LevelDebug, e.prefix, format, args...) }
func (e Entry) Infof(format string, args ...any)      { emit(LevelInfo, e.prefix, format, args...) }
func (e Entry) Highlightf(format string, args ...any) { emit(LevelInfo, e.prefix, format, args...) }
func (e Entry) Warnf(format string, args ...any)      { emit(LevelWarn, e.prefix, format, args...) }
func (e Entry) Errorf(format string, args ...any)     { emit(LevelError, e.prefix, format, args...) }

// Package-level helpers for callers that don't carry an Entry.
func Debugf(format string, args ...any)     { emit(LevelDebug, "", format, args...) }
func Infof(format string, args ...any)      { emit(LevelInfo, "", format, args...) }
func Highlightf(format string, args ...any) { emit(LevelInfo, "", format, args...) }
func Warnf(format string, args ...any)      { emit(LevelWarn, "", format, args...) }
func Errorf(format string, args ...any)     { emit(LevelError, "", format, args...) }

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
