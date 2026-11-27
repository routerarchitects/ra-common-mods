package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/routerarchitects/ra-common-mods/kafka"
)

func main() {
	os.Exit(run())
}

func run() int {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	brokers := []string{"localhost:9092"}
	if envBrokers := os.Getenv("KAFKA_BROKERS"); envBrokers != "" {
		brokers = nil
		for _, part := range strings.Split(envBrokers, ",") {
			if b := strings.TrimSpace(part); b != "" {
				brokers = append(brokers, b)
			}
		}
	}

	brokerCfg := kafka.DefaultBrokerConfig()
	brokerCfg.Brokers = brokers
	brokerCfg.ClientID = "hybrid-example"

	reg := prometheus.NewRegistry()
	metrics, err := kafka.NewMetrics(reg)
	if err != nil {
		logger.Error("failed to init metrics", "error", err)
		return 1
	}

	// Shared producer (must stay open until consumers finish shutting down).
	dlqProducer, err := kafka.NewProducer(
		brokerCfg,
		kafka.DefaultProducerConfig(),
		kafka.WithProducerMetrics(metrics),
	)
	if err != nil {
		logger.Error("failed to create producer", "error", err)
		return 1
	}
	defer dlqProducer.Close()

	// Critical consumer group: sync commits, oldest offset fallback, strict DLQ stop/retry.
	criticalCfg := kafka.DefaultConsumerConfig()
	criticalCfg.GroupID = "hybrid-critical-group"
	criticalCfg.InitialOffset = "oldest"
	criticalCfg.CommitMode = kafka.CommitSync

	criticalConsumer, err := kafka.NewConsumer(brokerCfg, criticalCfg, logger, kafka.WithConsumerMetrics(metrics))
	if err != nil {
		logger.Error("failed to create critical consumer", "error", err)
		return 1
	}
	defer criticalConsumer.Close()

	if err := criticalConsumer.Subscribe(kafka.Subscription{
		Topic: "critical.events",
		RetryPolicy: &kafka.RetryPolicy{
			Strategy:         kafka.RetryStrategyDLQ,
			MaxRetries:       3,
			InitialDelay:     100 * time.Millisecond,
			MaxDelay:         5 * time.Second,
			Multiplier:       2.0,
			DLQTopic:         "critical.events.dlq",
			DLQProducer:      dlqProducer,
			DLQFailurePolicy: kafka.DLQFailureRetry,
		},
		Handler: func(ctx context.Context, msg *kafka.Message) error {
			logger.Info("critical event", "topic", msg.Topic, "partition", msg.Partition, "offset", msg.Offset)
			return nil
		},
	}); err != nil {
		logger.Error("subscribe critical.events failed", "error", err)
		return 1
	}

	// Background consumer group: multi-topic, periodic commits, availability-oriented policies.
	bgCfg := kafka.DefaultConsumerConfig()
	bgCfg.GroupID = "hybrid-background-group"
	bgCfg.BalanceStrategy = "sticky"

	bgConsumer, err := kafka.NewConsumer(brokerCfg, bgCfg, logger, kafka.WithConsumerMetrics(metrics))
	if err != nil {
		logger.Error("failed to create background consumer", "error", err)
		return 1
	}
	defer bgConsumer.Close()

	for _, topic := range []string{"telemetry.state", "telemetry.audit"} {
		tName := topic
		if err := bgConsumer.Subscribe(kafka.Subscription{
			Topic:          tName,
			HandlerTimeout: 10 * time.Second,
			RetryPolicy:    kafka.DefaultRetryPolicy(),
			Handler: func(ctx context.Context, msg *kafka.Message) error {
				logger.Info("background event", "topic", msg.Topic, "partition", msg.Partition, "offset", msg.Offset)
				return nil
			},
		}); err != nil {
			logger.Error("subscribe failed", "topic", tName, "error", err)
			return 1
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var (
		wg     sync.WaitGroup
		failed atomic.Bool
	)
	runGroup := func(name string, gc kafka.Consumer) {
		defer wg.Done()
		if err := gc.Run(ctx); err != nil {
			logger.Error("consumer group exited with error", "name", name, "error", err, "health", gc.Health())
			failed.Store(true)
			stop()
		}
	}

	wg.Add(2)
	go runGroup("critical", criticalConsumer)
	go runGroup("background", bgConsumer)

	fmt.Println("Hybrid consumers started. Press Ctrl+C to shut down.")
	wg.Wait()

	if failed.Load() {
		return 1
	}
	return 0
}
