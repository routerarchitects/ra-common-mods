package logger

import (
	"context"
)

type ctxFieldsKey struct{}

// withCtxFields appends args to the context for logging.
// args should be key-value pairs (slog style).
func withCtxFields(ctx context.Context, args []any) context.Context {
	// Simple slice append for now.
	existing, _ := ctx.Value(ctxFieldsKey{}).([]any)
	newArgs := make([]any, 0, len(existing)+len(args))
	newArgs = append(newArgs, existing...)
	newArgs = append(newArgs, args...)
	return context.WithValue(ctx, ctxFieldsKey{}, newArgs)
}

func getCtxFields(ctx context.Context) []any {
	v, _ := ctx.Value(ctxFieldsKey{}).([]any)
	return v
}
