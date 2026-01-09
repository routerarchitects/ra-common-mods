package logger

import (
	"context"
	"log/slog"
)

// This is for future use when opentelemetry gets integrated.

// ContextHandler adds correlation IDs from context to the record.
type ContextHandler struct {
	Next slog.Handler
}

func (h *ContextHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.Next.Enabled(ctx, level)
}

func (h *ContextHandler) Handle(ctx context.Context, r slog.Record) error {
	if corr, ok := getCorrelation(ctx); ok {
		if corr.requestID != "" {
			r.Add(slog.String("request_id", corr.requestID))
		}
		if corr.traceID != "" {
			r.Add(slog.String("trace_id", corr.traceID))
		}
		if corr.spanID != "" {
			r.Add(slog.String("span_id", corr.spanID))
		}
	}

	// Add arbitrary context fields
	if args := getCtxFields(ctx); len(args) > 0 {
		// verify args are valid generic key-values for slog
		r.Add(args...)
	}

	return h.Next.Handle(ctx, r)
}

func (h *ContextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &ContextHandler{Next: h.Next.WithAttrs(attrs)}
}

func (h *ContextHandler) WithGroup(name string) slog.Handler {
	return &ContextHandler{Next: h.Next.WithGroup(name)}
}
