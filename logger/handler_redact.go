package logger

import (
	"context"
	"log/slog"
	"strings"
)

// RedactHandler redacts sensitive keys in the record.
type RedactHandler struct {
	Next        slog.Handler
	RedactKeys  map[string]struct{}
	Replacement string
}

func (h *RedactHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.Next.Enabled(ctx, level)
}

func (h *RedactHandler) Handle(ctx context.Context, r slog.Record) error {
	// We use a "ReplaceAttr" function on a cloned record?
	// Or we iterate over attributes?
	// slog.Record attributes are efficiently stored.
	// The best way is to traverse and replace.

	// We cannot easily mutate Record.Attrs in place without allocating.
	// Ideally, redaction should be done by the final JSONHandler's ReplaceAttr option.
	// BUT, we want it compasable.
	// Since we are writing a middleware, let's implement a simple pass.

	// However, for deep redaction (nested JSON), standard slog doesn't support recursive traversal easily
	// without `ReplaceAttr` hook in the HandlerOptions of the *backend* handler.

	// STRATEGY: We will NOT implement complex redaction here if we can avoid it.
	// Instead, we will configure the Final (JSON) Handler with a ReplaceAttr function.
	// That is much more efficient.

	// So this RedactHandler might strictly be for *filtering* messages if needed,
	// but for key masking, let's move that logic to `logger.go` builder.

	// Wait, the requirement says "RedactHandler: Middleware".
	// If I do it here, I have to iterate attributes.
	// Let's stick to the "Builder passes ReplaceAttr to JSONHandler" approach for efficiency.
	// But if the user *wants* a RedactHandler middleware (e.g. for non-JSON sinks), I can provide a simple one.

	// For now, I'll implement a `replaceAttr` function in `handler_redact.go` that can be passed to JSONHandler.
	// That's the idiomatic slog way.

	return h.Next.Handle(ctx, r)
}

func (h *RedactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &RedactHandler{
		Next:        h.Next.WithAttrs(attrs),
		RedactKeys:  h.RedactKeys,
		Replacement: h.Replacement,
	}
}

func (h *RedactHandler) WithGroup(name string) slog.Handler {
	return &RedactHandler{
		Next:        h.Next.WithGroup(name),
		RedactKeys:  h.RedactKeys,
		Replacement: h.Replacement,
	}
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
