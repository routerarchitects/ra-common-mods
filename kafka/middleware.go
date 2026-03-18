package kafka

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// LoggingInterceptor creates an interceptor that logs message processing.
func LoggingInterceptor(logger *slog.Logger) Interceptor {
	return func(next Handler) Handler {
		return func(ctx context.Context, msg *Message) error {
			start := time.Now()

			logger.Info("processing message",
				"topic", msg.Topic,
				"partition", msg.Partition,
				"offset", msg.Offset,
			)

			err := next(ctx, msg)
			duration := time.Since(start)

			if err != nil {
				logger.Error("message processing failed",
					"topic", msg.Topic,
					"partition", msg.Partition,
					"offset", msg.Offset,
					"duration", duration,
					"error", err,
				)
			} else {
				logger.Info("message processed successfully",
					"topic", msg.Topic,
					"partition", msg.Partition,
					"offset", msg.Offset,
					"duration", duration,
				)
			}

			return err
		}
	}
}

// RecoveryInterceptor creates an interceptor that recovers from panics.
func RecoveryInterceptor(logger *slog.Logger) Interceptor {
	return func(next Handler) Handler {
		return func(ctx context.Context, msg *Message) (err error) {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("panic recovered: %v", r)
					logger.Error("panic recovered",
						"topic", msg.Topic,
						"partition", msg.Partition,
						"offset", msg.Offset,
						"panic", r,
					)
				}
			}()

			return next(ctx, msg)
		}
	}
}

// MetricsInterceptor creates an interceptor that tracks metrics.
// This is a simple example - in production you'd integrate with Prometheus.
func MetricsInterceptor() Interceptor {
	return func(next Handler) Handler {
		return func(ctx context.Context, msg *Message) error {
			start := time.Now()

			err := next(ctx, msg)
			duration := time.Since(start)

			// TODO: Integrate with Prometheus metrics
			// For now, we just track duration
			_ = duration

			// Example metrics you might track:
			// - messagesProcessed.Inc()
			// - messageProcessingDuration.Observe(duration.Seconds())
			// - messagesFailedTotal.Inc() (if err != nil)

			return err
		}
	}
}

// TracingInterceptor creates an interceptor that extracts/injects trace context.
// This is a placeholder - in production you'd integrate with OpenTelemetry.
func TracingInterceptor() Interceptor {
	return func(next Handler) Handler {
		return func(ctx context.Context, msg *Message) error {
			// TODO: Integrate with OpenTelemetry
			// Extract trace context from message headers
			// span := tracer.Start(ctx, "kafka.consume")
			// defer span.End()

			// For now, we just pass through
			return next(ctx, msg)
		}
	}
}
