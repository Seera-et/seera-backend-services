// Package logger builds the process-wide structured logger.
package logger

import (
	"io"
	"log/slog"
	"strings"
)

// New returns a logger writing to w: JSON in production so logs are queryable,
// human-readable text in development.
//
// Unknown levels fall back to info rather than failing — config validates the
// level, and a logger is needed to report configuration errors in the first
// place.
func New(w io.Writer, level string, dev bool) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(level)}
	if dev {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
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
