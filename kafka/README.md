# Kafka Module

Common Golang Kafka module providing opininated producer and consumer functionality for microservices.

## Features

- **Producer**: Synchronous message publishing with SASL/TLS support
- **Consumer**: Built-in worker pool management with automatic offset tracking
- **Retry Logic**: Exponential backoff with configurable DLQ
- **Middleware/Interceptors**: Logging, recovery, metrics, and tracing
- **SASL Authentication**: PLAIN, SCRAM-SHA-256, SCRAM-SHA-512
- **TLS Support**: Client certificates and CA verification

## Installation

```bash
go get github.com/routerarchitects/ra-common-mods/kafka
```

## Quick Start

### Producer

```go
package main

import (
    "context"
    "log"
    
    "github.com/routerarchitects/ra-common-mods/kafka"
)

func main() {
    cfg := kafka.DefaultConfig()
    cfg.Brokers = []string{"localhost:9092"}
    
    producer, err := kafka.NewProducer(cfg)
    if err != nil {
        log.Fatal(err)
    }
    defer producer.Close()
    
    // Publish a message
    err = producer.Publish(
        context.Background(),
        "my-topic",
        []byte("key"),
        []byte("value"),
    )
    if err != nil {
        log.Fatal(err)
    }
    
    // Or publish JSON
    err = producer.PublishJSON(
        context.Background(),
        "my-topic",
        "user-123",
        map[string]interface{}{
            "name": "John Doe",
            "age": 30,
        },
    )
}
```

### Consumer

```go
package main

import (
    "context"
    "log"
    "log/slog"
    "os"
    "os/signal"
    "syscall"
    
    "github.com/routerarchitects/ra-common-mods/kafka"
)

func main() {
    cfg := kafka.DefaultConfig()
    cfg.Brokers = []string{"localhost:9092"}
    cfg.Consumer.GroupID = "my-consumer-group"
    
    logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
    
    consumer, err := kafka.NewConsumer(cfg, logger)
    if err != nil {
        log.Fatal(err)
    }
    defer consumer.Close()
    
    // Create context with cancellation
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()
    
    // Handle shutdown gracefully
    sigterm := make(chan os.Signal, 1)
    signal.Notify(sigterm, syscall.SIGINT, syscall.SIGTERM)
    go func() {
        <-sigterm
        cancel()
    }()
    
    // Define message handler
    handler := func(ctx context.Context, msg *kafka.Message) error {
        logger.Info("received message",
            "topic", msg.Topic,
            "key", string(msg.Key),
            "value", string(msg.Value),
        )
        // Process message...
        return nil
    }
    
    // Subscribe with options
    opts := &kafka.SubscribeOptions{
        Workers: 5, // Process messages concurrently with 5 workers
        BufferSize: 100,
        Interceptors: []kafka.Interceptor{
            kafka.LoggingInterceptor(logger),
            kafka.RecoveryInterceptor(logger),
            kafka.MetricsInterceptor(),
        },
        RetryPolicy: &kafka.RetryPolicy{
            MaxRetries: 3,
            InitialDelay: 100 * time.Millisecond,
            MaxDelay: 30 * time.Second,
            Multiplier: 2.0,
            DLQTopic: "my-topic-dlq",
        },
    }
    
    // Subscribe (blocking)
    err = consumer.Subscribe(ctx, "my-topic", handler, opts)
    if err != nil && err != context.Canceled {
        log.Fatal(err)
    }
}
```

## Configuration

### Environment-Based Configuration

You can load configuration from environment variables or config files:

```go
cfg := kafka.Config{
    Brokers: []string{os.Getenv("KAFKA_BROKERS")},
    ClientID: os.Getenv("KAFKA_CLIENT_ID"),
    Auth: kafka.AuthConfig{
        SASL: kafka.SASLConfig{
            Enabled: os.Getenv("KAFKA_SASL_ENABLED") == "true",
            Mechanism: os.Getenv("KAFKA_SASL_MECHANISM"), // PLAIN, SCRAM-SHA-256, SCRAM-SHA-512
            Username: os.Getenv("KAFKA_SASL_USERNAME"),
            Password: os.Getenv("KAFKA_SASL_PASSWORD"),
        },
        TLS: kafka.TLSConfig{
            Enabled: os.Getenv("KAFKA_TLS_ENABLED") == "true",
            CertFile: os.Getenv("KAFKA_TLS_CERT_FILE"),
            KeyFile: os.Getenv("KAFKA_TLS_KEY_FILE"),
            CAFile: os.Getenv("KAFKA_TLS_CA_FILE"),
        },
    },
    Consumer: kafka.ConsumerConfig{
        GroupID: os.Getenv("KAFKA_CONSUMER_GROUP_ID"),
        InitialOffset: "newest", // or "oldest"
    },
}
```

## Worker Pool Design

The consumer uses a built-in worker pool to process messages concurrently:

- **Per-Partition Workers**: Workers are assigned per partition to maintain Kafka's ordering guarantees
- **Automatic Offset Management**: Offsets are committed only after successful processing
- **Backpressure**: Buffered channels prevent overwhelming workers

## Retry & DLQ

Failed messages are automatically retried with exponential backoff:

1. Message fails → Retry with `InitialDelay`
2. Exponential backoff → Multiply delay by `Multiplier` 
3. Max retries exhausted → Send to DLQ topic (if configured)

## Middleware/Interceptors

Built-in interceptors for cross-cutting concerns:

- **LoggingInterceptor**: Logs message processing with duration
- **RecoveryInterceptor**: Recovers from panics
- **MetricsInterceptor**: Tracks metrics (integrate with Prometheus)
- **TracingInterceptor**: Trace context propagation (integrate with OpenTelemetry)

## License

MIT
