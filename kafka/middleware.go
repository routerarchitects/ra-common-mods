package kafka

import (
	"context"
	"log/slog"
	"time"
)

// LoggingInterceptor creates an interceptor that logs per-attempt message processing.
// Successful attempts are logged at Debug level to avoid info-level flooding under high throughput.
// Only bounded safe failure categories are logged; application error messages and caller logs
// must avoid secrets or sensitive payloads.
func LoggingInterceptor(logger *slog.Logger) Interceptor {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next Handler) Handler {
		return func(ctx context.Context, msg *Message) error {
			start := time.Now()

			logger.Debug("processing message",
				"topic", msg.Topic,
				"partition", msg.Partition,
				"offset", msg.Offset,
			)

			err := next(ctx, msg)
			duration := time.Since(start)

			if err != nil {
				logger.Error("message processing attempt failed",
					"topic", msg.Topic,
					"partition", msg.Partition,
					"offset", msg.Offset,
					"duration", duration,
					"outcome", "failure",
					"failure_category", classifyFailureCategory(err),
				)
			} else {
				logger.Debug("message attempt processed",
					"topic", msg.Topic,
					"partition", msg.Partition,
					"offset", msg.Offset,
					"duration", duration,
					"outcome", "success",
				)
			}

			return err
		}
	}
}

// RecoveryInterceptor creates an interceptor that recovers from panics and converts them to errors.
// Note that Consumer already provides built-in panic containment around handler/interceptor calls.
func RecoveryInterceptor(logger *slog.Logger) Interceptor {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next Handler) Handler {
		return func(ctx context.Context, msg *Message) (err error) {
			defer func() {
				if r := recover(); r != nil {
					panicErr := newHandlerPanicError(r).(*handlerPanicError)
					err = panicErr
					logger.Error("panic recovered",
						"topic", msg.Topic,
						"partition", msg.Partition,
						"offset", msg.Offset,
						"failure_category", "handler_panic",
						"panic_type", panicErr.typeName,
						"panic_frames", panicErr.frames,
					)
				}
			}()

			return next(ctx, msg)
		}
	}
}
