Design Document: Common Kafka Module
Goal Description
Create a standard, robust, and opinionated Kafka module for Go microservices. This module will abstract the underlying Kafka driver to provide a consistent API for producing and consuming messages, ensuring best practices for error handling, observability (metrics/tracing), and configuration are applied uniformly across all services.

User Review Required
IMPORTANT

Driver Selection: This design proposes using IBM/sarama as the underlying driver. It is a pure Go implementation, avoiding CGO dependencies (easier for Alpine builds), and is widely used and robust. Opinionated Defaults: The module will enforce specific patterns for:

context.Context usage for cancellation and tracing.
slog for logging.
OpenTelemetry for tracing.
Prometheus for metrics.
Proposed Architecture
1. Configuration
Configuration will be driven by a struct that can be loaded from env vars or config files.

type Config struct {
    Brokers       []string `json:"brokers" yaml:"brokers"`
    SchemaRegistry URL     `json:"schema_registry,omitempty"` // Optional
    ClientID      string   `json:"client_id"`
    // Auth configuration (SASL/TLS)
    Auth          AuthConfig `json:"auth"` 
    // Consumer specific
    Consumer      ConsumerConfig `json:"consumer"`
    // Producer specific
    Producer      ProducerConfig `json:"producer"`
}
2. Producer Interface
The producer will support synchronous and asynchronous publishing (optional, default sync for safety/simplicity in "opinionated" design?). "Opinionated" usually favors safety -> Sync by default, or Async with robust error channel. Let's propose a simple synchronous-looking API that handles batching internally if possible, or just standard Sync.

type Producer interface {
    // Publish sends a message to the specified topic.
    // Context is used for tracing and timeout.
    Publish(ctx context.Context, topic string, key, value []byte) error
    
    // PublishJSON convenience method
    PublishJSON(ctx context.Context, topic string, key string, data interface{}) error
    
    Close() error
}
3. Consumer Interface
The consumer uses a Subscribe model with built-in worker pool management. This provides a cleaner API while maintaining flexibility.

type Message struct {
    Key, Value []byte
    Topic      string
    Partition  int32
    Offset     int64
    Headers    []RecordHeader
    Timestamp  time.Time
}
// Handler processes a message. Return error to Nack, nil to Ack.
type Handler func(ctx context.Context, msg *Message) error
type SubscribeOptions struct {
    // Number of concurrent workers processing messages
    Workers       int
    
    // Channel buffer size (default: 100)
    BufferSize    int
    
    // Auto-commit interval (0 = manual via Ack/Nack only)
    AutoCommit    time.Duration
    
    // Retry policy for failed messages
    RetryPolicy   *RetryPolicy
    
    // Interceptors to run before handler
    Interceptors  []Interceptor
}
type Consumer interface {
    // Subscribe to a topic with a handler and worker pool.
    // This method blocks until ctx is cancelled or a fatal error occurs.
    Subscribe(ctx context.Context, topic string, handler Handler, opts *SubscribeOptions) error
    
    // SubscribeMultiple subscribes to multiple topics with the same handler.
    SubscribeMultiple(ctx context.Context, topics []string, handler Handler, opts *SubscribeOptions) error
    
    Close() error
}
Key Benefits:

Built-in Worker Pool: No need to manually spawn goroutines - specify Workers: 10 and the library handles it.
Automatic Offset Management: The library tracks which offsets can be safely committed based on handler results.
Per-Partition Ordering: Workers are assigned per-partition to maintain Kafka's ordering guarantees.
Backpressure: Internal buffered channels prevent overwhelming the workers.
4. Middleware / Interceptors
Interceptors wrap the handler execution, allowing for cross-cutting concerns.

// Interceptor wraps a handler with additional behavior (logging, metrics, tracing, etc.)
type Interceptor func(next Handler) Handler
// Built-in interceptors:
// - TracingInterceptor: Extract/inject trace context from headers
// - LoggingInterceptor: Log message receipt and processing duration
// - MetricsInterceptor: Track processed/failed message counts
// - RecoveryInterceptor: Recover from panics and Nack the message
Example usage:

opts := &kafka.SubscribeOptions{
    Workers: 5,
    Interceptors: []kafka.Interceptor{
        kafka.TracingInterceptor(),
        kafka.LoggingInterceptor(logger),
        kafka.MetricsInterceptor(metrics),
        kafka.RecoveryInterceptor(),
    },
}
5. Resiliency (Opinionated)
type RetryPolicy struct {
    MaxRetries    int           // Default: 3
    InitialDelay  time.Duration // Default: 100ms
    MaxDelay      time.Duration // Default: 30s
    Multiplier    float64       // Default: 2.0 (exponential backoff)
    
    // DLQ configuration
    DLQTopic      string        // If set, send failed messages here after exhausting retries
    DLQProducer   Producer      // Producer instance for DLQ
}
Behavior:

If handler returns an error, the message is retried with exponential backoff
Retries happen in-memory without re-consuming from Kafka (to maintain offset order)
After MaxRetries, the message is either sent to DLQ or offset is committed with error logged
Per-partition ordering is maintained during retries
Proposed Changes
Structure of src/ra-common-mods/kafka:

[NEW] 
config.go
Configuration structs and defaults.
[NEW] 
producer.go
NewProducer(cfg Config) (Producer, error) implementation using Sarama.
[NEW] 
consumer.go
NewConsumer(cfg Config) (Consumer, error) implementation using Sarama ConsumerGroup.
[NEW] 
message.go
Wrapper struct for Kafka messages (Metadata, Headers, Value).
[NEW] 
middleware.go
Standard interceptors.
Verification Plan
Automated Tests
Unit Tests: Mock the underlying Sarama interfaces to test the wrapper logic (retries, middleware execution).
Integration Tests: Spin up a Kafka container (via Docker) and run real produce/consume cycles.
Note: Requires docker available in the environment.
Manual Verification
Create a cmd/example within the module or a separate test file that connects to a local Kafka (if available) and exchanges messages.