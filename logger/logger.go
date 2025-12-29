package logger

import (
	"context"
	"log/slog"
	"maps"
	"os"
	"strings"
)

// Init initializes the global logger.
// It returns the configured logger (which is also set as default).
func Init(cfg Config) (*slog.Logger, func(), error) {
	// 1. Parsing Levels
	defaultLvl := ParseLevel(cfg.Levels.DefaultLevel)
	subLevels := make(map[string]slog.Level)

	if cfg.Levels.SubsystemLevelsRaw != "" {
		parts := strings.SplitSeq(cfg.Levels.SubsystemLevelsRaw, ",")
		for part := range parts {
			kv := strings.Split(strings.TrimSpace(part), "=")
			if len(kv) == 2 {
				subLevels[kv[0]] = ParseLevel(kv[1])
			}
		}
	}
	cfg.Levels.SubsystemLevels = map[string]slog.Level{}
	// Copy manual map
	maps.Copy(cfg.Levels.SubsystemLevels, subLevels)

	// 2. Redaction
	redactKeys := make(map[string]struct{})
	if cfg.Redaction.Enabled {
		keys := strings.SplitSeq(cfg.Redaction.KeysCSV, ",")
		for k := range keys {
			redactKeys[strings.ToLower(strings.TrimSpace(k))] = struct{}{}
		}
	}

	// 3. Backend (JSON vs Text)
	opts := &slog.HandlerOptions{
		Level:       slog.LevelDebug,
		AddSource:   cfg.Output.AddSource,
		ReplaceAttr: redactionFunction(redactKeys, cfg.Redaction.Replacement),
	}

	var backend slog.Handler
	if strings.EqualFold(cfg.Output.Format, "text") || (cfg.Output.Format == "" && cfg.Environment == "dev") {
		backend = slog.NewTextHandler(os.Stderr, opts)
	} else {
		backend = slog.NewJSONHandler(os.Stderr, opts)
	}

	// Subsystem Handler
	h := &ContextHandler{
		Next: backend,
	}

	// Context Handler
	wrapped := &SubsystemHandler{
		Next:          h,
		defaultLevel:  defaultLvl,
		subSystemName: "main",
	}

	l := slog.New(wrapped)
	l = l.With("service", cfg.ServiceName, "version", cfg.ServiceVersion, "env", cfg.Environment)

	slog.SetDefault(l)
	globalConfig = cfg

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
