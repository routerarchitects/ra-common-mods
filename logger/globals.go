package logger

import (
	"context"
	"log/slog"
	"sync"
)

var (
	globalConfig Config
	mu           sync.RWMutex
)

// Subsystem returns a logger with the subsystem attribute pre-bound.
// This is a helper for services.
func Subsystem(name string) *slog.Logger {
	mu.RLock()
	if _, ok := globalConfig.Levels.SubsystemLevels[name]; !ok {
		globalConfig.Levels.SubsystemLevels[name] = ParseLevel(globalConfig.Levels.DefaultLevel)
	}
	mu.RUnlock()

	// We assume L() is already the Handler Chain.
	return L().With("subsystem", name)
}

// GetConfig returns the current config.
func GetConfig() (Config, error) {
	return globalConfig, nil
}

// GetSubsystemLevels returns a copy of current levels.
func GetSubsystemLevels() (map[string]string, error) {
	levels := make(map[string]string)
	mu.RLock()
	defer mu.RUnlock()
	for k, v := range globalConfig.Levels.SubsystemLevels {
		levels[k] = v.String()
	}
	return levels, nil
}

// UpdateSubsystemLevels updates just the levels.
func UpdateSubsystemLevels(levels map[string]string) error {
	mu.Lock()
	defer mu.Unlock()
	cfg := globalConfig

	// We must initialize the map if nil, because config structs might have nil map initially
	if cfg.Levels.SubsystemLevels == nil {
		cfg.Levels.SubsystemLevels = make(map[string]slog.Level)
	}

	// Since we are applying on top of existing, we don't wipe previous
	for k, v := range levels {
		cfg.Levels.SubsystemLevels[k] = ParseLevel(v)
	}
	globalConfig = cfg
	return nil
}

// Legacy Shutdown
type ShutdownFunc func(ctx context.Context) error
