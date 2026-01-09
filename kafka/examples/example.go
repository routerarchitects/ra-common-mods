package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/routerarchitects/ra-common-mods/kafka"
	"github.com/routerarchitects/ra-common-mods/logger"
)

// Event represents a sample message structure
type Event struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	Payload   string    `json:"payload"`
	Timestamp time.Time `json:"timestamp"`
	Worker    int       `json:"worker"`
}

type Config struct {
	Log   logger.Config `json:"log"`
	Kafka kafka.Config  `json:"kafka"`
}

func main() {
	// data, err := os.ReadFile("config.json")
	// if err != nil {
	// 	panic(err)
	// }
	// Setup logger
	var appCfg Config
	// if err := json.Unmarshal(data, &appCfg); err != nil {
	// 	panic(err)
	// }
	if err := env.Parse(&appCfg); err != nil {
		panic(err)
	}

	aLog, shutdown, err := logger.Init(appCfg.Log)
	if err != nil {
		panic(err)
	}
	defer shutdown()

	aLog.Info("Logger Initiated")

	// Create context with cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle graceful shutdown
	sigterm := make(chan os.Signal, 1)
	signal.Notify(sigterm, syscall.SIGINT, syscall.SIGTERM)

	// Configuration
	cfg := appCfg.Kafka
	cfg.Brokers = []string{"924a0bd396af:9092"}
	cfg.ClientID = "kafka-example"
	cfg.Consumer.GroupID = "example-consumer-group"

	topic := "example-topic"

	// Start producer in background
	go runProducer(ctx, cfg, topic, aLog)

	// Wait a bit for producer to start
	time.Sleep(2 * time.Second)

	// Start consumer
	go runConsumer(ctx, cfg, topic, aLog)

	// Wait for termination signal
	<-sigterm
	aLog.Info("Shutting down gracefully...")
	cancel()

	// Give some time for cleanup
	time.Sleep(2 * time.Second)
	aLog.Info("Shutdown complete")
}

// runProducer creates a producer and spawns 10 goroutines to publish messages
func runProducer(ctx context.Context, cfg kafka.Config, topic string, logger *slog.Logger) {
	// Create producer
	producer, err := kafka.NewProducer(cfg)
	if err != nil {
		log.Fatalf("Failed to create producer: %v", err)
	}
	defer producer.Close()

	logger.Info("Producer started")

	// Spawn 10 producer goroutines
	var wg sync.WaitGroup
	numProducers := 10

	for i := 1; i <= numProducers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()

			messageCount := 0
			ticker := time.NewTicker(1 * time.Second)
			defer ticker.Stop()

			for {
				select {
				case <-ctx.Done():
					logger.Info("Producer worker shutting down",
						"worker", workerID,
						"messages_sent", messageCount,
					)
					return

				case <-ticker.C:
					// Create event
					event := Event{
						ID:        fmt.Sprintf("event-%d-%d", workerID, messageCount),
						Type:      "user.action",
						Payload:   fmt.Sprintf("Message from worker %d, count %d", workerID, messageCount),
						Timestamp: time.Now(),
						Worker:    workerID,
					}

					// Publish as JSON
					err := producer.PublishJSON(ctx, topic, event.ID, event)
					if err != nil {
						logger.Error("Failed to publish message",
							"worker", workerID,
							"error", err,
						)
					} else {
						messageCount++
						logger.Info("Published message",
							"worker", workerID,
							"event_id", event.ID,
							"total_sent", messageCount,
						)
					}
				}
			}
		}(i)
	}

	// Wait for all producers to finish
	wg.Wait()
	logger.Info("All producer workers stopped")
}

// runConsumer creates a consumer with 10 workers to process messages
func runConsumer(ctx context.Context, cfg kafka.Config, topic string, logger *slog.Logger) {
	// Create consumer
	consumer, err := kafka.NewConsumer(cfg, logger)
	if err != nil {
		log.Fatalf("Failed to create consumer: %v", err)
	}
	defer consumer.Close()

	logger.Info("Consumer started with 10 workers")

	// Track statistics
	stats := &consumerStats{
		processedByWorker: make(map[int]int),
	}

	// Define message handler
	handler := func(ctx context.Context, msg *kafka.Message) error {
		// Decode the event
		var event Event
		if err := json.Unmarshal(msg.Value, &event); err != nil {
			logger.Error("Failed to unmarshal event",
				"error", err,
				"value", string(msg.Value),
			)
			return err
		}

		// Simulate processing time
		processingTime := time.Duration(50+event.Worker*10) * time.Millisecond
		time.Sleep(processingTime)

		// Track stats
		stats.mu.Lock()
		stats.total++
		stats.processedByWorker[event.Worker]++
		currentTotal := stats.total
		stats.mu.Unlock()

		logger.Info("Processed message",
			"event_id", event.ID,
			"event_type", event.Type,
			"producer_worker", event.Worker,
			"processing_time", processingTime,
			"total_processed", currentTotal,
		)

		return nil
	}

	// Subscribe options with 10 workers
	opts := &kafka.SubscribeOptions{
		Interceptors: []kafka.Interceptor{
			kafka.RecoveryInterceptor(logger),
			kafka.MetricsInterceptor(),
		},
		RetryPolicy: &kafka.RetryPolicy{
			Strategy:     kafka.RetryStrategyInfinite,
			MaxRetries:   3,
			InitialDelay: 100 * time.Millisecond,
			MaxDelay:     30 * time.Second,
			Multiplier:   2.0,
		},
	}

	// Start stats reporter
	go reportStats(ctx, stats, logger)

	// Subscribe (blocking until context is cancelled)
	err = consumer.Subscribe(ctx, topic, handler, opts)
	if err != nil && err != context.Canceled {
		logger.Error("Consumer error", "error", err)
	}

	// Print final statistics
	stats.mu.Lock()
	logger.Info("Consumer stopped",
		"total_processed", stats.total,
		"by_worker", stats.processedByWorker,
	)
	stats.mu.Unlock()
}

// consumerStats tracks consumer statistics
type consumerStats struct {
	mu                sync.Mutex
	total             int
	processedByWorker map[int]int
}

// reportStats periodically reports consumer statistics
func reportStats(ctx context.Context, stats *consumerStats, logger *slog.Logger) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			stats.mu.Lock()
			total := stats.total
			byWorker := make(map[int]int)
			for k, v := range stats.processedByWorker {
				byWorker[k] = v
			}
			stats.mu.Unlock()

			logger.Info("=== Consumer Statistics ===",
				"total_messages_processed", total,
				"messages_by_producer_worker", byWorker,
			)
		}
	}
}
