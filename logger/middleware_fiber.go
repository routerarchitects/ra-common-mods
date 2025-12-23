package logger

import (
	"context"

	"github.com/gofiber/fiber/v3"
)

// FiberMiddleware injects correlation IDs into Fiber's UserContext.
func FiberMiddleware() fiber.Handler {
	return func(c fiber.Ctx) error {
		// Access global config for settings
		cfg := globalConfig.Correlation
		if !cfg.Enabled {
			return c.Next()
		}

		reqID := c.Get(cfg.RequestIDHeader)
		traceID := c.Get(cfg.TraceIDHeader)
		spanID := c.Get(cfg.SpanIDHeader)

		if reqID == "" && cfg.GenerateRequestID {
			reqID = newRequestID()
		}

		uctx := c.Context()
		if uctx == nil {
			uctx = context.Background()
		}

		uctx = withCorrelation(uctx, correlation{
			requestID: reqID,
			traceID:   traceID,
			spanID:    spanID,
		})

		c.SetContext(uctx)

		if cfg.Propagate {
			if reqID != "" {
				c.Set(cfg.RequestIDHeader, reqID)
			}
			if traceID != "" {
				c.Set(cfg.TraceIDHeader, traceID)
			}
			if spanID != "" {
				c.Set(cfg.SpanIDHeader, spanID)
			}
		}

		return c.Next()
	}
}
