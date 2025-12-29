package logger

import (
	"context"
	"log/slog"
)

// SubsystemHandler filters logs based on the "subsystem" attribute.
type SubsystemHandler struct {
	Next          slog.Handler
	subSystemName string
	defaultLevel  slog.Level
}

func (h *SubsystemHandler) Enabled(ctx context.Context, level slog.Level) bool {
	mu.RLock()
	subLogLevel, ok := globalConfig.Levels.SubsystemLevels[h.subSystemName]
	mu.RUnlock()

	if !ok {
		return level >= h.defaultLevel
	}
	return level >= subLogLevel
}

func (h *SubsystemHandler) Handle(ctx context.Context, r slog.Record) error {
	// subsystem log level handing is already done in Enabled()

	return h.Next.Handle(ctx, r)
}

func (h *SubsystemHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	// Optimization: If we see "subsystem" in attrs, we can return a handler
	// with that specific level cached as its DefaultLevel!

	sub := h.subSystemName
	for _, a := range attrs {
		if a.Key == "subsystem" {
			sub = a.Value.String()
			break
		}
	}

	next := h.Next.WithAttrs(attrs)
	newH := &SubsystemHandler{
		Next:          next,
		subSystemName: sub,
		defaultLevel:  h.defaultLevel,
	}
	return newH
}

func (h *SubsystemHandler) WithGroup(name string) slog.Handler {
	return &SubsystemHandler{
		Next:          h.Next.WithGroup(name),
		subSystemName: h.subSystemName,
		defaultLevel:  h.defaultLevel,
	}
}
