# Kafka Module

Common Golang Kafka module providing opinionated producer and consumer functionality for microservices.

## Features

- **Producer**: Synchronous message publishing with SASL/TLS support
- **Consumer**: Sarama consumer-group based consumption with retry support
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
    
    "github.com/caarlos0/env/v11"
    
    "github.com/routerarchitects/ra-common-mods/kafka"
)

func main() {
    var cfg kafka.Config
    if err := env.Parse(&cfg); err != nil {
        log.Fatal(err)
    }
    // Ensure required values are set (for example KAFKA_BROKERS).
    
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
    "time"

    "github.com/caarlos0/env/v11"
    
    "github.com/routerarchitects/ra-common-mods/kafka"
)

func main() {
    var cfg kafka.Config
    if err := env.Parse(&cfg); err != nil {
        log.Fatal(err)
    }
    // Ensure required values are set (for example KAFKA_BROKERS and KAFKA_CONSUMER_GROUP_ID).
    
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
        AutoCommit: 5 * time.Second,
        Interceptors: []kafka.Interceptor{
            kafka.LoggingInterceptor(logger),
            kafka.RecoveryInterceptor(logger),
            kafka.MetricsInterceptor(),
        },
        RetryPolicy: &kafka.RetryPolicy{
            Strategy: kafka.RetryStrategyLogAndIgnore,
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

You can load configuration from environment variables directly using `github.com/caarlos0/env/v11`:

```go
var cfg kafka.Config
if err := env.Parse(&cfg); err != nil {
    log.Fatal(err)
}
```

`kafka.Config` already has `env` and `envDefault` tags, so parsing fills values from environment variables and applies defaults where defined.
Minimum required env vars for a working consumer setup are:
- `KAFKA_BROKERS`
- `KAFKA_CONSUMER_GROUP_ID`
- `KAFKA_CONSUMER_COMMIT_INTERVAL` (must be greater than zero if set)

Topic subscription is runtime-driven, not config-driven:
- Use `consumer.Subscribe(ctx, topic, ...)` for a single topic.
- Use `consumer.SubscribeMultiple(ctx, topics, ...)` for multiple topics.

## Consumption Model

The consumer processes messages synchronously per claim and marks offsets after processing.
Commit behavior is controlled by `SubscribeOptions.AutoCommit` and consumer commit settings.
`Consumer.CommitInterval` (`COMMIT_INTERVAL`) must be greater than zero.

- `AutoCommit == -1`: offsets are committed synchronously after each processed message.
- `AutoCommit == 0`: inherit `Consumer.CommitInterval` (`COMMIT_INTERVAL`).
- `AutoCommit > 0`: offsets are auto-committed at the configured interval.

## Retry & DLQ

Failed messages are automatically retried with exponential backoff:

1. Message fails → Retry with `InitialDelay`
2. Exponential backoff → Multiply delay by `Multiplier` 
3. Max retries exhausted:
   - `RetryStrategyDLQ`: Send to DLQ topic (if configured)
   - `RetryStrategyLogAndIgnore`: Log and skip message
   - `RetryStrategyInfinite`: Retry until success or context cancellation

## Middleware/Interceptors

Built-in interceptors for cross-cutting concerns:

- **LoggingInterceptor**: Logs message processing with duration
- **RecoveryInterceptor**: Recovers from panics
- **MetricsInterceptor**: Tracks metrics (integrate with Prometheus)
- **TracingInterceptor**: Trace context propagation (integrate with OpenTelemetry)
