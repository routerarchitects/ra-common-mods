package kafka

import (
	"context"
	"time"
)

// Handler processes a message. Return error to Nack, nil to Ack.
type Handler func(ctx context.Context, msg *Message) error

// Interceptor wraps a handler with additional behavior (logging, metrics, tracing, etc.)
type Interceptor func(next Handler) Handler

// Producer defines the interface for publishing messages to Kafka.
type Producer interface {
	// Publish sends a message to the specified topic.
	// Context is used for tracing and timeout.
	Publish(ctx context.Context, topic string, key, value []byte) error

	// PublishWithHeaders sends a message with custom headers.
	PublishWithHeaders(ctx context.Context, topic string, key, value []byte, headers []RecordHeader) error

	// PublishJSON is a convenience method to publish JSON-encoded data.
	PublishJSON(ctx context.Context, topic string, key string, data interface{}) error

	// Close closes the producer and releases resources.
	Close() error
}

// Consumer defines the interface for consuming messages from Kafka.
type Consumer interface {
	// Subscribe to a topic with a handler and worker pool.
	// This method blocks until ctx is cancelled or a fatal error occurs.
	Subscribe(ctx context.Context, topic string, handler Handler, opts *SubscribeOptions) error

	// SubscribeMultiple subscribes to multiple topics with the same handler.
	SubscribeMultiple(ctx context.Context, topics []string, handler Handler, opts *SubscribeOptions) error

	// Close closes the consumer and releases resources.
	Close() error
}

// SubscribeOptions contains options for subscribing to topics.
type SubscribeOptions struct {
	// Number of concurrent workers processing messages
	Workers int

	// Channel buffer size (default: 100)
	BufferSize int

	// AutoCommit interval (0 = manual via handler return only)
	AutoCommit time.Duration

	// Retry policy for failed messages
	RetryPolicy *RetryPolicy

	// Interceptors to run before handler
	Interceptors []Interceptor
}

// RetryPolicy defines the retry behavior for failed messages.
type RetryPolicy struct {
	// MaxRetries is the maximum number of retries (default: 3)
	MaxRetries int

	// InitialDelay is the initial delay before first retry (default: 100ms)
	InitialDelay time.Duration

	// MaxDelay is the maximum delay between retries (default: 30s)
	MaxDelay time.Duration

	// Multiplier is the backoff multiplier (default: 2.0 for exponential backoff)
	Multiplier float64

	// DLQTopic is the dead letter queue topic (optional)
	DLQTopic string

	// DLQProducer is the producer for DLQ (optional)
	DLQProducer Producer
}

// DefaultSubscribeOptions returns sensible defaults for subscription.
func DefaultSubscribeOptions() *SubscribeOptions {
	return &SubscribeOptions{
		Workers:    1,
		BufferSize: 100,
		AutoCommit: 5 * time.Second,
		RetryPolicy: &RetryPolicy{
			MaxRetries:   3,
			InitialDelay: 100 * time.Millisecond,
			MaxDelay:     30 * time.Second,
			Multiplier:   2.0,
		},
		Interceptors: []Interceptor{},
	}
}
