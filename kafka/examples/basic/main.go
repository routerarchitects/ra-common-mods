package main

import (
	"context"
	"encoding/json"
	"fmt"
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
	Log      logger.Config        `json:"log"`
	Broker   kafka.BrokerConfig   `json:"broker"`
	Producer kafka.ProducerConfig `json:"producer" envPrefix:"KAFKA_PRODUCER_"`
	Consumer kafka.ConsumerConfig `json:"consumer" envPrefix:"KAFKA_CONSUMER_"`
}

func main() {
	os.Exit(run())
}

func run() int {
	var appCfg Config
	if err := env.Parse(&appCfg); err != nil {
		panic(err)
	}

	aLog, shutdown, err := logger.Init(appCfg.Log)
	if err != nil {
		panic(err)
	}
	defer shutdown()

	aLog.Info("Logger Initiated")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	brokerCfg := appCfg.Broker
	if len(brokerCfg.Brokers) == 0 {
		brokerCfg.Brokers = []string{"localhost:9092"}
	}
	brokerCfg.ClientID = "kafka-example"

	producerCfg := appCfg.Producer
	consumerCfg := appCfg.Consumer
	consumerCfg.GroupID = "example-consumer-group"

	topic := "example-topic"

	// Shared producer must outlive consumer goroutines so shutdown finishes cleanly.
	producer, err := kafka.NewProducer(brokerCfg, producerCfg)
	if err != nil {
		aLog.Error("Failed to create producer", "error", err)
		return 1
	}
	defer producer.Close()

	consumer, err := kafka.NewConsumer(brokerCfg, consumerCfg, aLog)
	if err != nil {
		aLog.Error("Failed to create consumer", "error", err)
		return 1
	}
	defer consumer.Close()

	stats := &consumerStats{
		processedByWorker: make(map[int]int),
	}

	handler := newMessageHandler(stats, aLog)
	if err := consumer.Subscribe(kafka.Subscription{
		Topic:   topic,
		Handler: handler,
		Interceptors: []kafka.Interceptor{
			kafka.RecoveryInterceptor(aLog),
		},
		RetryPolicy: kafka.DefaultRetryPolicy(),
	}); err != nil {
		aLog.Error("Subscribe failed", "error", err)
		return 1
	}

	var (
		wg          sync.WaitGroup
		consumerErr error
	)

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := consumer.Run(ctx); err != nil {
			consumerErr = err
			aLog.Error("Consumer exited with terminal error; shutting down", "error", err)
			stop()
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		reportStats(ctx, stats, aLog)
	}()

	if !waitForReady(ctx, consumer, 30*time.Second) {
		timedOut := ctx.Err() == nil
		stop()
		wg.Wait()
		if timedOut {
			aLog.Error("Consumer did not become ready before timeout", "health", consumer.Health())
		}
		if timedOut || consumerErr != nil {
			return 1
		}
		return 0
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		runProducer(ctx, producer, topic, aLog)
	}()

	<-ctx.Done()
	aLog.Info("Shutting down gracefully...")
	wg.Wait()

	stats.mu.Lock()
	aLog.Info("Consumer stopped",
		"total_processed", stats.total,
		"by_worker", stats.processedByWorker,
	)
	stats.mu.Unlock()

	if consumerErr != nil {
		aLog.Error("Shutdown complete with consumer failure", "error", consumerErr)
		return 1
	}
	aLog.Info("Shutdown complete")
	return 0
}

func waitForReady(ctx context.Context, c kafka.Consumer, timeout time.Duration) bool {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		if c.Health().Ready {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return false
		case <-ticker.C:
		}
	}
}

func newMessageHandler(stats *consumerStats, logger *slog.Logger) kafka.Handler {
	return func(ctx context.Context, msg *kafka.Message) error {
		var event Event
		if err := json.Unmarshal(msg.Value, &event); err != nil {
			logger.Error("Failed to unmarshal event", "error", err)
			return kafka.Permanent(err)
		}

		processingTime := time.Duration(50+event.Worker*10) * time.Millisecond
		select {
		case <-time.After(processingTime):
		case <-ctx.Done():
			return ctx.Err()
		}

		stats.mu.Lock()
		stats.total++
		stats.processedByWorker[event.Worker]++
		currentTotal := stats.total
		stats.mu.Unlock()

		if event.ID == "event-1-5" {
			err := fmt.Errorf("simulated processing error for message index %s", event.ID)
			logger.Error("Processing failed for message",
				"event_id", event.ID,
				"event_type", event.Type,
				"producer_worker", event.Worker,
				"processing_time", processingTime,
				"total_processed", currentTotal,
				"error", err,
			)
			return err
		}

		logger.Info("Processed message",
			"event_id", event.ID,
			"event_type", event.Type,
			"producer_worker", event.Worker,
			"processing_time", processingTime,
			"total_processed", currentTotal,
		)
		return nil
	}
}

// runProducer spawns producer goroutines to publish messages until completion or context cancellation.
func runProducer(ctx context.Context, producer kafka.Producer, topic string, logger *slog.Logger) {
	logger.Info("Producer started")

	var wg sync.WaitGroup
	numProducers := 1

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
					if messageCount >= 20 {
						logger.Info("Producer worker reached max message limit",
							"worker", workerID,
							"messages_sent", messageCount,
						)
						return
					}
					event := Event{
						ID:        fmt.Sprintf("event-%d-%d", workerID, messageCount),
						Type:      "user.action",
						Payload:   fmt.Sprintf("Message from worker %d, count %d", workerID, messageCount),
						Timestamp: time.Now(),
						Worker:    workerID,
					}

					if err := producer.PublishJSON(ctx, topic, event.ID, event); err != nil {
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

	wg.Wait()
	logger.Info("All producer workers stopped")
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
