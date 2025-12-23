package logger

import "context"

type correlation struct {
	requestID string
	traceID   string
	spanID    string
}

type correlationKey struct{}

func withCorrelation(ctx context.Context, c correlation) context.Context {
	return context.WithValue(ctx, correlationKey{}, c)
}

func getCorrelation(ctx context.Context) (correlation, bool) {
	v := ctx.Value(correlationKey{})
	if v == nil {
		return correlation{}, false
	}
	c, ok := v.(correlation)
	return c, ok
}
