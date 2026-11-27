package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/routerarchitects/ra-common-mods/kafka"
	"github.com/routerarchitects/ra-common-mods/logger"
)

type scenarioStatus struct {
	Passed bool
	Reason string
}

type scenarioReport struct {
	mu      sync.Mutex
	results map[string]scenarioStatus
	cancel  context.CancelFunc
}

func newScenarioReport(cancel context.CancelFunc) *scenarioReport {
	return &scenarioReport{results: make(map[string]scenarioStatus), cancel: cancel}
}

func (r *scenarioReport) set(name string, passed bool, reason string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.results[name]; exists {
		return false
	}
	r.results[name] = scenarioStatus{Passed: passed, Reason: reason}
	return true
}

func (r *scenarioReport) runConsumer(ctx context.Context, c kafka.Consumer, l *slog.Logger, scenario string) {
	if err := c.Run(ctx); err != nil {
		l.Error("Consumer exited with error", "error", err)
		r.set(scenario, false, err.Error())
		r.cancel()
	}
}

func (r *scenarioReport) logSummary(l *slog.Logger) {
	r.mu.Lock()
	defer r.mu.Unlock()

	l.Info("=== Scenario Summary ===")
	for _, name := range []string{"retry-success", "dlq", "concurrency", "multi-consumer"} {
		res, ok := r.results[name]
		if !ok {
			l.Error("scenario status missing", "scenario", name)
			continue
		}
		if res.Passed {
			l.Info("scenario passed", "scenario", name)
		} else {
			l.Error("scenario failed", "scenario", name, "reason", res.Reason)
		}
	}
}

func (r *scenarioReport) allPassed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, name := range []string{"retry-success", "dlq", "concurrency", "multi-consumer"} {
		if res, ok := r.results[name]; !ok || !res.Passed {
			return false
		}
	}
	return true
}

func waitForConsumerReady(ctx context.Context, c kafka.Consumer, timeout time.Duration) bool {
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

func assertScenario(l *slog.Logger, report *scenarioReport, scenario string, cond bool, msg string, args ...any) {
	if cond {
		if report.set(scenario, true, "") {
			l.Info("assertion passed", "scenario", scenario)
		}
		return
	}
	reason := fmt.Sprintf(msg, args...)
	if report.set(scenario, false, reason) {
		l.Error("assertion failed", "scenario", scenario, "reason", reason)
	}
}

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
	os.Exit(run())
}

func run() int {
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
	l, shutdown, err := logger.Init(logCfg)
	if err != nil {
		panic(err)
	}
	defer shutdown()

	l.Info("Starting Advanced Kafka Example")

	// 2. Base Kafka Config
	// In a real app, this comes from env vars.
	// Assuming localhost:9092 for this example.
	brokers := []string{"localhost:9092"}
	if envBrokers := os.Getenv("KAFKA_BROKERS"); envBrokers != "" {
		parts := strings.Split(envBrokers, ",")
		brokers = make([]string, 0, len(parts))
		for _, broker := range parts {
			broker = strings.TrimSpace(broker)
			if broker != "" {
				brokers = append(brokers, broker)
			}
		}
		if len(brokers) == 0 {
			brokers = []string{"localhost:9092"}
		}
	}

	brokerCfg := kafka.DefaultBrokerConfig()
	brokerCfg.Brokers = brokers
	brokerCfg.ClientID = "advanced-example"

	producerCfg := kafka.DefaultProducerConfig()
	producerCfg.Timeout = 5 * time.Second

	consumerCfg := kafka.DefaultConsumerConfig()
	consumerCfg.GroupID = ConsumerGroup

	// 3. Create Shared Producer
	producer, err := kafka.NewProducer(brokerCfg, producerCfg)
	if err != nil {
		l.Error("Failed to create producer", "error", err)
		return 1
	}
	defer producer.Close()

	// 4. Create Context
	signalCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancel(signalCtx)
	defer cancel()

	// 5. Initialize Chaos State
	chaos := NewChaosState()
	report := newScenarioReport(cancel)

	var wg sync.WaitGroup

	// --- Scenario 1: Retry with eventual success ---
	wg.Add(1)
	go func() {
		defer wg.Done()
		runRetrySuccessScenario(ctx, l, brokerCfg, consumerCfg, producer, chaos, report)
	}()

	// --- Scenario 2: DLQ Strategy ---
	wg.Add(1)
	go func() {
		defer wg.Done()
		runDLQScenario(ctx, l, brokerCfg, consumerCfg, producer, chaos, report)
	}()

	// --- Scenario 3: Concurrency / Worker Pool ---
	wg.Add(1)
	go func() {
		defer wg.Done()
		runConcurrencyScenario(ctx, l, brokerCfg, consumerCfg, producer, report)
	}()

	// --- Scenario 4: Multiple Independent Consumers ---
	wg.Add(1)
	go func() {
		defer wg.Done()
		runMultiConsumerScenario(ctx, l, brokerCfg, consumerCfg, producer, report)
	}()

	wg.Wait()
	report.logSummary(l)
	if !report.allPassed() {
		l.Error("One or more scenarios failed")
		return 1
	}
	l.Info("All scenarios completed")
	return 0
}

// --- Scenario Helpers ---

func runRetrySuccessScenario(ctx context.Context, parentLog *slog.Logger, brokerCfg kafka.BrokerConfig, baseConsumerCfg kafka.ConsumerConfig, producer kafka.Producer, chaos *ChaosState, report *scenarioReport) {
	l := parentLog.With("scenario", "retry-success")
	l.Info("Starting Scenario: Retry Success")

	// Setup unique group for this scenario to avoid collisions
	myCfg := baseConsumerCfg
	myCfg.GroupID = "group-retry-success"
	myCfg.InitialOffset = "oldest"

	consumer, err := kafka.NewConsumer(brokerCfg, myCfg, l)
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
	runCtx, runCancel := context.WithTimeout(ctx, 15*time.Second)
	defer runCancel()

	var retrySuccessVerified atomic.Bool
	handler := func(c context.Context, msg *kafka.Message) error {
		var p Payload
		if err := json.Unmarshal(msg.Value, &p); err != nil {
			return kafka.Permanent(err)
		}

		if chaos.ShouldFail(p.ID) {
			l.Warn("Simulating processing FAILURE", "id", p.ID)
			return errors.New("simulated transient error")
		}

		l.Info("Processing SUCCESS", "id", p.ID)
		if p.ID == msgID {
			retrySuccessVerified.Store(true)
			runCancel()
		}
		return nil
	}

	if err := consumer.Subscribe(kafka.Subscription{
		Topic:   TopicRetrySuccess,
		Handler: handler,
		RetryPolicy: &kafka.RetryPolicy{
			Strategy:     kafka.RetryStrategyInfinite,
			MaxRetries:   5,
			InitialDelay: 100 * time.Millisecond,
			MaxDelay:     1 * time.Second,
			Multiplier:   1.5,
		},
	}); err != nil {
		l.Error("Subscribe failed", "error", err)
		return
	}

	report.runConsumer(runCtx, consumer, l, "retry-success")
	assertScenario(l, report, "retry-success", retrySuccessVerified.Load(), "message %q was never processed successfully", msgID)
	l.Info("Scenario: Retry Success - Finished")
}

func runDLQScenario(ctx context.Context, parentLog *slog.Logger, brokerCfg kafka.BrokerConfig, baseConsumerCfg kafka.ConsumerConfig, producer kafka.Producer, chaos *ChaosState, report *scenarioReport) {
	l := parentLog.With("scenario", "dlq")
	l.Info("Starting Scenario: DLQ")

	myCfg := baseConsumerCfg
	myCfg.GroupID = "group-dlq"
	myCfg.InitialOffset = "oldest"

	// 1. Create Main Consumer (that will fail)
	consumer, err := kafka.NewConsumer(brokerCfg, myCfg, l)
	if err != nil {
		l.Error("Failed to create consumer", "error", err)
		return
	}
	defer consumer.Close()

	// 2. Create DLQ Consumer (to verify receipt)
	verifyCfg := baseConsumerCfg
	verifyCfg.GroupID = "group-dlq-verify" // different group
	verifyCfg.InitialOffset = "oldest"
	dlqVerifier, err := kafka.NewConsumer(brokerCfg, verifyCfg, l.With("role", "verifier"))
	if err != nil {
		l.Error("Failed to create DLQ verifier consumer", "error", err)
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

	mainCtx, mainCancel := context.WithTimeout(ctx, 15*time.Second)
	defer mainCancel()
	dlqCtx, dlqCancel := context.WithTimeout(ctx, 15*time.Second)
	defer dlqCancel()

	// Routine A: Main Consumer (Failing)
	go func() {
		defer wg.Done()
		handler := func(c context.Context, msg *kafka.Message) error {
			var p Payload
			if err := json.Unmarshal(msg.Value, &p); err != nil {
				l.Error("Invalid payload in DLQ scenario", "error", err)
				return kafka.Permanent(err)
			}
			if p.ID == msgID {
				l.Warn("Failing message to trigger DLQ", "id", p.ID)
				return errors.New("permanent error")
			}
			return nil
		}

		// Configure Policy with DLQ
		if err := consumer.Subscribe(kafka.Subscription{
			Topic:   TopicRetryDLQ,
			Handler: handler,
			RetryPolicy: &kafka.RetryPolicy{
				Strategy:         kafka.RetryStrategyDLQ,
				MaxRetries:       2,
				InitialDelay:     50 * time.Millisecond,
				MaxDelay:         1 * time.Second,
				Multiplier:       2.0,
				DLQTopic:         TopicDLQTarget,
				DLQProducer:      producer, // Using shared producer
				DLQFailurePolicy: kafka.DLQFailureStop,
			},
		}); err != nil {
			l.Error("DLQ subscribe failed", "error", err)
			return
		}

		report.runConsumer(mainCtx, consumer, l, "dlq")
	}()

	// Routine B: DLQ Verifier (Listening on TopicDLQTarget)
	go func() {
		defer wg.Done()
		handler := func(c context.Context, msg *kafka.Message) error {
			// Check headers for x-original-topic
			origTopic := ""
			for _, h := range msg.Headers {
				if h.Key == kafka.HeaderOriginalTopic {
					origTopic = string(h.Value)
				}
			}

			l.Info("Received message in DLQ", "topic", msg.Topic, "orig_topic", origTopic)
			if origTopic == TopicRetryDLQ {
				dlqReceived.Store(true)
				mainCancel()
				dlqCancel()
			}
			return nil
		}

		if err := dlqVerifier.Subscribe(kafka.Subscription{
			Topic:       TopicDLQTarget,
			Handler:     handler,
			RetryPolicy: kafka.DefaultRetryPolicy(),
		}); err != nil {
			l.Error("DLQ verifier subscribe failed", "error", err)
			return
		}

		report.runConsumer(dlqCtx, dlqVerifier, l, "dlq")
	}()

	wg.Wait()

	assertScenario(l, report, "dlq", dlqReceived.Load(), "expected message was not received in DLQ")
}

func runConcurrencyScenario(ctx context.Context, parentLog *slog.Logger, brokerCfg kafka.BrokerConfig, baseConsumerCfg kafka.ConsumerConfig, producer kafka.Producer, report *scenarioReport) {
	l := parentLog.With("scenario", "concurrency")
	l.Info("Starting Scenario: Concurrency")

	myCfg := baseConsumerCfg
	myCfg.GroupID = "group-concurrent"
	myCfg.InitialOffset = "oldest"

	// Note: For this to work efficiently, TopicConcurrency should have > 1 partition.
	// If it has 1 partition, Sarama will give it to only 1 consumer in the group.
	// But we can still see the API usage.

	// Publish a batch of messages
	count := 20
	for i := 0; i < count; i++ {
		key := fmt.Sprintf("k-%d", i%3) // 3 partitions ideally
		payload := Payload{ID: fmt.Sprintf("job-%d", i), Msg: "worker job", Timestamp: time.Now().Unix()}
		if err := producer.PublishJSON(ctx, TopicConcurrency, key, payload); err != nil {
			l.Error("Failed to publish concurrency payload", "key", key, "error", err)
		}
	}
	l.Info("Published batch of messages", "count", count)

	// Spawn 3 consumers in the same group (simulating 3 replicas of a service)
	// In a real app these would be in different processes/pods.
	var wg sync.WaitGroup
	workers := 3
	var processedCount atomic.Int64
	concCtx, concCancel := context.WithTimeout(ctx, 15*time.Second)
	defer concCancel()

	for i := 0; i < workers; i++ {
		wg.Add(1)
		workerID := i
		go func() {
			defer wg.Done()
			wLog := l.With("worker", workerID)

			c, err := kafka.NewConsumer(brokerCfg, myCfg, wLog)
			if err != nil {
				wLog.Error("Failed to create worker consumer", "error", err)
				return
			}
			defer c.Close()

			handler := func(ctx context.Context, msg *kafka.Message) error {
				select {
				case <-time.After(100 * time.Millisecond):
				case <-ctx.Done():
					return ctx.Err()
				}
				total := processedCount.Add(1)
				wLog.Info("Worker processed message", "partition", msg.Partition, "offset", msg.Offset)
				if total >= int64(count) {
					concCancel()
				}
				return nil
			}

			if err := c.Subscribe(kafka.Subscription{
				Topic:       TopicConcurrency,
				Handler:     handler,
				RetryPolicy: kafka.DefaultRetryPolicy(),
			}); err != nil {
				wLog.Error("Failed to subscribe worker consumer", "error", err)
				return
			}

			report.runConsumer(concCtx, c, wLog, "concurrency")
		}()
	}

	wg.Wait()
	assertScenario(l, report, "concurrency", processedCount.Load() > 0, "no messages were processed by worker consumers")
	l.Info("Scenario: Concurrency - Finished")
}

func runMultiConsumerScenario(ctx context.Context, parentLog *slog.Logger, brokerCfg kafka.BrokerConfig, baseConsumerCfg kafka.ConsumerConfig, producer kafka.Producer, report *scenarioReport) {
	l := parentLog.With("scenario", "multi-consumer")
	l.Info("Starting Scenario: Multi Independent Consumers")

	var serviceAReceived atomic.Bool
	var serviceBReceived atomic.Bool

	myCfgA := baseConsumerCfg
	myCfgA.GroupID = "group-service-A"
	myCfgA.InitialOffset = "oldest"
	cA, err := kafka.NewConsumer(brokerCfg, myCfgA, l.With("service", "A"))
	if err != nil {
		l.Error("Failed to create service A consumer", "error", err)
		return
	}
	defer cA.Close()

	myCfgB := baseConsumerCfg
	myCfgB.GroupID = "group-service-B"
	myCfgB.InitialOffset = "oldest"
	cB, err := kafka.NewConsumer(brokerCfg, myCfgB, l.With("service", "B"))
	if err != nil {
		l.Error("Failed to create service B consumer", "error", err)
		return
	}
	defer cB.Close()

	ctxA, cancelA := context.WithTimeout(ctx, 15*time.Second)
	defer cancelA()
	ctxB, cancelB := context.WithTimeout(ctx, 15*time.Second)
	defer cancelB()

	if err := cA.Subscribe(kafka.Subscription{
		Topic:       TopicMultiConsumerA,
		RetryPolicy: kafka.DefaultRetryPolicy(),
		Handler: func(ctx context.Context, msg *kafka.Message) error {
			l.Info("Service A received message", "topic", msg.Topic)
			if msg.Topic != TopicMultiConsumerA {
				l.Error("Service A received wrong topic!", "topic", msg.Topic)
				return kafka.Permanent(fmt.Errorf("unexpected topic %s", msg.Topic))
			}
			serviceAReceived.Store(true)
			cancelA()
			return nil
		},
	}); err != nil {
		l.Error("Service A subscribe failed", "error", err)
		return
	}

	if err := cB.Subscribe(kafka.Subscription{
		Topic:       TopicMultiConsumerB,
		RetryPolicy: kafka.DefaultRetryPolicy(),
		Handler: func(ctx context.Context, msg *kafka.Message) error {
			l.Info("Service B received message", "topic", msg.Topic)
			if msg.Topic != TopicMultiConsumerB {
				l.Error("Service B received wrong topic!", "topic", msg.Topic)
				return kafka.Permanent(fmt.Errorf("unexpected topic %s", msg.Topic))
			}
			serviceBReceived.Store(true)
			cancelB()
			return nil
		},
	}); err != nil {
		l.Error("Service B subscribe failed", "error", err)
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		report.runConsumer(ctxA, cA, l, "multi-consumer")
	}()

	go func() {
		defer wg.Done()
		report.runConsumer(ctxB, cB, l, "multi-consumer")
	}()

	// Wait deterministically for both consumers to report readiness before publishing.
	if !waitForConsumerReady(ctx, cA, 10*time.Second) || !waitForConsumerReady(ctx, cB, 10*time.Second) {
		l.Warn("Consumers did not report ready before timeout")
	}

	// Publish to Topic A and Topic B
	if err := producer.PublishJSON(ctx, TopicMultiConsumerA, "a1", Payload{Msg: "Message for A"}); err != nil {
		l.Error("Failed to publish message for service A", "error", err)
	}
	if err := producer.PublishJSON(ctx, TopicMultiConsumerB, "b1", Payload{Msg: "Message for B"}); err != nil {
		l.Error("Failed to publish message for service B", "error", err)
	}

	wg.Wait()
	assertScenario(
		l,
		report,
		"multi-consumer",
		serviceAReceived.Load() && serviceBReceived.Load(),
		"service message receipt incomplete: serviceA=%t serviceB=%t",
		serviceAReceived.Load(),
		serviceBReceived.Load(),
	)
	l.Info("Scenario: Multi Independent Consumers - Finished")
}
