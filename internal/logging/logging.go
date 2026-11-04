// Package logging builds the agent's structured logger. Output goes to stderr, which systemd sends to journald.
package logging

import (
	"io"
	"log/slog"
)

// New returns a JSON logger. level is assumed to be validated by config ("debug", "info", "warn", "error").
func New(w io.Writer, level string) *slog.Logger {
	var l slog.Level
	_ = l.UnmarshalText([]byte(level))
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: l}))
}
