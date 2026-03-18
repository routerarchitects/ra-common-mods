package logger

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"sync"
	"sync/atomic"
)

var (
	globalConfig atomic.Value
	mu           sync.Mutex
)

func getConfig() Config {
	val := globalConfig.Load()
	if val == nil {
		return Config{}
	}
	return val.(Config)
}

// Subsystem returns a logger with the subsystem attribute pre-bound.
// This is a helper for services.
func Subsystem(name string) *slog.Logger {
	mu.Lock()
	defer mu.Unlock()

	// Re-read config under lock in case it changed
	cfg := getConfig()
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
	cfg := getConfig()

	// Copy-On-Write: Create new map
	newLevels := make(map[string]slog.Level)
	if cfg.Levels.SubsystemLevels != nil {
		maps.Copy(newLevels, cfg.Levels.SubsystemLevels)
	}
	cfg.Levels.SubsystemLevels = newLevels
	return cfg
}

// GetSubsystemLevels returns a copy of current levels.
func GetSubsystemLevels() map[string]string {
	levels := make(map[string]string)

	cfg := getConfig()

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
		level, err := ParseLevelChecked(v)
		if err != nil {
			return fmt.Errorf("invalid log level for subsystem %q: %w", k, err)
		}
		cfg.Levels.SubsystemLevels[k] = level
	}

	// Update config
	globalConfig.Store(cfg)
	return nil
}

// Legacy Shutdown
type ShutdownFunc func(ctx context.Context) error
