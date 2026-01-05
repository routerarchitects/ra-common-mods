package logger

import (
	"context"
	"log/slog"
	"maps"
	"sync"
	"sync/atomic"
)

var (
	globalConfig atomic.Value
	mu           sync.Mutex
)

// Subsystem returns a logger with the subsystem attribute pre-bound.
// This is a helper for services.
func Subsystem(name string) *slog.Logger {
	mu.Lock()
	defer mu.Unlock()

	// Re-read config under lock in case it changed
	cfg := GetConfig()
	if _, ok := cfg.Levels.SubsystemLevels[name]; ok {
		return L().With("subsystem", name)
	}

	// Copy-On-Write:
	newCfg := GetCopyConfig()

	// Add new subsystem with default level
	newCfg.Levels.SubsystemLevels[name] = ParseLevel(cfg.Levels.DefaultLevel)
	globalConfig.Store(newCfg)

	// We assume L() is already the Handler Chain.
	return L().With("subsystem", name)
}

// GetCopyConfig returns a copy of the current config.
func GetCopyConfig() Config {
	val := globalConfig.Load()
	if val == nil {
		return Config{}
	}
	cfg := val.(Config)

	// Copy-On-Write: Create new map
	newLevels := make(map[string]slog.Level)
	if cfg.Levels.SubsystemLevels != nil {
		maps.Copy(newLevels, cfg.Levels.SubsystemLevels)
	}
	cfg.Levels.SubsystemLevels = newLevels
	return cfg
}

// GetConfig returns the current config.
func GetConfig() Config {
	val := globalConfig.Load()
	if val == nil {
		return Config{}
	}
	return val.(Config)
}

// GetSubsystemLevels returns a copy of current levels.
func GetSubsystemLevels() map[string]string {
	levels := make(map[string]string)

	cfg := GetConfig()

	for k, v := range cfg.Levels.SubsystemLevels {
		levels[k] = v.String()
	}
	return levels
}

// UpdateSubsystemLevels updates just the levels.
func UpdateSubsystemLevels(levels map[string]string) error {
	mu.Lock()
	defer mu.Unlock()

	cfg := GetCopyConfig()

	// Apply updates
	for k, v := range levels {
		cfg.Levels.SubsystemLevels[k] = ParseLevel(v)
	}

	// Update config
	globalConfig.Store(cfg)
	return nil
}

// Legacy Shutdown
type ShutdownFunc func(ctx context.Context) error
