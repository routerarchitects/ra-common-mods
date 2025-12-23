package logger

import (
	"log/slog"
	"strings"
)

// GetAllLevels returns all supported levels as strings.
func GetAllLevels() []string {
	return []string{"debug", "info", "warn", "error"}
}

// ParseLevel parses a string into a slog.Level.
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
