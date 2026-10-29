// Package logx configures structured logging (log/slog) for every binary.
package logx

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// New returns a logger writing to stderr. level: debug|info|warn|error.
// format: text|json. SAR_LOG_LEVEL and SAR_LOG_FORMAT override both.
func New(component, level, format string) *slog.Logger {
	return NewTo(os.Stderr, component, level, format)
}

// NewTo is New with an explicit writer (used by tests).
func NewTo(w io.Writer, component, level, format string) *slog.Logger {
	if v := os.Getenv("SAR_LOG_LEVEL"); v != "" {
		level = v
	}
	if v := os.Getenv("SAR_LOG_FORMAT"); v != "" {
		format = v
	}
	opts := &slog.HandlerOptions{Level: ParseLevel(level)}
	var h slog.Handler
	if strings.EqualFold(format, "json") {
		h = slog.NewJSONHandler(w, opts)
	} else {
		h = slog.NewTextHandler(w, opts)
	}
	return slog.New(h).With("component", component)
}

// ParseLevel maps a name to a slog level (default info).
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
