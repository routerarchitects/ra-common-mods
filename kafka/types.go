package kafka

import (
	"context"
	"time"
)

// Handler processes a message.
// Return nil on durable success.
// Return error on failure so retry policy/logging can handle it.
// Source message Key, Value, Headers, and metadata (Topic, Partition, Offset, Timestamp)
// must be treated as read-only by handlers and interceptors.
// Application error messages and caller-side logs should avoid embedding secrets or sensitive payloads.
// Handlers registered with Consumer may be invoked concurrently across different partitions
// and must be safe for concurrent use and idempotent across replays.
type Handler func(ctx context.Context, msg *Message) error

// Interceptor wraps a handler with additional behavior (logging, recovery, tracing, etc.).
type Interceptor func(next Handler) Handler

// Producer defines the interface for publishing messages to Kafka.
type Producer interface {
	// Publish sends a message to the specified topic.
	// Context is checked before publishing; in-flight synchronous Sarama calls cannot be aborted mid-send.
	Publish(ctx context.Context, topic string, key, value []byte) error

	// PublishWithHeaders sends a message with custom headers.
	PublishWithHeaders(ctx context.Context, topic string, key, value []byte, headers []RecordHeader) error

	// PublishJSON is a convenience method to publish JSON-encoded data.
	PublishJSON(ctx context.Context, topic string, key string, data interface{}) error

	// Close closes the producer and releases resources.
	Close() error
}

// ConsumerState represents the lifecycle state of a Consumer.
type ConsumerState string

const (
	ConsumerStateRegistered  ConsumerState = "registered"
	ConsumerStateStarting    ConsumerState = "starting"
	ConsumerStateRunning     ConsumerState = "running"
	ConsumerStateRebalancing ConsumerState = "rebalancing"
	ConsumerStateDraining    ConsumerState = "draining"
	ConsumerStateStopped     ConsumerState = "stopped"
	ConsumerStateFailed      ConsumerState = "failed"
	ConsumerStateClosed      ConsumerState = "closed"
)

// ConsumerHealth provides a concurrency-safe snapshot of consumer group lifecycle and readiness.
type ConsumerHealth struct {
	State            ConsumerState
	Ready            bool
	AssignedClaims   int
	LastError        string
	LastErrorAt      time.Time
	LastSessionAt    time.Time
	LastResolvedAt   time.Time
	LastSuccessfulAt time.Time
}

// Subscription defines a topic subscription and its independent handler and retry policy.
type Subscription struct {
	Topic          string
	Handler        Handler
	HandlerTimeout time.Duration // 0 means inherit ConsumerConfig.HandlerTimeout
	RetryPolicy    *RetryPolicy  // required; use DefaultRetryPolicy() for bounded retries followed by discard
	Interceptors   []Interceptor
}

// Consumer defines the lifecycle and topic-registration interface for a single consumer group member.
type Consumer interface {
	Subscribe(subscription Subscription) error
	Run(ctx context.Context) error
	Health() ConsumerHealth
	Close() error
}

// RetryStrategy defines how to handle messages that fail processing after max retries.
type RetryStrategy string

const (
	// RetryStrategyInfinite retries indefinitely until success or session cancellation (blocks partition).
	RetryStrategyInfinite RetryStrategy = "infinite"

	// RetryStrategyDLQ retries up to MaxRetries, then sends failed messages to a Dead Letter Queue.
	RetryStrategyDLQ RetryStrategy = "dlq"

	// RetryStrategyLogAndIgnore retries up to MaxRetries, then logs, records discard, and marks the offset.
	RetryStrategyLogAndIgnore RetryStrategy = "ignore"

	// RetryStrategyStop retries up to MaxRetries, then stops the consumer with a terminal error without marking the offset.
	RetryStrategyStop RetryStrategy = "stop"
)

// DLQFailurePolicy determines what happens if publishing to the DLQ fails after handler retries are exhausted.
type DLQFailurePolicy string

const (
	// DLQFailureDiscard logs and discards the message and marks the source offset if the session is active.
	DLQFailureDiscard DLQFailurePolicy = "discard"

	// DLQFailureRetry retries transient DLQ failures with backoff and stops the consumer on known permanent failures.
	DLQFailureRetry DLQFailurePolicy = "retry"

	// DLQFailureStop leaves the source offset unmarked and stops the consumer group with a terminal error.
	DLQFailureStop DLQFailurePolicy = "stop"
)

// RetryPolicy defines the retry behavior for failed messages.
type RetryPolicy struct {
	// Strategy determines behavior after MaxRetries (required: ignore, dlq, infinite, or stop)
	Strategy RetryStrategy

	// MaxRetries is the maximum number of retries (default: 3). Ignored if Strategy is Infinite.
	MaxRetries int

	// InitialDelay is the initial delay before first retry (default: 100ms)
	InitialDelay time.Duration

	// MaxDelay is the maximum delay between retries (default: 30s)
	MaxDelay time.Duration

	// Multiplier is the backoff multiplier (default: 2.0 for exponential backoff)
	Multiplier float64

	// DLQTopic is the dead letter queue topic (required for RetryStrategyDLQ)
	DLQTopic string

	// DLQProducer is the producer for DLQ (required for RetryStrategyDLQ)
	DLQProducer Producer

	// DLQFailurePolicy determines behavior when DLQ publication fails (required for new DLQ subscriptions).
	DLQFailurePolicy DLQFailurePolicy
}

// DefaultRetryPolicy returns a fresh copy of the default bounded retry policy.
func DefaultRetryPolicy() *RetryPolicy {
	return &RetryPolicy{
		Strategy:     RetryStrategyLogAndIgnore,
		MaxRetries:   3,
		InitialDelay: 100 * time.Millisecond,
		MaxDelay:     30 * time.Second,
		Multiplier:   2.0,
	}
}

// ProducerOption configures optional producer behavior such as instrumentation.
type ProducerOption func(*producerOptions)

type producerOptions struct {
	syncProducerFactory func(addrs []string, config *saramaConfigAlias) (syncProducerClient, error)
	metrics             *Metrics
	tracing             *kafkaTracer
}

// ConsumerOption configures optional consumer behavior such as instrumentation.
type ConsumerOption func(*consumerOptions)

type consumerOptions struct {
	consumerGroupFactory func(addrs []string, groupID string, config *saramaConfigAlias) (consumerGroupClient, error)
	metrics              *Metrics
	tracing              *kafkaTracer
	sleepFn              func(ctx context.Context, d time.Duration) error
	jitterFn             func(max time.Duration) time.Duration
}
