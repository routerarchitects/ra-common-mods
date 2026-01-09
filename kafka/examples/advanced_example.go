package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/routerarchitects/ra-common-mods/kafka"
	"github.com/routerarchitects/ra-common-mods/logger"
)

// Global constants for topics (usually these would be env vars)
const (
	TopicRetrySuccess   = "advanced-retry-success"
	TopicRetryDLQ       = "advanced-retry-dlq"
	TopicDLQTarget      = "advanced-dlq-target"
	TopicConcurrency    = "advanced-concurrency"
	TopicMultiConsumerA = "advanced-multi-a"
	TopicMultiConsumerB = "advanced-multi-b"
	ConsumerGroup       = "advanced-example-group"
)

// ChaosState tracks failure counts for specific message IDs to simulate transient errors
type ChaosState struct {
	mu          sync.Mutex
	failures    map[string]int
	maxFailures map[string]int  // How many times a message should fail before succeeding
	failForever map[string]bool // If true, fail forever
}

func NewChaosState() *ChaosState {
	return &ChaosState{
		failures:    make(map[string]int),
		maxFailures: make(map[string]int),
		failForever: make(map[string]bool),
	}
}

func (c *ChaosState) ShouldFail(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.failForever[id] {
		return true
	}

	count := c.failures[id]
	max := c.maxFailures[id]

	if count < max {
		c.failures[id]++
		return true
	}
	return false
}

func (c *ChaosState) ConfigureTransientFailure(id string, failures int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.maxFailures[id] = failures
}

func (c *ChaosState) ConfigurePermanentFailure(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failForever[id] = true
}

// Payload is our test message structure
type Payload struct {
	ID        string `json:"id"`
	Msg       string `json:"msg"`
	Timestamp int64  `json:"ts"`
}

func main() {
	// 1. Initialize Logger
	logCfg := logger.Config{
		ServiceName:    "kafka-advanced-example",
		ServiceVersion: "0.1.0",
		Environment:    "dev",
		Levels: logger.LevelsConfig{
			DefaultLevel: "info",
		},
		Output: logger.OutputConfig{
			Format: "text", // easier to read in console
		},
	}
	l, _, err := logger.Init(logCfg)
	if err != nil {
		panic(err)
	}

	l.Info("Starting Advanced Kafka Example")

	// 2. Base Kafka Config
	// In a real app, this comes from env vars.
	// Assuming localhost:9092 for this example.
	brokers := []string{"localhost:9092"}
	if envBrokers := os.Getenv("KAFKA_BROKERS"); envBrokers != "" {
		brokers = []string{envBrokers}
	}

	baseCfg := kafka.Config{
		Brokers:  brokers,
		ClientID: "advanced-example",
		Consumer: kafka.ConsumerConfig{
			GroupID:           ConsumerGroup,
			InitialOffset:     "newest",
			SessionTimeout:    10 * time.Second,
			HeartbeatInterval: 3 * time.Second,
		},
		Producer: kafka.ProducerConfig{
			RequiredAcks: -1,
			Idempotent:   true,
			MaxRetries:   3,
			Timeout:      5 * time.Second,
		},
	}

	// 3. Create Shared Producer
	producer, err := kafka.NewProducer(baseCfg)
	if err != nil {
		l.Error("Failed to create producer", "error", err)
		os.Exit(1)
	}
	defer producer.Close()

	// 4. Create Context
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		l.Info("Shutdown signal received")
		cancel()
	}()

	// 5. Initialize Chaos State
	chaos := NewChaosState()

	var wg sync.WaitGroup

	// --- Scenario 1: Retry with eventual success ---
	wg.Add(1)
	go func() {
		defer wg.Done()
		runRetrySuccessScenario(ctx, l, baseCfg, producer, chaos)
	}()

	// --- Scenario 2: DLQ Strategy ---
	wg.Add(1)
	go func() {
		defer wg.Done()
		runDLQScenario(ctx, l, baseCfg, producer, chaos)
	}()

	// --- Scenario 3: Concurrency / Worker Pool ---
	wg.Add(1)
	go func() {
		defer wg.Done()
		runConcurrencyScenario(ctx, l, baseCfg, producer)
	}()

	// --- Scenario 4: Multiple Independent Consumers ---
	wg.Add(1)
	go func() {
		defer wg.Done()
		runMultiConsumerScenario(ctx, l, baseCfg, producer)
	}()

	wg.Wait()
	l.Info("All scenarios completed (or context cancelled)")
}

// --- Scenario Helpers ---

func runRetrySuccessScenario(ctx context.Context, parentLog *slog.Logger, cfg kafka.Config, producer kafka.Producer, chaos *ChaosState) {
	l := parentLog.With("scenario", "retry-success")
	l.Info("Starting Scenario: Retry Success")

	// Setup unique group for this scenario to avoid collisions
	myCfg := cfg
	myCfg.Consumer.GroupID = "group-retry-success"

	consumer, err := kafka.NewConsumer(myCfg, l)
	if err != nil {
		l.Error("Failed to create consumer", "error", err)
		return
	}
	defer consumer.Close()

	// Prepare Chaos: ID "fail-twice" will fail 2 times then succeed
	msgID := "fail-twice"
	chaos.ConfigureTransientFailure(msgID, 2)

	// Publish Message
	payload := Payload{ID: msgID, Msg: "I will fail twice", Timestamp: time.Now().Unix()}
	if err := producer.PublishJSON(ctx, TopicRetrySuccess, msgID, payload); err != nil {
		l.Error("Failed to publish", "error", err)
		return
	}
	l.Info("Published test message", "id", msgID)

	// Handler
	handler := func(c context.Context, msg *kafka.Message) error {
		var p Payload
		if err := json.Unmarshal(msg.Value, &p); err != nil {
			return nil // invalid json, skip
		}

		if chaos.ShouldFail(p.ID) {
			l.Warn("Simulating processing FAILURE", "id", p.ID)
			return errors.New("simulated transient error")
		}

		l.Info("Processing SUCCESS", "id", p.ID)
		return nil
	}

	opts := kafka.DefaultSubscribeOptions()
	opts.RetryPolicy = &kafka.RetryPolicy{
		Strategy:     kafka.RetryStrategyInfinite, // Or ignore, but we want it to succeed eventually
		MaxRetries:   5,
		InitialDelay: 100 * time.Millisecond,
		MaxDelay:     1 * time.Second,
		Multiplier:   1.5,
	}

	// Run consumer for a fixed time or until verified (simplified: fixed time here)
	timeoutCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := consumer.Subscribe(timeoutCtx, TopicRetrySuccess, handler, opts); err != nil {
		if err != context.DeadlineExceeded && err != context.Canceled {
			l.Error("Consumer exited with error", "error", err)
		}
	}
	l.Info("Scenario: Retry Success - Finished")
}

func runDLQScenario(ctx context.Context, parentLog *slog.Logger, cfg kafka.Config, producer kafka.Producer, chaos *ChaosState) {
	l := parentLog.With("scenario", "dlq")
	l.Info("Starting Scenario: DLQ")

	myCfg := cfg
	myCfg.Consumer.GroupID = "group-dlq"

	// 1. Create Main Consumer (that will fail)
	consumer, err := kafka.NewConsumer(myCfg, l)
	if err != nil {
		l.Error("Failed to create consumer", "error", err)
		return
	}
	defer consumer.Close()

	// 2. Create DLQ Consumer (to verify receipt)
	verifyCfg := cfg
	verifyCfg.Consumer.GroupID = "group-dlq-verify" // different group
	dlqVerifier, err := kafka.NewConsumer(verifyCfg, l.With("role", "verifier"))
	if err != nil {
		return
	}
	defer dlqVerifier.Close()

	// Prepare Chaos: ID "fail-forever"
	msgID := "fail-forever"
	chaos.ConfigurePermanentFailure(msgID)

	// Publish Message
	payload := Payload{ID: msgID, Msg: "I will fail forever", Timestamp: time.Now().Unix()}
	if err := producer.PublishJSON(ctx, TopicRetryDLQ, msgID, payload); err != nil {
		l.Error("Failed to publish", "error", err)
		return
	}

	// Shared verification state
	var dlqReceived atomic.Bool

	var wg sync.WaitGroup
	wg.Add(2)

	// Routine A: Main Consumer (Failing)
	go func() {
		defer wg.Done()
		handler := func(c context.Context, msg *kafka.Message) error {
			var p Payload
			json.Unmarshal(msg.Value, &p)
			if p.ID == msgID {
				l.Warn("Failing message to trigger DLQ", "id", p.ID)
				return errors.New("permanent error")
			}
			return nil
		}

		// Configure Policy with DLQ
		opts := kafka.DefaultSubscribeOptions()
		opts.RetryPolicy = &kafka.RetryPolicy{
			Strategy:     kafka.RetryStrategyDLQ,
			MaxRetries:   2,
			InitialDelay: 50 * time.Millisecond,
			DLQTopic:     TopicDLQTarget,
			DLQProducer:  producer, // Using shared producer
		}

		// Run briefly
		timeoutCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		consumer.Subscribe(timeoutCtx, TopicRetryDLQ, handler, opts)
	}()

	// Routine B: DLQ Verifier (Listening on TopicDLQTarget)
	go func() {
		defer wg.Done()
		handler := func(c context.Context, msg *kafka.Message) error {
			// Check headers for x-original-topic
			origTopic := ""
			for _, h := range msg.Headers {
				if h.Key == "x-original-topic" {
					origTopic = string(h.Value)
				}
			}

			l.Info("Received message in DLQ", "topic", msg.Topic, "orig_topic", origTopic)
			if origTopic == TopicRetryDLQ {
				dlqReceived.Store(true)
			}
			return nil
		}

		timeoutCtx, cancel := context.WithTimeout(ctx, 6*time.Second) // slightly longer
		defer cancel()
		dlqVerifier.Subscribe(timeoutCtx, TopicDLQTarget, handler, nil)
	}()

	wg.Wait()

	if dlqReceived.Load() {
		l.Info("Scenario: DLQ - VERIFIED SUCCESS")
	} else {
		l.Error("Scenario: DLQ - FAILED (Message not received in DLQ)")
	}
}

func runConcurrencyScenario(ctx context.Context, parentLog *slog.Logger, cfg kafka.Config, producer kafka.Producer) {
	l := parentLog.With("scenario", "concurrency")
	l.Info("Starting Scenario: Concurrency")

	myCfg := cfg
	myCfg.Consumer.GroupID = "group-concurrent"

	// Note: For this to work efficiently, TopicConcurrency should have > 1 partition.
	// If it has 1 partition, Sarama will give it to only 1 consumer in the group.
	// But we can still see the API usage.

	// Publish a batch of messages
	count := 20
	for i := 0; i < count; i++ {
		key := fmt.Sprintf("k-%d", i%3) // 3 partitions ideally
		payload := Payload{ID: fmt.Sprintf("job-%d", i), Msg: "worker job", Timestamp: time.Now().Unix()}
		producer.PublishJSON(ctx, TopicConcurrency, key, payload)
	}
	l.Info("Published batch of messages", "count", count)

	// Spawn 3 consumers in the same group (simulating 3 replicas of a service)
	// In a real app these would be in different processes/pods.
	var wg sync.WaitGroup
	workers := 3

	for i := 0; i < workers; i++ {
		wg.Add(1)
		workerID := i
		go func() {
			defer wg.Done()
			wLog := l.With("worker", workerID)

			c, _ := kafka.NewConsumer(myCfg, wLog)
			defer c.Close()

			handler := func(ctx context.Context, msg *kafka.Message) error {
				// simulate work
				time.Sleep(100 * time.Millisecond)
				wLog.Info("Worker processed message", "partition", msg.Partition, "offset", msg.Offset)
				return nil
			}

			// Run for 5 seconds
			timeoutCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()

			c.Subscribe(timeoutCtx, TopicConcurrency, handler, nil)
		}()
	}

	wg.Wait()
	l.Info("Scenario: Concurrency - Finished")
}

func runMultiConsumerScenario(ctx context.Context, parentLog *slog.Logger, cfg kafka.Config, producer kafka.Producer) {
	l := parentLog.With("scenario", "multi-consumer")
	l.Info("Starting Scenario: Multi Independent Consumers")

	// Publish to Topic A and Topic B
	producer.PublishJSON(ctx, TopicMultiConsumerA, "a1", Payload{Msg: "Message for A"})
	producer.PublishJSON(ctx, TopicMultiConsumerB, "b1", Payload{Msg: "Message for B"})

	var wg sync.WaitGroup
	wg.Add(2)

	// Consumer A
	go func() {
		defer wg.Done()
		myCfg := cfg
		myCfg.Consumer.GroupID = "group-service-A"

		c, _ := kafka.NewConsumer(myCfg, l.With("service", "A"))
		defer c.Close()

		handler := func(ctx context.Context, msg *kafka.Message) error {
			l.Info("Service A received message", "topic", msg.Topic)
			if msg.Topic != TopicMultiConsumerA {
				l.Error("Service A received wrong topic!", "topic", msg.Topic)
			}
			return nil
		}

		timeoutCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		c.Subscribe(timeoutCtx, TopicMultiConsumerA, handler, nil)
	}()

	// Consumer B
	go func() {
		defer wg.Done()
		myCfg2 := cfg
		myCfg2.Consumer.GroupID = "group-service-B"

		c, _ := kafka.NewConsumer(myCfg2, l.With("service", "B"))
		defer c.Close()

		handler := func(ctx context.Context, msg *kafka.Message) error {
			l.Info("Service B received message", "topic", msg.Topic)
			if msg.Topic != TopicMultiConsumerB {
				l.Error("Service B received wrong topic!", "topic", msg.Topic)
			}
			return nil
		}

		timeoutCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		c.Subscribe(timeoutCtx, TopicMultiConsumerB, handler, nil)
	}()

	wg.Wait()
	l.Info("Scenario: Multi Independent Consumers - Finished")
}
