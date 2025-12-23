package logger

import (
	"context"
	"log/slog"
)

// SubsystemHandler filters logs based on the "subsystem" attribute.
type SubsystemHandler struct {
	Next slog.Handler

	// DefaultLevel is the level to use if no subsystem matches.
	DefaultLevel slog.Level

	// SubsystemLevels maps subsystem names to minimum enabled levels.
	SubsystemLevels map[string]slog.Level
}

func (h *SubsystemHandler) Enabled(ctx context.Context, level slog.Level) bool {
	// We cannot check subsystem here easily because it's in the Record, which we don't have yet.
	// slog.Handler.Enabled() receives Context but not Record/Attrs.
	// Unlike Zap, slog checks Enabled() *before* creating the Record.

	// Issue: We don't know the subsystem yet!
	// Solution: The user uses `logger.With("subsystem", "db").Info(...)`.
	// The `With` call creates a logger with that attribute.
	// BUT `Enabled(ctx, level)` is called on the *Derived* logger.

	// Does Enabled() have access to the pre-bound attributes?
	// No, standard Handler interface doesn't expose them in `Enabled`.
	// However, when `WithAttrs` is called, we return a NEW Handler.
	// We can inspect the attributes *there* and bake the level into the new handler!

	// This is the clever bit.

	return level >= h.DefaultLevel && h.Next.Enabled(ctx, level)
}

func (h *SubsystemHandler) Handle(ctx context.Context, r slog.Record) error {
	// Double check checks?
	// In Handle, we have the Record. We can look for "subsystem" attr.
	// But Handle is called after Enabled.
	// If Enabled returned true (using DefaultLevel), but subsystem level is higher (e.g. Default=Info, DB=Error),
	// we effectively filter it here.

	effective := h.DefaultLevel

	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "subsystem" {
			if lvl, ok := h.SubsystemLevels[a.Value.String()]; ok {
				effective = lvl
			}
			return false // Stop iteration
		}
		return true
	})

	if r.Level < effective {
		return nil // Drop it
	}

	return h.Next.Handle(ctx, r)
}

func (h *SubsystemHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	// Optimization: If we see "subsystem" in attrs, we can return a handler
	// with that specific level cached as its DefaultLevel!

	sub := ""
	for _, a := range attrs {
		if a.Key == "subsystem" {
			sub = a.Value.String()
			break
		}
	}

	next := h.Next.WithAttrs(attrs)
	newH := &SubsystemHandler{
		Next:            next,
		DefaultLevel:    h.DefaultLevel,
		SubsystemLevels: h.SubsystemLevels,
	}

	if sub != "" {
		if lvl, ok := h.SubsystemLevels[sub]; ok {
			newH.DefaultLevel = lvl
		}
	}

	return newH
}

func (h *SubsystemHandler) WithGroup(name string) slog.Handler {
	return &SubsystemHandler{
		Next:            h.Next.WithGroup(name),
		DefaultLevel:    h.DefaultLevel,
		SubsystemLevels: h.SubsystemLevels,
	}
}
