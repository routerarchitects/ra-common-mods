package logger

import (
	"fmt"
	"log/slog"
	"strings"
)

// GetAllLevels returns all supported levels as strings.
func GetAllLevels() []string {
	return []string{"debug", "info", "warn", "error"}
}

// ParseLevelChecked parses a string into a slog.Level and validates input.
func ParseLevelChecked(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("invalid log level: %q", s)
	}
}

// ParseLevel parses a string into a slog.Level.
// Unknown values are coerced to info for backwards compatibility.
func ParseLevel(s string) slog.Level {
	level, err := ParseLevelChecked(s)
	if err != nil {
		return slog.LevelInfo
	}
	return level
}
