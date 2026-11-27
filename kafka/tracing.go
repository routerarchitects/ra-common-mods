package kafka

import (
	"context"
	"errors"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

const instrumentationScope = "github.com/routerarchitects/ra-common-mods/kafka"

// TracingConfig configures opt-in OpenTelemetry instrumentation using caller-owned providers.
// Leaving TracerProvider nil disables tracing with zero overhead and no global state side effects.
type TracingConfig struct {
	TracerProvider trace.TracerProvider
	Propagator     propagation.TextMapPropagator
}

// WithProducerTracing enables opt-in OpenTelemetry tracing on a Producer.
func WithProducerTracing(cfg TracingConfig) ProducerOption {
	return func(o *producerOptions) {
		o.tracing = newKafkaTracer(cfg)
	}
}

// WithConsumerTracing enables opt-in OpenTelemetry tracing on a Consumer.
func WithConsumerTracing(cfg TracingConfig) ConsumerOption {
	return func(o *consumerOptions) {
		o.tracing = newKafkaTracer(cfg)
	}
}

// NewTracingInterceptor returns an Interceptor that extracts trace context from message headers
// and records a span per invocation using the supplied TracingConfig.
// When core consumer tracing is enabled via WithConsumerTracing, do not also attach
// NewTracingInterceptor for the same consumer to avoid duplicate spans.
func NewTracingInterceptor(cfg TracingConfig) Interceptor {
	kt := newKafkaTracer(cfg)
	if kt == nil {
		return func(next Handler) Handler { return next }
	}
	return func(next Handler) Handler {
		return func(ctx context.Context, msg *Message) error {
			spanCtx, endSpan := kt.startConsumeSpan(ctx, "", msg, 0)
			var err error
			panicked := true
			defer func() {
				if panicked {
					endSpan(&handlerPanicError{})
				}
			}()
			err = next(spanCtx, msg)
			panicked = false
			endSpan(err)
			return err
		}
	}
}

type kafkaTracer struct {
	tracer     trace.Tracer
	propagator propagation.TextMapPropagator
}

func newKafkaTracer(cfg TracingConfig) *kafkaTracer {
	if isNilInterface(cfg.TracerProvider) {
		return nil
	}
	prop := cfg.Propagator
	if isNilInterface(prop) {
		prop = propagation.TraceContext{}
	}
	return &kafkaTracer{
		tracer:     cfg.TracerProvider.Tracer(instrumentationScope),
		propagator: prop,
	}
}

func (kt *kafkaTracer) startPublishSpan(ctx context.Context, topic string, headers []RecordHeader) (context.Context, []RecordHeader, func(error)) {
	if kt == nil {
		return ctx, headers, nil
	}
	spanCtx, span := kt.tracer.Start(
		ctx,
		"kafka.publish",
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(
			attribute.String("messaging.system", "kafka"),
			attribute.String("messaging.destination.name", topic),
		),
	)

	copied := cloneHeaders(headers)
	carrier := &recordHeaderCarrier{headers: &copied}
	kt.propagator.Inject(spanCtx, carrier)

	endFn := func(err error) {
		if err != nil {
			category := "publish_" + classifyPublishError(err)
			span.SetAttributes(attribute.String("error.type", category))
			span.RecordError(errors.New(category))
			span.SetStatus(codes.Error, category)
		}
		span.End()
	}
	return spanCtx, copied, endFn
}

func (kt *kafkaTracer) startConsumeSpan(ctx context.Context, groupID string, msg *Message, attempt int) (context.Context, func(error)) {
	if kt == nil {
		return ctx, nil
	}
	// Extract parent trace context into ctx while preserving deadlines and cancellation on ctx.
	extractedCtx := kt.propagator.Extract(ctx, &recordHeaderCarrier{headers: &msg.Headers})

	attrs := []attribute.KeyValue{
		attribute.String("messaging.system", "kafka"),
		attribute.String("messaging.destination.name", msg.Topic),
		attribute.Int64("messaging.kafka.partition", int64(msg.Partition)),
		attribute.Int64("messaging.kafka.message.offset", msg.Offset),
	}
	if groupID != "" {
		attrs = append(attrs, attribute.String("messaging.kafka.consumer.group", groupID))
	}
	if attempt > 0 {
		attrs = append(attrs, attribute.Int("messaging.kafka.attempt", attempt))
	}

	spanCtx, span := kt.tracer.Start(
		extractedCtx,
		"kafka.consume",
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(attrs...),
	)

	endFn := func(err error) {
		if err != nil {
			category := classifyFailureCategory(err)
			span.SetAttributes(attribute.String("error.type", category))
			span.RecordError(errors.New(category))
			span.SetStatus(codes.Error, category)
		}
		span.End()
	}
	return spanCtx, endFn
}

type recordHeaderCarrier struct {
	headers *[]RecordHeader
}

func (c *recordHeaderCarrier) Get(key string) string {
	if c == nil || c.headers == nil {
		return ""
	}
	for _, h := range *c.headers {
		if strings.EqualFold(h.Key, key) {
			return string(h.Value)
		}
	}
	return ""
}

func (c *recordHeaderCarrier) Set(key, value string) {
	if c == nil || c.headers == nil {
		return
	}
	for i := range *c.headers {
		if strings.EqualFold((*c.headers)[i].Key, key) {
			(*c.headers)[i].Key = key
			(*c.headers)[i].Value = []byte(value)
			return
		}
	}
	*c.headers = append(*c.headers, RecordHeader{
		Key:   key,
		Value: []byte(value),
	})
}

func (c *recordHeaderCarrier) Keys() []string {
	if c == nil || c.headers == nil {
		return nil
	}
	keys := make([]string, len(*c.headers))
	for i, h := range *c.headers {
		keys[i] = h.Key
	}
	return keys
}
