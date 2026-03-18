package logger

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"strings"
)

// Init initializes the global logger.
// It returns the configured logger (which is also set as default).
func Init(cfg Config) (*slog.Logger, func(), error) {
	// 1. Parsing Levels
	defaultLvl, err := ParseLevelChecked(cfg.Levels.DefaultLevel)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid default log level: %w", err)
	}
	subLevels := make(map[string]slog.Level)

	if cfg.Levels.SubsystemLevelsRaw != "" {
		parts := strings.SplitSeq(cfg.Levels.SubsystemLevelsRaw, ",")
		for part := range parts {
			kv := strings.Split(strings.TrimSpace(part), "=")
			if len(kv) == 2 {
				subsystem := strings.TrimSpace(kv[0])
				if subsystem == "" {
					continue
				}
				level, levelErr := ParseLevelChecked(kv[1])
				if levelErr != nil {
					return nil, nil, fmt.Errorf("invalid log level for subsystem %q: %w", subsystem, levelErr)
				}
				subLevels[subsystem] = level
			}
		}
	}
	cfg.Levels.SubsystemLevels = map[string]slog.Level{}
	// Copy manual map
	maps.Copy(cfg.Levels.SubsystemLevels, subLevels)

	// 2. Redaction
	var redactFunc func([]string, slog.Attr) slog.Attr
	redactKeys := make(map[string]struct{})
	if cfg.Redaction.Enabled {
		keys := strings.SplitSeq(cfg.Redaction.KeysCSV, ",")
		for k := range keys {
			redactKeys[strings.ToLower(strings.TrimSpace(k))] = struct{}{}
		}
		redactFunc = redactionFunction(redactKeys, cfg.Redaction.Replacement)
	}

	// 3. Backend (JSON vs Text)
	opts := &slog.HandlerOptions{
		Level:       slog.LevelDebug,
		AddSource:   cfg.Output.AddSource,
		ReplaceAttr: redactFunc,
	}

	var backend slog.Handler
	if strings.EqualFold(cfg.Output.Format, "text") || (cfg.Output.Format == "" && cfg.Environment == "dev") {
		backend = slog.NewTextHandler(os.Stderr, opts)
	} else {
		backend = slog.NewJSONHandler(os.Stderr, opts)
	}

	// Context Handle
	h := &ContextHandler{
		Next: backend,
	}

	// Subsystem Handler
	wrapped := &SubsystemHandler{
		Next:          h,
		defaultLevel:  defaultLvl,
		subSystemName: "main",
	}

	l := slog.New(wrapped)
	l = l.With("service", cfg.ServiceName, "version", cfg.ServiceVersion, "env", cfg.Environment)

	slog.SetDefault(l)
	globalConfig.Store(cfg)

	return l, func() {}, nil
}

// L returns the global default logger.
func L() *slog.Logger {
	return slog.Default()
}

// With returns a context with fields bound.
func With(ctx context.Context, args ...any) context.Context {
	return withCtxFields(ctx, args)
}

// redactionFunction returns a ReplaceAttr function for slog.HandlerOptions
func redactionFunction(keys map[string]struct{}, replacement string) func([]string, slog.Attr) slog.Attr {
	return func(groups []string, a slog.Attr) slog.Attr {
		// keys checks
		// We only check the key name.
		if _, ok := keys[strings.ToLower(a.Key)]; ok {
			return slog.String(a.Key, replacement)
		}
		return a
	}
}
