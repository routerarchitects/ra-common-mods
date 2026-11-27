//go:build integration

package kafka_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"os/exec"
	"runtime/pprof"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IBM/sarama"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/routerarchitects/ra-common-mods/kafka"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func integrationBrokers(t *testing.T) []string {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv("KAFKA_TEST_BROKERS"))
	if raw == "" {
		raw = "localhost:9092"
	}
	parts := strings.Split(raw, ",")
	var brokers []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			brokers = append(brokers, p)
		}
	}
	if len(brokers) == 0 {
		t.Fatal("KAFKA_TEST_BROKERS yielded no brokers")
	}

	// Verify broker reachability; fail visibly when integration suite is run.
	cfg := sarama.NewConfig()
	cfg.Net.DialTimeout = 5 * time.Second
	cfg.Net.ReadTimeout = 5 * time.Second
	cfg.Net.WriteTimeout = 5 * time.Second
	var admin sarama.ClusterAdmin
	var err error
	deadline := time.Now().Add(25 * time.Second)
	for {
		admin, err = sarama.NewClusterAdmin(brokers, cfg)
		if err == nil {
			if _, lErr := admin.ListTopics(); lErr == nil {
				_ = admin.Close()
				break
			} else {
				err = lErr
				_ = admin.Close()
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("Kafka broker(s) %v unreachable for integration tests: %v", brokers, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	return brokers
}

func uniqueName(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

func createTestTopic(t *testing.T, brokers []string, topic string, partitions int32) {
	t.Helper()
	cfg := sarama.NewConfig()
	admin, err := sarama.NewClusterAdmin(brokers, cfg)
	if err != nil {
		t.Fatalf("create cluster admin: %v", err)
	}
	defer admin.Close()

	err = admin.CreateTopic(topic, &sarama.TopicDetail{
		NumPartitions:     partitions,
		ReplicationFactor: 1,
	}, false)
	if err != nil && !errors.Is(err, sarama.ErrTopicAlreadyExists) {
		t.Fatalf("create topic %s: %v", topic, err)
	}

	// Wait until metadata shows all partitions with leaders.
	client, err := sarama.NewClient(brokers, cfg)
	if err != nil {
		t.Fatalf("create metadata client: %v", err)
	}
	defer client.Close()

	deadline := time.Now().Add(15 * time.Second)
	for {
		_ = client.RefreshMetadata(topic)
		parts, pErr := client.Partitions(topic)
		if pErr == nil && int32(len(parts)) == partitions {
			allReady := true
			for _, p := range parts {
				if _, lErr := client.Leader(topic, p); lErr != nil {
					allReady = false
					break
				}
			}
			if allReady {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("topic %s metadata not ready in time", topic)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func committedOffset(t *testing.T, brokers []string, groupID, topic string, partition int32) (int64, bool) {
	t.Helper()
	admin, err := sarama.NewClusterAdmin(brokers, sarama.NewConfig())
	if err != nil {
		t.Fatalf("cluster admin: %v", err)
	}
	defer admin.Close()

	resp, err := admin.ListConsumerGroupOffsets(groupID, map[string][]int32{topic: {partition}})
	if err != nil {
		t.Fatalf("ListConsumerGroupOffsets: %v", err)
	}
	block := resp.GetBlock(topic, partition)
	if block == nil || block.Offset < 0 {
		return -1, false
	}
	return block.Offset, true
}

func waitForCondition(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", desc)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func brokerCfgFor(brokers []string) kafka.BrokerConfig {
	b := kafka.DefaultBrokerConfig()
	b.Brokers = brokers
	b.ClientID = "kafka-it"
	return b
}

func consumerCfgFor(groupID string) kafka.ConsumerConfig {
	c := kafka.DefaultConsumerConfig()
	c.GroupID = groupID
	c.InitialOffset = "oldest"
	c.CommitInterval = 300 * time.Millisecond
	c.ShutdownTimeout = 5 * time.Second
	return c
}

type faultInjectingProducer struct {
	inner     kafka.Producer
	failCount atomic.Int32
	failAll   atomic.Bool
}

func (f *faultInjectingProducer) Publish(ctx context.Context, topic string, key, value []byte) error {
	return f.PublishWithHeaders(ctx, topic, key, value, nil)
}

func (f *faultInjectingProducer) PublishWithHeaders(ctx context.Context, topic string, key, value []byte, headers []kafka.RecordHeader) error {
	if f.failAll.Load() {
		return errors.New("injected DLQ failure")
	}
	if f.failCount.Load() > 0 {
		if f.failCount.Add(-1) >= 0 {
			return errors.New("injected transient DLQ failure")
		}
	}
	return f.inner.PublishWithHeaders(ctx, topic, key, value, headers)
}

func (f *faultInjectingProducer) PublishJSON(ctx context.Context, topic string, key string, data interface{}) error {
	return f.inner.PublishJSON(ctx, topic, key, data)
}

func (f *faultInjectingProducer) Close() error { return nil }

func TestIT01_BasicProduceConsumeJSONHeadersAndTracing(t *testing.T) {
	brokers := integrationBrokers(t)
	topicA := uniqueName("it01-a")
	topicB := uniqueName("it01-b")
	createTestTopic(t, brokers, topicA, 1)
	createTestTopic(t, brokers, topicB, 1)

	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	defer func() { _ = tp.Shutdown(context.Background()) }()
	tCfg := kafka.TracingConfig{TracerProvider: tp, Propagator: propagation.TraceContext{}}

	prod, err := kafka.NewProducer(brokerCfgFor(brokers), kafka.DefaultProducerConfig(), kafka.WithProducerTracing(tCfg))
	if err != nil {
		t.Fatal(err)
	}
	defer prod.Close()

	ctx := context.Background()
	if err := prod.PublishWithHeaders(ctx, topicA, []byte("k1"), []byte("v1"), []kafka.RecordHeader{{Key: "x-hdr", Value: []byte("abc")}}); err != nil {
		t.Fatalf("PublishWithHeaders: %v", err)
	}
	if err := prod.PublishJSON(ctx, topicB, "k2", map[string]string{"hello": "world"}); err != nil {
		t.Fatalf("PublishJSON: %v", err)
	}

	// Verify cancelled context error wrapping
	cctx, ccancel := context.WithCancel(ctx)
	ccancel()
	if err := prod.Publish(cctx, topicA, []byte("k"), []byte("v")); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	gc, err := kafka.NewConsumer(brokerCfgFor(brokers), consumerCfgFor(uniqueName("grp-it01")), testLogger(), kafka.WithConsumerTracing(tCfg))
	if err != nil {
		t.Fatal(err)
	}

	var gotA, gotB atomic.Bool
	if err := gc.Subscribe(kafka.Subscription{
		Topic:       topicA,
		RetryPolicy: kafka.DefaultRetryPolicy(),
		Handler: func(ctx context.Context, msg *kafka.Message) error {
			v, ok := msg.GetHeader("x-hdr")
			if string(msg.Key) == "k1" && string(msg.Value) == "v1" && ok && string(v) == "abc" {
				gotA.Store(true)
			}
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := gc.Subscribe(kafka.Subscription{
		Topic:       topicB,
		RetryPolicy: kafka.DefaultRetryPolicy(),
		Handler: func(ctx context.Context, msg *kafka.Message) error {
			if string(msg.Key) == "k2" && strings.Contains(string(msg.Value), `"hello":"world"`) {
				gotB.Store(true)
			}
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- gc.Run(runCtx) }()

	waitForCondition(t, 20*time.Second, "IT01 messages", func() bool {
		return gotA.Load() && gotB.Load()
	})
	cancel()
	if err := <-runDone; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(recorder.Ended()) < 4 {
		t.Fatalf("expected >=4 spans (2 publish + 2 consume), got %d", len(recorder.Ended()))
	}
}

func TestIT02_IT03_MultiTopicIsolationTransientRetryAndMetrics(t *testing.T) {
	brokers := integrationBrokers(t)
	topic1 := uniqueName("it02-t1")
	topic2 := uniqueName("it02-t2")
	createTestTopic(t, brokers, topic1, 1)
	createTestTopic(t, brokers, topic2, 1)

	reg := prometheus.NewRegistry()
	metrics, err := kafka.NewMetrics(reg)
	if err != nil {
		t.Fatal(err)
	}

	prod, err := kafka.NewProducer(brokerCfgFor(brokers), kafka.DefaultProducerConfig(), kafka.WithProducerMetrics(metrics))
	if err != nil {
		t.Fatal(err)
	}
	defer prod.Close()

	if err := prod.Publish(context.Background(), topic1, []byte("k1"), []byte("retry-me")); err != nil {
		t.Fatal(err)
	}
	if err := prod.Publish(context.Background(), topic2, []byte("k2"), []byte("ok")); err != nil {
		t.Fatal(err)
	}

	groupID := uniqueName("grp-it02")
	gc, err := kafka.NewConsumer(brokerCfgFor(brokers), consumerCfgFor(groupID), testLogger(), kafka.WithConsumerMetrics(metrics))
	if err != nil {
		t.Fatal(err)
	}

	var t1Attempts atomic.Int32
	var t1Done, t2Done atomic.Bool
	if err := gc.Subscribe(kafka.Subscription{
		Topic: topic1,
		RetryPolicy: &kafka.RetryPolicy{
			Strategy:     kafka.RetryStrategyLogAndIgnore,
			MaxRetries:   3,
			InitialDelay: 50 * time.Millisecond,
			MaxDelay:     200 * time.Millisecond,
			Multiplier:   2,
		},
		Handler: func(ctx context.Context, msg *kafka.Message) error {
			if t1Attempts.Add(1) < 3 {
				return errors.New("transient error")
			}
			t1Done.Store(true)
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := gc.Subscribe(kafka.Subscription{
		Topic:       topic2,
		RetryPolicy: &kafka.RetryPolicy{Strategy: kafka.RetryStrategyLogAndIgnore, MaxRetries: 0},
		Handler: func(ctx context.Context, msg *kafka.Message) error {
			t2Done.Store(true)
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- gc.Run(runCtx) }()

	waitForCondition(t, 20*time.Second, "IT02/IT03 processing and offset commit", func() bool {
		if !t1Done.Load() || !t2Done.Load() {
			return false
		}
		o1, ok1 := committedOffset(t, brokers, groupID, topic1, 0)
		o2, ok2 := committedOffset(t, brokers, groupID, topic2, 0)
		return ok1 && o1 == 1 && ok2 && o2 == 1
	})
	cancel()
	if err := <-runDone; err != nil {
		t.Fatalf("Run: %v", err)
	}

	if t1Attempts.Load() != 3 {
		t.Fatalf("expected 3 attempts on topic1, got %d", t1Attempts.Load())
	}
	// Verify via registry gather that processed and retried metrics are non-zero.
	fams, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, f := range fams {
		seen[f.GetName()] = true
	}
	if !seen["kafka_messages_processed_total"] || !seen["kafka_messages_retried_total"] {
		t.Fatalf("expected processed and retried metrics in registry, got %v", seen)
	}
}

func TestIT04_BoundedRetriesAndRealDLQ(t *testing.T) {
	brokers := integrationBrokers(t)
	srcTopic := uniqueName("it04-src")
	dlqTopic := uniqueName("it04-dlq")
	createTestTopic(t, brokers, srcTopic, 1)
	createTestTopic(t, brokers, dlqTopic, 1)

	prod, err := kafka.NewProducer(brokerCfgFor(brokers), kafka.DefaultProducerConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer prod.Close()

	if err := prod.PublishWithHeaders(context.Background(), srcTopic, []byte("bad-key"), []byte("bad-val"),
		[]kafka.RecordHeader{{Key: "x-trace-test", Value: []byte("preserve-me")}}); err != nil {
		t.Fatal(err)
	}

	srcGroup := uniqueName("grp-it04-src")
	gc, err := kafka.NewConsumer(brokerCfgFor(brokers), consumerCfgFor(srcGroup), testLogger())
	if err != nil {
		t.Fatal(err)
	}

	var attempts atomic.Int32
	if err := gc.Subscribe(kafka.Subscription{
		Topic: srcTopic,
		RetryPolicy: &kafka.RetryPolicy{
			Strategy:         kafka.RetryStrategyDLQ,
			MaxRetries:       2,
			InitialDelay:     20 * time.Millisecond,
			MaxDelay:         100 * time.Millisecond,
			Multiplier:       2,
			DLQTopic:         dlqTopic,
			DLQProducer:      prod,
			DLQFailurePolicy: kafka.DLQFailureStop,
		},
		Handler: func(ctx context.Context, msg *kafka.Message) error {
			attempts.Add(1)
			return errors.New("permanent validation failure")
		},
	}); err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- gc.Run(runCtx) }()

	waitForCondition(t, 20*time.Second, "source offset committed after DLQ", func() bool {
		off, ok := committedOffset(t, brokers, srcGroup, srcTopic, 0)
		return ok && off == 1
	})
	cancel()
	if err := <-runDone; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if attempts.Load() != 3 {
		t.Fatalf("expected 3 attempts (1 + MaxRetries=2), got %d", attempts.Load())
	}

	// Verify DLQ record content and metadata via a fresh consumer.
	dlqVerify, err := kafka.NewConsumer(brokerCfgFor(brokers), consumerCfgFor(uniqueName("grp-it04-dlq")), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	var dlqMsg *kafka.Message
	var dlqMu sync.Mutex
	dlqCtx, dlqCancel := context.WithCancel(context.Background())
	if err := dlqVerify.Subscribe(kafka.Subscription{
		Topic:       dlqTopic,
		RetryPolicy: kafka.DefaultRetryPolicy(),
		Handler: func(ctx context.Context, msg *kafka.Message) error {
			dlqMu.Lock()
			dlqMsg = msg
			dlqMu.Unlock()
			dlqCancel()
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := dlqVerify.Run(dlqCtx); err != nil {
		t.Fatalf("dlqVerify Run: %v", err)
	}

	if dlqMsg == nil || string(dlqMsg.Key) != "bad-key" || string(dlqMsg.Value) != "bad-val" {
		t.Fatalf("unexpected DLQ record: %+v", dlqMsg)
	}
	checkHdr := func(k, want string) {
		v, ok := dlqMsg.GetHeader(k)
		if !ok || string(v) != want {
			t.Fatalf("DLQ header %s = %q (present=%v), want %q", k, string(v), ok, want)
		}
	}
	checkHdr("x-trace-test", "preserve-me")
	checkHdr(kafka.HeaderOriginalTopic, srcTopic)
	checkHdr(kafka.HeaderOriginalPartition, "0")
	checkHdr(kafka.HeaderOriginalOffset, "0")
	checkHdr(kafka.HeaderOriginalConsumerGroup, srcGroup)
	checkHdr(kafka.HeaderOriginalAttempts, "3")
	checkHdr(kafka.HeaderOriginalError, "handler_error")
}

func TestIT05_DLQFailDiscardAndStopOffsetSafety(t *testing.T) {
	brokers := integrationBrokers(t)
	srcTopic := uniqueName("it05-src")
	dlqTopic := uniqueName("it05-dlq")
	createTestTopic(t, brokers, srcTopic, 1)
	createTestTopic(t, brokers, dlqTopic, 1)

	realProd, err := kafka.NewProducer(brokerCfgFor(brokers), kafka.DefaultProducerConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer realProd.Close()

	// Produce 2 messages: offset 0 (fails) and offset 1 (would succeed).
	for i := 0; i < 2; i++ {
		if err := realProd.Publish(context.Background(), srcTopic, []byte("k"), []byte(fmt.Sprintf("msg-%d", i))); err != nil {
			t.Fatal(err)
		}
	}

	faultProd := &faultInjectingProducer{inner: realProd}
	faultProd.failAll.Store(true)

	stopGroup := uniqueName("grp-it05-stop")
	cc := consumerCfgFor(stopGroup)
	cc.CommitMode = kafka.CommitSync
	gcStop, err := kafka.NewConsumer(brokerCfgFor(brokers), cc, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := gcStop.Subscribe(kafka.Subscription{
		Topic: srcTopic,
		RetryPolicy: &kafka.RetryPolicy{
			Strategy:         kafka.RetryStrategyDLQ,
			MaxRetries:       0,
			DLQTopic:         dlqTopic,
			DLQProducer:      faultProd,
			DLQFailurePolicy: kafka.DLQFailureStop,
		},
		Handler: func(ctx context.Context, msg *kafka.Message) error {
			if string(msg.Value) == "msg-0" {
				return errors.New("fail msg-0")
			}
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	if err := gcStop.Run(context.Background()); err == nil {
		t.Fatalf("expected terminal error from DLQFailureStop")
	}
	if off, ok := committedOffset(t, brokers, stopGroup, srcTopic, 0); ok && off > 0 {
		t.Fatalf("expected no committed offset past unresolved msg-0, got %d", off)
	}

	// Restart with same group and DLQ healed -> msg-0 and msg-1 both delivered (msg-0 dead-lettered, msg-1 processed).
	faultProd.failAll.Store(false)
	gcResume, err := kafka.NewConsumer(brokerCfgFor(brokers), cc, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	var mu sync.Mutex
	resCtx, resCancel := context.WithCancel(context.Background())
	if err := gcResume.Subscribe(kafka.Subscription{
		Topic: srcTopic,
		RetryPolicy: &kafka.RetryPolicy{
			Strategy:         kafka.RetryStrategyDLQ,
			MaxRetries:       0,
			DLQTopic:         dlqTopic,
			DLQProducer:      faultProd,
			DLQFailurePolicy: kafka.DLQFailureStop,
		},
		Handler: func(ctx context.Context, msg *kafka.Message) error {
			mu.Lock()
			seen = append(seen, string(msg.Value))
			mu.Unlock()
			if string(msg.Value) == "msg-0" {
				return errors.New("fail msg-0")
			}
			resCancel()
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := gcResume.Run(resCtx); err != nil {
		t.Fatalf("resume Run: %v", err)
	}
	if !slices.Equal(seen, []string{"msg-0", "msg-1"}) {
		t.Fatalf("expected replay of [msg-0 msg-1], got %v", seen)
	}

	// Verify DLQFailureDiscard advances past failure.
	faultProd.failAll.Store(true)
	discardGroup := uniqueName("grp-it05-discard")
	gcDiscard, err := kafka.NewConsumer(brokerCfgFor(brokers), consumerCfgFor(discardGroup), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	var msg1Processed atomic.Bool
	dCtx, dCancel := context.WithCancel(context.Background())
	if err := gcDiscard.Subscribe(kafka.Subscription{
		Topic: srcTopic,
		RetryPolicy: &kafka.RetryPolicy{
			Strategy:         kafka.RetryStrategyDLQ,
			MaxRetries:       0,
			DLQTopic:         dlqTopic,
			DLQProducer:      faultProd,
			DLQFailurePolicy: kafka.DLQFailureDiscard,
		},
		Handler: func(ctx context.Context, msg *kafka.Message) error {
			if string(msg.Value) == "msg-0" {
				return errors.New("fail msg-0")
			}
			msg1Processed.Store(true)
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	dDone := make(chan error, 1)
	go func() { dDone <- gcDiscard.Run(dCtx) }()
	waitForCondition(t, 20*time.Second, "discard continues to msg-1 and commits", func() bool {
		if !msg1Processed.Load() {
			return false
		}
		off, ok := committedOffset(t, brokers, discardGroup, srcTopic, 0)
		return ok && off == 2
	})
	dCancel()
	if err := <-dDone; err != nil {
		t.Fatalf("discard Run: %v", err)
	}
}

func TestIT05_DLQRetryStopsOnBrokerMessageSizeLimit(t *testing.T) {
	brokers := integrationBrokers(t)
	source := uniqueName("it05-size-source")
	dlq := uniqueName("it05-size-dlq")
	group := uniqueName("grp-it05-size")
	createTestTopic(t, brokers, source, 1)
	createTestTopic(t, brokers, dlq, 1)
	admin, err := sarama.NewClusterAdmin(brokers, sarama.NewConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := admin.Close(); err != nil {
			t.Errorf("close admin: %v", err)
		}
	}()
	limit := "1024"
	if err := admin.AlterConfig(sarama.TopicResource, dlq, map[string]*string{"max.message.bytes": &limit}, false); err != nil {
		t.Fatalf("set DLQ topic message limit: %v", err)
	}
	producerCfg := kafka.DefaultProducerConfig()
	producerCfg.Compression = "none"
	producer, err := kafka.NewProducer(brokerCfgFor(brokers), producerCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := producer.Close(); err != nil {
			t.Errorf("close producer: %v", err)
		}
	}()
	for _, payload := range []string{"resolved", strings.Repeat("x", 2048), "must remain unread"} {
		if err := producer.Publish(t.Context(), source, nil, []byte(payload)); err != nil {
			t.Fatalf("publish source record: %v", err)
		}
	}
	cfg := consumerCfgFor(group)
	cfg.CommitMode = kafka.CommitSync
	consumer, err := kafka.NewConsumer(brokerCfgFor(brokers), cfg, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := consumer.Close(); err != nil {
			t.Errorf("close consumer: %v", err)
		}
	}()
	handlerErr := errors.New("poison record")
	var handled []int64
	if err := consumer.Subscribe(kafka.Subscription{
		Topic: source,
		RetryPolicy: &kafka.RetryPolicy{Strategy: kafka.RetryStrategyDLQ, DLQTopic: dlq,
			DLQProducer: producer, DLQFailurePolicy: kafka.DLQFailureRetry},
		Handler: func(_ context.Context, msg *kafka.Message) error {
			handled = append(handled, msg.Offset)
			if msg.Offset == 1 {
				return handlerErr
			}
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	if err := consumer.Run(ctx); !errors.Is(err, handlerErr) || !errors.Is(err, sarama.ErrMessageSizeTooLarge) {
		t.Fatalf("expected handler failure and broker message-size rejection, got %v", err)
	}
	if !slices.Equal(handled, []int64{0, 1}) {
		t.Fatalf("handled offsets %v; want resolved offset 0 and poison offset 1", handled)
	}
	if offset, ok := committedOffset(t, brokers, group, source, 0); !ok || offset != 1 {
		t.Fatalf("committed offset = %d, present = %t; want 1 with the poison record unresolved", offset, ok)
	}
	if health := consumer.Health(); health.State != kafka.ConsumerStateFailed || health.LastError != "terminal: dlq_message_too_large" {
		t.Fatalf("unexpected terminal health %+v", health)
	}
}

func TestIT06_DLQRetryBlocksAffectedPartitionUntilRecovered(t *testing.T) {
	brokers := integrationBrokers(t)
	srcTopic := uniqueName("it06-src")
	dlqTopic := uniqueName("it06-dlq")
	createTestTopic(t, brokers, srcTopic, 2)
	createTestTopic(t, brokers, dlqTopic, 1)

	realProd, err := kafka.NewProducer(brokerCfgFor(brokers), kafka.DefaultProducerConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer realProd.Close()

	// Send one failing record and several succeeding records across keys so both partitions get work.
	sc := sarama.NewConfig()
	sc.Producer.Return.Successes = true
	rawProd, err := sarama.NewSyncProducer(brokers, sc)
	if err != nil {
		t.Fatal(err)
	}
	sc.Producer.Partitioner = sarama.NewManualPartitioner
	_ = rawProd.Close()

	// Use manual partitioner Sarama SyncProducer so partition 0 and partition 1 are deterministic.
	manProd, err := sarama.NewSyncProducer(brokers, sc)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = manProd.SendMessage(&sarama.ProducerMessage{Topic: srcTopic, Partition: 0, Value: sarama.StringEncoder("p0-fail")})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = manProd.SendMessage(&sarama.ProducerMessage{Topic: srcTopic, Partition: 0, Value: sarama.StringEncoder("p0-after")})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = manProd.SendMessage(&sarama.ProducerMessage{Topic: srcTopic, Partition: 1, Value: sarama.StringEncoder("p1-ok")})
	if err != nil {
		t.Fatal(err)
	}
	_ = manProd.Close()

	faultProd := &faultInjectingProducer{inner: realProd}
	faultProd.failAll.Store(true)

	groupID := uniqueName("grp-it06")
	gc, err := kafka.NewConsumer(brokerCfgFor(brokers), consumerCfgFor(groupID), testLogger())
	if err != nil {
		t.Fatal(err)
	}

	var p1Done, p0AfterDone atomic.Bool
	if err := gc.Subscribe(kafka.Subscription{
		Topic: srcTopic,
		RetryPolicy: &kafka.RetryPolicy{
			Strategy:         kafka.RetryStrategyDLQ,
			MaxRetries:       0,
			InitialDelay:     100 * time.Millisecond,
			MaxDelay:         200 * time.Millisecond,
			Multiplier:       1.5,
			DLQTopic:         dlqTopic,
			DLQProducer:      faultProd,
			DLQFailurePolicy: kafka.DLQFailureRetry,
		},
		Handler: func(ctx context.Context, msg *kafka.Message) error {
			switch string(msg.Value) {
			case "p0-fail":
				return errors.New("send me to dlq")
			case "p0-after":
				p0AfterDone.Store(true)
			case "p1-ok":
				p1Done.Store(true)
			}
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- gc.Run(runCtx) }()

	// Partition 1 must make progress while Partition 0 waits on DLQ retry!
	waitForCondition(t, 20*time.Second, "partition 1 processes while partition 0 retries DLQ", func() bool {
		return p1Done.Load()
	})
	if p0AfterDone.Load() {
		t.Fatalf("p0-after must not run before p0-fail DLQ succeeds")
	}
	if !gc.Health().Ready {
		t.Fatalf("expected consumer ready during partition DLQ retry")
	}

	// Heal DLQ -> p0-fail transfers and p0-after completes.
	faultProd.failAll.Store(false)
	waitForCondition(t, 20*time.Second, "p0-after completes after DLQ heals", func() bool {
		return p0AfterDone.Load()
	})
	cancel()
	if err := <-runDone; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestIT06_ShutdownDuringBlockedDLQPublication(t *testing.T) {
	brokers := integrationBrokers(t)
	sourceTopic := uniqueName("it06-blocked-dlq-source")
	dlqTopic := uniqueName("it06-blocked-dlq")
	groupID := uniqueName("grp-it06-blocked-dlq")
	createTestTopic(t, brokers, sourceTopic, 1)
	sourceProducer, err := kafka.NewProducer(brokerCfgFor(brokers), kafka.DefaultProducerConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer sourceProducer.Close()
	for range 3 {
		if err := sourceProducer.Publish(t.Context(), sourceTopic, nil, []byte("record")); err != nil {
			t.Fatal(err)
		}
	}

	// The real Sarama producer uses a separate TCP endpoint. Withhold Produce
	// responses while keeping metadata reachable so its network retry can run.
	dlqBroker := sarama.NewMockBroker(t, 1)
	defer dlqBroker.Close()
	responses := map[string]sarama.MockResponse{
		"ApiVersionsRequest": sarama.NewMockApiVersionsResponse(t).SetApiKeys([]sarama.ApiVersionsResponseKey{
			{ApiKey: 0, MinVersion: 0, MaxVersion: 8},  // Produce
			{ApiKey: 3, MinVersion: 0, MaxVersion: 10}, // Metadata
		}),
		"MetadataRequest": sarama.NewMockMetadataResponse(t).
			SetBroker(dlqBroker.Addr(), dlqBroker.BrokerID()).SetLeader(dlqTopic, 0, dlqBroker.BrokerID()),
	}
	dlqBroker.SetHandlerByMap(responses)
	dlqBrokerCfg := brokerCfgFor([]string{dlqBroker.Addr()})
	const producerReadTimeout = 750 * time.Millisecond
	dlqBrokerCfg.ReadTimeout = producerReadTimeout
	dlqBrokerCfg.DialTimeout = producerReadTimeout
	dlqBrokerCfg.WriteTimeout = producerReadTimeout
	producerCfg := kafka.DefaultProducerConfig()
	producerCfg.Idempotent = false
	producerCfg.MaxRetries = 1
	producerCfg.RetryBackoff = 10 * time.Millisecond
	producerCfg.Timeout = producerReadTimeout
	dlqProducer, err := kafka.NewProducer(dlqBrokerCfg, producerCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer dlqProducer.Close()
	produceRequests := func() int {
		count := 0
		for _, exchange := range dlqBroker.History() {
			if _, ok := exchange.Request.(*sarama.ProduceRequest); ok {
				count++
			}
		}
		return count
	}

	consumerCfg := consumerCfgFor(groupID)
	consumerCfg.CommitMode = kafka.CommitSync
	consumerCfg.ShutdownTimeout = 75 * time.Millisecond
	consumerCfg.CloseTimeout = 100 * time.Millisecond
	watchdog := &logSignal{contains: "consumer close teardown timeout exceeded", observed: make(chan struct{})}
	consumer, err := kafka.NewConsumer(brokerCfgFor(brokers), consumerCfg,
		slog.New(slog.NewTextHandler(watchdog, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer consumer.Close()
	var handlerCalls atomic.Int32
	if err := consumer.Subscribe(kafka.Subscription{
		Topic: sourceTopic,
		RetryPolicy: &kafka.RetryPolicy{
			Strategy:         kafka.RetryStrategyDLQ,
			DLQTopic:         dlqTopic,
			DLQProducer:      dlqProducer,
			DLQFailurePolicy: kafka.DLQFailureDiscard,
		},
		Handler: func(_ context.Context, msg *kafka.Message) error {
			handlerCalls.Add(1)
			if msg.Offset == 1 {
				return kafka.Permanent(errors.New("poison record"))
			}
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runDone := make(chan error, 1)
	go func() {
		var runErr error
		pprof.Do(ctx, pprof.Labels("kafka-test-consumer", groupID), func(ctx context.Context) {
			runErr = consumer.Run(ctx)
		})
		runDone <- runErr
	}()
	consumerGoroutinesPresent := func() bool {
		// Sarama's response receivers are covered as well as the consumer's
		// own goroutines. The producer was started outside this labeled scope.
		var profile strings.Builder
		if err := pprof.Lookup("goroutine").WriteTo(&profile, 1); err != nil {
			t.Fatal(err)
		}
		return strings.Contains(profile.String(), groupID)
	}
	waitForCondition(t, 15*time.Second, "blocked Produce request reaches DLQ broker", func() bool {
		return produceRequests() >= 1
	})
	cancelStart := time.Now()
	cancel()
	select {
	case <-watchdog.observed:
	case <-time.After(producerReadTimeout):
		t.Fatal("consumer teardown watchdog did not fire during blocked publication")
	}
	select {
	case err := <-runDone:
		t.Fatalf("Run returned before the producer exhausted its network retry: %v", err)
	default:
	}
	if !consumerGoroutinesPresent() {
		t.Fatal("expected labeled consumer goroutines during the blocked send")
	}
	select {
	case err := <-runDone:
		if err != nil && !strings.Contains(err.Error(), "consumer group close:") {
			t.Fatalf("Run returned an unexpected error after interrupted DLQ publication: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("consumer did not finish shutdown after the bounded producer operation")
	}
	shutdownDuration := time.Since(cancelStart)
	if got := produceRequests(); got != 1+producerCfg.MaxRetries {
		t.Fatalf("synchronous publisher made %d Produce attempts, want %d", got, 1+producerCfg.MaxRetries)
	}
	if got := handlerCalls.Load(); got != 2 {
		t.Fatalf("handled %d records, want 2 with the record after the poison message untouched", got)
	}
	if got, ok := committedOffset(t, brokers, groupID, sourceTopic, 0); !ok || got != 1 {
		t.Fatalf("committed offset = %d, present = %t; want 1 with the poison record unresolved", got, ok)
	}
	waitForCondition(t, 2*time.Second, "consumer goroutines and broker response receivers exit", func() bool {
		return !consumerGoroutinesPresent()
	})
	t.Logf("shutdown took %s with a %s drain and %s consumer teardown timeout", shutdownDuration, consumerCfg.ShutdownTimeout, consumerCfg.CloseTimeout)

	responses["ProduceRequest"] = sarama.NewMockProduceResponse(t)
	dlqBroker.SetHandlerByMap(responses)
	if err := dlqProducer.Publish(t.Context(), dlqTopic, nil, []byte("probe")); err != nil {
		t.Fatalf("shared producer should still be usable after consumer shutdown: %v", err)
	}
}

func TestIT07_RebalanceDuringRetryPreservesUnresolvedOffset(t *testing.T) {
	brokers := integrationBrokers(t)
	topic := uniqueName("it07-reb")
	createTestTopic(t, brokers, topic, 2)

	prod, err := kafka.NewProducer(brokerCfgFor(brokers), kafka.DefaultProducerConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer prod.Close()

	for i := 0; i < 4; i++ {
		if err := prod.Publish(context.Background(), topic, []byte(fmt.Sprintf("k-%d", i)), []byte(fmt.Sprintf("v-%d", i))); err != nil {
			t.Fatal(err)
		}
	}

	groupID := uniqueName("grp-it07")
	cCfg := consumerCfgFor(groupID)
	cCfg.SessionTimeout = 6 * time.Second
	cCfg.HeartbeatInterval = 1500 * time.Millisecond
	cCfg.RebalanceTimeout = 6 * time.Second

	gc1, err := kafka.NewConsumer(brokerCfgFor(brokers), cCfg, testLogger())
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	completed := map[string]int{}
	rebalanceTrigger := make(chan struct{})
	var triggerOnce sync.Once

	if err := gc1.Subscribe(kafka.Subscription{
		Topic: topic,
		RetryPolicy: &kafka.RetryPolicy{
			Strategy:     kafka.RetryStrategyInfinite,
			InitialDelay: 500 * time.Millisecond,
			MaxDelay:     500 * time.Millisecond,
			Multiplier:   1,
		},
		Handler: func(ctx context.Context, msg *kafka.Message) error {
			if string(msg.Value) == "v-0" {
				triggerOnce.Do(func() { close(rebalanceTrigger) })
				// Block/retry on gc1 until session cancels due to rebalance!
				<-ctx.Done()
				return ctx.Err()
			}
			mu.Lock()
			completed[string(msg.Value)]++
			mu.Unlock()
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	ctx1, cancel1 := context.WithCancel(context.Background())
	done1 := make(chan error, 1)
	go func() { done1 <- gc1.Run(ctx1) }()

	<-rebalanceTrigger

	// Join second member to trigger rebalance, then stop gc1 so gc2 takes over all partitions and completes v-0..v-3.
	gc2, err := kafka.NewConsumer(brokerCfgFor(brokers), cCfg, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := gc2.Subscribe(kafka.Subscription{
		Topic:       topic,
		RetryPolicy: kafka.DefaultRetryPolicy(),
		Handler: func(ctx context.Context, msg *kafka.Message) error {
			mu.Lock()
			completed[string(msg.Value)]++
			mu.Unlock()
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan error, 1)
	go func() { done2 <- gc2.Run(ctx2) }()

	// Wait briefly for rebalance then gracefully stop gc1 so v-0 is guaranteed to move to gc2 if it hadn't already.
	time.Sleep(1500 * time.Millisecond)
	cancel1()
	if err := <-done1; err != nil {
		t.Fatalf("gc1 Run: %v", err)
	}

	waitForCondition(t, 20*time.Second, "all 4 messages completed after rebalance", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(completed) == 4
	})
	cancel2()
	if err := <-done2; err != nil {
		t.Fatalf("gc2 Run: %v", err)
	}
}

func TestIT08_GracefulShutdownAndRestartPeriodicAndSync(t *testing.T) {
	brokers := integrationBrokers(t)
	for _, mode := range []kafka.CommitMode{kafka.CommitPeriodic, kafka.CommitSync} {
		t.Run(string(mode), func(t *testing.T) {
			topic := uniqueName("it08-" + string(mode))
			createTestTopic(t, brokers, topic, 2)

			sc := sarama.NewConfig()
			sc.Producer.Return.Successes = true
			sc.Producer.Partitioner = sarama.NewManualPartitioner
			rawProd, err := sarama.NewSyncProducer(brokers, sc)
			if err != nil {
				t.Fatal(err)
			}
			// Only partition 0 gets messages; partition 1 remains idle!
			_, _, err = rawProd.SendMessage(&sarama.ProducerMessage{Topic: topic, Partition: 0, Value: sarama.StringEncoder("m0")})
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = rawProd.SendMessage(&sarama.ProducerMessage{Topic: topic, Partition: 0, Value: sarama.StringEncoder("m1")})
			if err != nil {
				t.Fatal(err)
			}
			_ = rawProd.Close()

			groupID := uniqueName("grp-it08-" + string(mode))
			cc := consumerCfgFor(groupID)
			cc.CommitMode = mode
			cc.CommitInterval = 200 * time.Millisecond
			cc.ShutdownTimeout = 5 * time.Second

			gc1, err := kafka.NewConsumer(brokerCfgFor(brokers), cc, testLogger())
			if err != nil {
				t.Fatal(err)
			}

			m0Started := make(chan struct{})
			m0Release := make(chan struct{})
			var processed []string
			var mu sync.Mutex

			if err := gc1.Subscribe(kafka.Subscription{
				Topic:       topic,
				RetryPolicy: kafka.DefaultRetryPolicy(),
				Handler: func(ctx context.Context, msg *kafka.Message) error {
					if string(msg.Value) == "m0" {
						close(m0Started)
						<-m0Release
					}
					mu.Lock()
					processed = append(processed, string(msg.Value))
					mu.Unlock()
					return nil
				},
			}); err != nil {
				t.Fatal(err)
			}

			ctx1, cancel1 := context.WithCancel(context.Background())
			done1 := make(chan error, 1)
			go func() { done1 <- gc1.Run(ctx1) }()

			<-m0Started
			cancel1() // Shutdown while m0 is active and partition 1 is idle!
			time.Sleep(150 * time.Millisecond)
			close(m0Release)
			if err := <-done1; err != nil {
				t.Fatalf("gc1 Run: %v", err)
			}

			if !slices.Equal(processed, []string{"m0"}) {
				t.Fatalf("expected only m0 admitted/finished during drain, got %v", processed)
			}

			// Restart with same group ID -> m1 must be replayed/delivered, m0 already committed!
			gc2, err := kafka.NewConsumer(brokerCfgFor(brokers), cc, testLogger())
			if err != nil {
				t.Fatal(err)
			}
			var resumed []string
			ctx2, cancel2 := context.WithCancel(context.Background())
			if err := gc2.Subscribe(kafka.Subscription{
				Topic:       topic,
				RetryPolicy: kafka.DefaultRetryPolicy(),
				Handler: func(ctx context.Context, msg *kafka.Message) error {
					mu.Lock()
					resumed = append(resumed, string(msg.Value))
					mu.Unlock()
					cancel2()
					return nil
				},
			}); err != nil {
				t.Fatal(err)
			}
			if err := gc2.Run(ctx2); err != nil {
				t.Fatalf("gc2 Run: %v", err)
			}
			if !slices.Equal(resumed, []string{"m1"}) {
				t.Fatalf("expected m1 on restart (no skip, no m0 replay), got %v", resumed)
			}
		})
	}
}

func TestIT09_HybridMultipleGroupsIndependentDelivery(t *testing.T) {
	brokers := integrationBrokers(t)
	sharedTopic := uniqueName("it09-shared")
	onlyBTopic := uniqueName("it09-onlyb")
	createTestTopic(t, brokers, sharedTopic, 1)
	createTestTopic(t, brokers, onlyBTopic, 1)

	bCfg := brokerCfgFor(brokers)
	dlqProd, err := kafka.NewProducer(bCfg, kafka.DefaultProducerConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer dlqProd.Close()

	if err := dlqProd.Publish(context.Background(), sharedTopic, []byte("k"), []byte("shared-msg")); err != nil {
		t.Fatal(err)
	}
	if err := dlqProd.Publish(context.Background(), onlyBTopic, []byte("k"), []byte("onlyb-msg")); err != nil {
		t.Fatal(err)
	}

	gA, err := kafka.NewConsumer(bCfg, consumerCfgFor(uniqueName("grp-it09-a")), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	gB, err := kafka.NewConsumer(bCfg, consumerCfgFor(uniqueName("grp-it09-b")), testLogger())
	if err != nil {
		t.Fatal(err)
	}

	var gotAShared, gotBShared, gotBOnly atomic.Bool
	if err := gA.Subscribe(kafka.Subscription{
		Topic:       sharedTopic,
		RetryPolicy: kafka.DefaultRetryPolicy(),
		Handler: func(ctx context.Context, msg *kafka.Message) error {
			gotAShared.Store(true)
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := gB.Subscribe(kafka.Subscription{
		Topic:       sharedTopic,
		RetryPolicy: kafka.DefaultRetryPolicy(),
		Handler: func(ctx context.Context, msg *kafka.Message) error {
			gotBShared.Store(true)
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := gB.Subscribe(kafka.Subscription{
		Topic:       onlyBTopic,
		RetryPolicy: kafka.DefaultRetryPolicy(),
		Handler: func(ctx context.Context, msg *kafka.Message) error {
			gotBOnly.Store(true)
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	ctxA, cancelA := context.WithCancel(context.Background())
	ctxB, cancelB := context.WithCancel(context.Background())
	doneA := make(chan error, 1)
	doneB := make(chan error, 1)
	go func() { doneA <- gA.Run(ctxA) }()
	go func() { doneB <- gB.Run(ctxB) }()

	waitForCondition(t, 20*time.Second, "both groups receive independent deliveries", func() bool {
		return gotAShared.Load() && gotBShared.Load() && gotBOnly.Load()
	})
	cancelA()
	if err := <-doneA; err != nil {
		t.Fatalf("gA Run: %v", err)
	}
	// Shared producer must still be usable after group A stops!
	if err := dlqProd.Publish(context.Background(), onlyBTopic, []byte("k2"), []byte("still-open")); err != nil {
		t.Fatalf("shared producer closed unexpectedly after gA stopped: %v", err)
	}
	cancelB()
	if err := <-doneB; err != nil {
		t.Fatalf("gB Run: %v", err)
	}
}

func TestIT10_MultipleReplicasAndAssignors(t *testing.T) {
	brokers := integrationBrokers(t)
	for _, strategy := range []string{"roundrobin", "sticky", "range"} {
		t.Run(strategy, func(t *testing.T) {
			topic := uniqueName("it10-" + strategy)
			createTestTopic(t, brokers, topic, 4)

			prodCfg := kafka.DefaultProducerConfig()
			prodCfg.MaxRetries = 10
			prodCfg.RetryBackoff = 200 * time.Millisecond
			prod, err := kafka.NewProducer(brokerCfgFor(brokers), prodCfg)
			if err != nil {
				t.Fatal(err)
			}
			defer prod.Close()

			const total = 12
			for i := 0; i < total; i++ {
				if err := prod.Publish(context.Background(), topic, []byte(fmt.Sprintf("k-%d", i)), []byte(fmt.Sprintf("v-%d", i))); err != nil {
					t.Fatal(err)
				}
			}

			groupID := uniqueName("grp-it10-" + strategy)
			cc := consumerCfgFor(groupID)
			cc.BalanceStrategy = strategy
			cc.SessionTimeout = 6 * time.Second
			cc.HeartbeatInterval = 1500 * time.Millisecond

			var mu sync.Mutex
			seen := map[string]int{}
			partSeq := map[int32]int64{}

			makeMember := func() kafka.Consumer {
				gc, err := kafka.NewConsumer(brokerCfgFor(brokers), cc, testLogger())
				if err != nil {
					t.Fatal(err)
				}
				if err := gc.Subscribe(kafka.Subscription{
					Topic:       topic,
					RetryPolicy: kafka.DefaultRetryPolicy(),
					Handler: func(ctx context.Context, msg *kafka.Message) error {
						mu.Lock()
						defer mu.Unlock()
						seen[string(msg.Value)]++
						if prev, ok := partSeq[msg.Partition]; ok && msg.Offset != prev+1 {
							t.Errorf("non-sequential offset on partition %d: prev=%d cur=%d", msg.Partition, prev, msg.Offset)
						}
						partSeq[msg.Partition] = msg.Offset
						return nil
					},
				}); err != nil {
					t.Fatal(err)
				}
				return gc
			}

			m1 := makeMember()
			m2 := makeMember()
			ctx, cancel := context.WithCancel(context.Background())
			d1 := make(chan error, 1)
			d2 := make(chan error, 1)
			go func() { d1 <- m1.Run(ctx) }()
			go func() { d2 <- m2.Run(ctx) }()

			waitForCondition(t, 25*time.Second, "all messages consumed by 2 replicas", func() bool {
				mu.Lock()
				defer mu.Unlock()
				return len(seen) == total
			})
			cancel()
			if err := <-d1; err != nil {
				t.Fatalf("m1 Run: %v", err)
			}
			if err := <-d2; err != nil {
				t.Fatalf("m2 Run: %v", err)
			}
		})
	}
}

func TestIT11_BrokerOutageAndRestoration(t *testing.T) {
	container := strings.TrimSpace(os.Getenv("KAFKA_TEST_CONTAINER"))
	if container == "" {
		t.Skip("skipping outage test: set KAFKA_TEST_CONTAINER to the Docker Kafka container name to test live pause/unpause")
	}

	brokers := integrationBrokers(t)
	topic := uniqueName("it11-outage")
	createTestTopic(t, brokers, topic, 1)

	bCfg := brokerCfgFor(brokers)
	bCfg.DialTimeout = time.Second
	bCfg.ReadTimeout = time.Second
	bCfg.WriteTimeout = time.Second

	cCfg := consumerCfgFor(uniqueName("grp-it11"))
	cCfg.SessionTimeout = 6 * time.Second
	cCfg.HeartbeatInterval = 1500 * time.Millisecond
	cCfg.RebalanceTimeout = 6 * time.Second
	cCfg.ShutdownTimeout = 2 * time.Second

	prod, err := kafka.NewProducer(bCfg, kafka.DefaultProducerConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer prod.Close()

	gc, err := kafka.NewConsumer(bCfg, cCfg, testLogger())
	if err != nil {
		t.Fatal(err)
	}

	var gotBefore, gotAfter atomic.Bool
	if err := gc.Subscribe(kafka.Subscription{
		Topic:       topic,
		RetryPolicy: kafka.DefaultRetryPolicy(),
		Handler: func(ctx context.Context, msg *kafka.Message) error {
			switch string(msg.Value) {
			case "before":
				gotBefore.Store(true)
			case "after":
				gotAfter.Store(true)
			}
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- gc.Run(ctx) }()

	// Start a second consumer before the outage so it enters consumeLoop, then cancel it mid-outage.
	cancelOutageCfg := consumerCfgFor(uniqueName("grp-it11-cancel"))
	cancelOutageCfg.SessionTimeout = 6 * time.Second
	cancelOutageCfg.HeartbeatInterval = 1500 * time.Millisecond
	cancelOutageCfg.RebalanceTimeout = 6 * time.Second
	cancelOutageCfg.ShutdownTimeout = time.Second
	gcOutage, err := kafka.NewConsumer(bCfg, cancelOutageCfg, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := gcOutage.Subscribe(kafka.Subscription{
		Topic:       topic,
		RetryPolicy: kafka.DefaultRetryPolicy(),
		Handler:     func(context.Context, *kafka.Message) error { return nil },
	}); err != nil {
		t.Fatal(err)
	}
	outageCtx, outageCancel := context.WithCancel(context.Background())
	outageDone := make(chan error, 1)
	go func() { outageDone <- gcOutage.Run(outageCtx) }()

	if err := prod.Publish(context.Background(), topic, []byte("k"), []byte("before")); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, 15*time.Second, "before outage message and outage consumer ready", func() bool {
		return gotBefore.Load() && gcOutage.Health().Ready
	})

	// Always unpause the container on cleanup so a failure never leaves the broker wedged.
	var paused atomic.Bool
	t.Cleanup(func() {
		if paused.Load() {
			_ = exec.Command("docker", "unpause", container).Run()
		}
	})

	// Pause broker longer than ReadTimeout (1s) and SessionTimeout (6s) and observe session disruption.
	pauseStart := time.Now()
	if out, err := exec.Command("docker", "pause", container).CombinedOutput(); err != nil {
		t.Fatalf("docker pause %s: %v (%s)", container, err, string(out))
	}
	paused.Store(true)

	waitForCondition(t, 25*time.Second, "consumers observe outage error or session disruption", func() bool {
		h1 := gc.Health()
		h2 := gcOutage.Health()
		return (h1.LastError != "" || !h1.Ready) && (h2.LastError != "" || !h2.Ready)
	})

	// Cancel gcOutage mid-outage and verify prompt exit without retrying after stop.
	// Note: Sarama's release()/LeaveGroup network timeouts on a paused socket bound total cleanup
	// duration, and client.Close() may return a LeaveGroup network error while the broker is paused.
	cancelStart := time.Now()
	outageCancel()
	select {
	case err := <-outageDone:
		if err != nil && !strings.Contains(err.Error(), "consumer group close:") {
			t.Fatalf("outage cancel Run returned unexpected non-close error: %v", err)
		}
		t.Logf("outage cancel exited in %s after %s pause (err=%v)", time.Since(cancelStart), time.Since(pauseStart), err)
	case <-time.After(35 * time.Second):
		t.Fatalf("outage consumer did not exit within 35s upon cancellation")
	}
	if h := gcOutage.Health(); (h.State != kafka.ConsumerStateStopped && h.State != kafka.ConsumerStateFailed) || h.Ready {
		t.Fatalf("expected Stopped/Failed and unready after outage cancel, got %+v", h)
	}
	_ = gcOutage.Close()

	// Ensure total broker pause exceeds SessionTimeout (6s) before restoring the broker.
	if remaining := (cCfg.SessionTimeout + time.Second) - time.Since(pauseStart); remaining > 0 {
		time.Sleep(remaining)
	}
	t.Logf("restoring broker after %s pause", time.Since(pauseStart))

	// Restore broker and verify original consumer recovers and consumes new records.
	if out, err := exec.Command("docker", "unpause", container).CombinedOutput(); err != nil {
		t.Fatalf("docker unpause %s: %v (%s)", container, err, string(out))
	}
	paused.Store(false)

	waitForCondition(t, 30*time.Second, "publish & consume after restoration", func() bool {
		if !gotAfter.Load() {
			_ = prod.Publish(context.Background(), topic, []byte("k"), []byte("after"))
		}
		return gotAfter.Load()
	})

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run after outage recovery: %v", err)
	}
}

func TestIT12_GroupOffsetContinuityAndInitialOffsetReplay(t *testing.T) {
	brokers := integrationBrokers(t)
	topic := uniqueName("it12-continuity")
	createTestTopic(t, brokers, topic, 1)

	prod, err := kafka.NewProducer(brokerCfgFor(brokers), kafka.DefaultProducerConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer prod.Close()

	// 1. Existing committed offset remains authoritative even when InitialOffset="newest".
	if err := prod.Publish(context.Background(), topic, []byte("k1"), []byte("first")); err != nil {
		t.Fatal(err)
	}

	groupID := uniqueName("grp-it12")
	cc := consumerCfgFor(groupID)
	cc.CommitMode = kafka.CommitSync

	c1, err := kafka.NewConsumer(brokerCfgFor(brokers), cc, testLogger())
	if err != nil {
		t.Fatal(err)
	}

	ctx1, cancel1 := context.WithCancel(context.Background())
	var gotFirst atomic.Bool
	done1 := make(chan error, 1)
	if err := c1.Subscribe(kafka.Subscription{
		Topic:       topic,
		RetryPolicy: kafka.DefaultRetryPolicy(),
		Handler: func(ctx context.Context, msg *kafka.Message) error {
			if string(msg.Value) == "first" {
				gotFirst.Store(true)
			}
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	go func() { done1 <- c1.Run(ctx1) }()

	waitForCondition(t, 20*time.Second, "first consumer processes first message and commits", func() bool {
		if !gotFirst.Load() {
			return false
		}
		off, ok := committedOffset(t, brokers, groupID, topic, 0)
		return ok && off == 1
	})
	cancel1()
	if err := <-done1; err != nil {
		t.Fatalf("first consumer Run: %v", err)
	}

	if err := prod.Publish(context.Background(), topic, []byte("k2"), []byte("second")); err != nil {
		t.Fatal(err)
	}

	cc2 := consumerCfgFor(groupID)
	cc2.InitialOffset = "newest"
	c2, err := kafka.NewConsumer(brokerCfgFor(brokers), cc2, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	var values []string
	var mu sync.Mutex
	ctx2, cancel2 := context.WithCancel(context.Background())
	if err := c2.Subscribe(kafka.Subscription{
		Topic:       topic,
		RetryPolicy: kafka.DefaultRetryPolicy(),
		Handler: func(ctx context.Context, msg *kafka.Message) error {
			mu.Lock()
			values = append(values, string(msg.Value))
			mu.Unlock()
			cancel2()
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := c2.Run(ctx2); err != nil {
		t.Fatalf("second consumer Run: %v", err)
	}
	if !slices.Equal(values, []string{"second"}) {
		t.Fatalf("expected continuity from committed offset with only [second], got %v", values)
	}

	// 2. R07: Fresh group with NO committed offset yet: compare "newest" (skips pre-existing) vs "oldest" (replays).
	replayTopic := uniqueName("it12-replay")
	createTestTopic(t, brokers, replayTopic, 1)
	if err := prod.Publish(context.Background(), replayTopic, []byte("k0"), []byte("unresolved-uncommitted")); err != nil {
		t.Fatal(err)
	}

	newestGroup := uniqueName("grp-it12-newest")
	newestCfg := consumerCfgFor(newestGroup)
	newestCfg.InitialOffset = "newest"
	cNewest, err := kafka.NewConsumer(brokerCfgFor(brokers), newestCfg, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	var newestSeen []string
	var newestReady atomic.Bool
	nCtx, nCancel := context.WithCancel(context.Background())
	if err := cNewest.Subscribe(kafka.Subscription{
		Topic:       replayTopic,
		RetryPolicy: kafka.DefaultRetryPolicy(),
		Handler: func(ctx context.Context, msg *kafka.Message) error {
			mu.Lock()
			newestSeen = append(newestSeen, string(msg.Value))
			mu.Unlock()
			if string(msg.Value) == "after-newest-join" {
				nCancel()
			}
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	nDone := make(chan error, 1)
	go func() { nDone <- cNewest.Run(nCtx) }()
	waitForCondition(t, 15*time.Second, "newest consumer ready", func() bool {
		if cNewest.Health().Ready {
			newestReady.Store(true)
			return true
		}
		return false
	})
	if err := prod.Publish(context.Background(), replayTopic, []byte("k1"), []byte("after-newest-join")); err != nil {
		t.Fatal(err)
	}
	if err := <-nDone; err != nil {
		t.Fatalf("cNewest Run: %v", err)
	}
	if !newestReady.Load() || !slices.Equal(newestSeen, []string{"after-newest-join"}) {
		t.Fatalf("expected InitialOffset=newest without committed offset to skip earlier record, got %v", newestSeen)
	}

	oldestGroup := uniqueName("grp-it12-oldest")
	oldestCfg := consumerCfgFor(oldestGroup)
	oldestCfg.InitialOffset = "oldest"
	cOldest, err := kafka.NewConsumer(brokerCfgFor(brokers), oldestCfg, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	var oldestSeen []string
	oCtx, oCancel := context.WithCancel(context.Background())
	if err := cOldest.Subscribe(kafka.Subscription{
		Topic:       replayTopic,
		RetryPolicy: kafka.DefaultRetryPolicy(),
		Handler: func(ctx context.Context, msg *kafka.Message) error {
			mu.Lock()
			oldestSeen = append(oldestSeen, string(msg.Value))
			count := len(oldestSeen)
			mu.Unlock()
			if count == 2 {
				oCancel()
			}
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := cOldest.Run(oCtx); err != nil {
		t.Fatalf("cOldest Run: %v", err)
	}
	if !slices.Equal(oldestSeen, []string{"unresolved-uncommitted", "after-newest-join"}) {
		t.Fatalf("expected InitialOffset=oldest to replay historical records, got %v", oldestSeen)
	}
}

type logSignal struct {
	contains string
	observed chan struct{}
	once     sync.Once
}

func (l *logSignal) Write(p []byte) (int, error) {
	if strings.Contains(string(p), l.contains) {
		l.once.Do(func() { close(l.observed) })
	}
	return len(p), nil
}

// Install the Sarama logger before any broker goroutine can use it. Only the
// saturation test activates the observer.
var saturationLogTarget atomic.Pointer[logSignal]

type saturationLogWriter struct{}

func (saturationLogWriter) Write(p []byte) (int, error) {
	if target := saturationLogTarget.Load(); target != nil {
		return target.Write(p)
	}
	return len(p), nil
}

func init() {
	sarama.Logger = log.New(saturationLogWriter{}, "", 0)
}

func TestIT13_PartitionSaturationDoesNotStallOtherTopics(t *testing.T) {
	brokers := integrationBrokers(t)
	satTopic := uniqueName("it13-sat")
	fastTopic := uniqueName("it13-fast")
	createTestTopic(t, brokers, satTopic, 1)
	createTestTopic(t, brokers, fastTopic, 1)
	saturation := &logSignal{
		contains: "abandoned subscription to " + satTopic + "/0 because consuming was taking too long",
		observed: make(chan struct{}),
	}
	saturationLogTarget.Store(saturation)
	defer saturationLogTarget.Store(nil)

	reg := prometheus.NewRegistry()
	metrics, err := kafka.NewMetrics(reg)
	if err != nil {
		t.Fatal(err)
	}

	prodCfg := kafka.DefaultProducerConfig()
	prodCfg.Compression = "none"
	prod, err := kafka.NewProducer(brokerCfgFor(brokers), prodCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer prod.Close()

	// Use DefaultConsumerConfig (MaxProcessingTime=100ms per F02).
	cCfg := kafka.DefaultConsumerConfig()
	cCfg.GroupID = uniqueName("grp-it13")
	cCfg.InitialOffset = "oldest"
	cCfg.ShutdownTimeout = 2 * time.Second
	cCfg.CloseTimeout = 2 * time.Second
	if cCfg.MaxProcessingTime != 100*time.Millisecond {
		t.Fatalf("expected default MaxProcessingTime=100ms, got %s", cCfg.MaxProcessingTime)
	}

	gc, err := kafka.NewConsumer(brokerCfgFor(brokers), cCfg, testLogger(), kafka.WithConsumerMetrics(metrics))
	if err != nil {
		t.Fatal(err)
	}

	satBlocked := make(chan struct{})
	var satOnce sync.Once
	releaseSat := make(chan struct{})
	var satResolved atomic.Int32
	var fastCount atomic.Int32

	if err := gc.Subscribe(kafka.Subscription{
		Topic:       satTopic,
		RetryPolicy: &kafka.RetryPolicy{Strategy: kafka.RetryStrategyInfinite, InitialDelay: 50 * time.Millisecond, MaxDelay: 100 * time.Millisecond, Multiplier: 1.5},
		Handler: func(ctx context.Context, msg *kafka.Message) error {
			satOnce.Do(func() { close(satBlocked) })
			select {
			case <-releaseSat:
				satResolved.Add(1)
			case <-ctx.Done():
				return ctx.Err()
			}
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := gc.Subscribe(kafka.Subscription{
		Topic:       fastTopic,
		RetryPolicy: kafka.DefaultRetryPolicy(),
		Handler: func(ctx context.Context, msg *kafka.Message) error {
			fastCount.Add(1)
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer gc.Close()
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- gc.Run(ctx) }()

	waitForCondition(t, 15*time.Second, "IT13 consumer ready", func() bool {
		return gc.Health().Ready
	})

	// One record is held by the blocked handler. The rest must exceed the channel capacity.
	bufferSize := sarama.NewConfig().ChannelBufferSize
	const workers = 5
	recordsPerWorker := (bufferSize+1)/workers + 1
	var pubWg sync.WaitGroup
	results := make(chan error, workers*recordsPerWorker)
	for worker := range workers {
		pubWg.Go(func() {
			for i := range recordsPerWorker {
				results <- prod.Publish(ctx, satTopic, []byte("sat"), []byte(fmt.Sprintf("sat-%d-%d", worker, i)))
			}
		})
	}
	pubWg.Wait()
	close(results)
	published := 0
	for err := range results {
		if err != nil {
			t.Errorf("publish to saturated partition: %v", err)
		} else {
			published++
		}
	}
	if published != workers*recordsPerWorker || published-1 <= bufferSize {
		t.Fatalf("published %d records, one in flight and channel capacity %d", published, bufferSize)
	}

	select {
	case <-satBlocked:
	case <-time.After(15 * time.Second):
		t.Fatalf("satTopic handler did not start blocking")
	}

	select {
	case <-saturation.observed:
	case <-time.After(5 * time.Second):
		t.Fatal("Sarama did not report backpressure on the blocked partition")
	}

	// Now publish 20 messages to fastTopic; they must make prompt progress despite satTopic saturation.
	const fastRecords = 20
	for i := 0; i < fastRecords; i++ {
		if err := prod.Publish(context.Background(), fastTopic, []byte(fmt.Sprintf("fk-%d", i)), []byte("fast-ok")); err != nil {
			t.Fatal(err)
		}
	}

	fastStart := time.Now()
	waitForCondition(t, 5*time.Second, "fastTopic completes while satTopic buffer is saturated", func() bool {
		return fastCount.Load() == fastRecords
	})
	if got := satResolved.Load(); got != 0 {
		t.Fatalf("blocked partition resolved %d records before fast topic finished", got)
	}
	t.Logf("fastTopic processed %d records in %s while %s was saturated", fastRecords, time.Since(fastStart), satTopic)

	// Verify observed in-process backlog and updated timestamp metrics were populated for satTopic.
	fams, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var sawBacklog, sawUpdated bool
	for _, f := range fams {
		switch f.GetName() {
		case "kafka_consumer_in_process_backlog":
			if len(f.GetMetric()) > 0 {
				sawBacklog = true
			}
		case "kafka_consumer_backlog_updated_timestamp_seconds":
			if len(f.GetMetric()) > 0 {
				sawUpdated = true
			}
		}
	}
	if !sawBacklog || !sawUpdated {
		t.Fatalf("expected backlog and updated timestamp metrics, got sawBacklog=%v sawUpdated=%v", sawBacklog, sawUpdated)
	}

	close(releaseSat)
	cancel()
	if err := <-runDone; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestIT14_ReadinessClaimInitAndNewestStartupRace(t *testing.T) {
	brokers := integrationBrokers(t)
	topic := uniqueName("it14-ready")
	createTestTopic(t, brokers, topic, 4)

	prod, err := kafka.NewProducer(brokerCfgFor(brokers), kafka.DefaultProducerConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer prod.Close()

	// Publish pre-existing record that "newest" must skip once ready.
	if err := prod.Publish(context.Background(), topic, []byte("pre"), []byte("pre-ready")); err != nil {
		t.Fatal(err)
	}

	cCfg := consumerCfgFor(uniqueName("grp-it14"))
	cCfg.InitialOffset = "newest"
	gc, err := kafka.NewConsumer(brokerCfgFor(brokers), cCfg, testLogger())
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	seen := map[string]bool{}
	if err := gc.Subscribe(kafka.Subscription{
		Topic:       topic,
		RetryPolicy: kafka.DefaultRetryPolicy(),
		Handler: func(ctx context.Context, msg *kafka.Message) error {
			mu.Lock()
			seen[string(msg.Value)] = true
			mu.Unlock()
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- gc.Run(ctx) }()

	waitForCondition(t, 20*time.Second, "consumer Ready after all 4 claims initialized", func() bool {
		h := gc.Health()
		return h.Ready && h.AssignedClaims == 4
	})

	// Immediately publish across all partitions after Ready=true; none must be missed.
	const afterCount = 20
	for i := 0; i < afterCount; i++ {
		if err := prod.Publish(context.Background(), topic, []byte(fmt.Sprintf("k-%d", i)), []byte(fmt.Sprintf("after-%d", i))); err != nil {
			t.Fatal(err)
		}
	}

	waitForCondition(t, 15*time.Second, "all post-ready messages received without startup race", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for i := 0; i < afterCount; i++ {
			if !seen[fmt.Sprintf("after-%d", i)] {
				return false
			}
		}
		return true
	})
	mu.Lock()
	sawPre := seen["pre-ready"]
	mu.Unlock()
	if sawPre {
		t.Fatalf("InitialOffset=newest should have skipped pre-ready message")
	}

	cancel()
	if err := <-runDone; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestIT15_InvalidOffsetRecoveryAndFailClosed(t *testing.T) {
	brokers := integrationBrokers(t)
	topic := uniqueName("it15-invalid-off")
	createTestTopic(t, brokers, topic, 1)

	prod, err := kafka.NewProducer(brokerCfgFor(brokers), kafka.DefaultProducerConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer prod.Close()

	for i := 0; i < 5; i++ {
		if err := prod.Publish(context.Background(), topic, []byte("k"), []byte(fmt.Sprintf("m-%d", i))); err != nil {
			t.Fatal(err)
		}
	}

	groupID := uniqueName("grp-it15")
	cc1 := consumerCfgFor(groupID)
	cc1.CommitMode = kafka.CommitSync
	c1, err := kafka.NewConsumer(brokerCfgFor(brokers), cc1, testLogger())
	if err != nil {
		t.Fatal(err)
	}

	ctx1, cancel1 := context.WithCancel(context.Background())
	var m0Done atomic.Bool
	if err := c1.Subscribe(kafka.Subscription{
		Topic:       topic,
		RetryPolicy: kafka.DefaultRetryPolicy(),
		Handler: func(ctx context.Context, msg *kafka.Message) error {
			if string(msg.Value) == "m-0" {
				m0Done.Store(true)
			}
			cancel1()
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := c1.Run(ctx1); err != nil {
		t.Fatalf("c1 Run: %v", err)
	}
	if !m0Done.Load() {
		t.Fatalf("expected m-0 committed")
	}

	// Truncate log so committed offset (1) falls before LogStartOffset (3).
	admin, err := sarama.NewClusterAdmin(brokers, sarama.NewConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.DeleteRecords(topic, map[int32]int64{0: 3}); err != nil {
		_ = admin.Close()
		t.Fatalf("DeleteRecords: %v", err)
	}
	_ = admin.Close()

	// 1. With ResetInvalidOffsets=false, consumer must fail observably with ErrOffsetOutOfRange.
	ccFail := consumerCfgFor(groupID)
	ccFail.ResetInvalidOffsets = false
	cFail, err := kafka.NewConsumer(brokerCfgFor(brokers), ccFail, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := cFail.Subscribe(kafka.Subscription{
		Topic:       topic,
		RetryPolicy: kafka.DefaultRetryPolicy(),
		Handler:     func(context.Context, *kafka.Message) error { return nil },
	}); err != nil {
		t.Fatal(err)
	}

	failCtx, failCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer failCancel()
	runErr := cFail.Run(failCtx)
	if !errors.Is(runErr, sarama.ErrOffsetOutOfRange) {
		t.Fatalf("expected ErrOffsetOutOfRange with ResetInvalidOffsets=false, got %v", runErr)
	}
	if h := cFail.Health(); h.State != kafka.ConsumerStateFailed || h.Ready {
		t.Fatalf("expected Failed/unready health, got %+v", h)
	}

	// 2. With ResetInvalidOffsets=true and InitialOffset=oldest, consumer resets to LogStartOffset=3 and reads m-3, m-4.
	ccReset := consumerCfgFor(groupID)
	ccReset.ResetInvalidOffsets = true
	ccReset.InitialOffset = "oldest"
	cReset, err := kafka.NewConsumer(brokerCfgFor(brokers), ccReset, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	var recovered []string
	var mu sync.Mutex
	resCtx, resCancel := context.WithCancel(context.Background())
	if err := cReset.Subscribe(kafka.Subscription{
		Topic:       topic,
		RetryPolicy: kafka.DefaultRetryPolicy(),
		Handler: func(ctx context.Context, msg *kafka.Message) error {
			mu.Lock()
			recovered = append(recovered, string(msg.Value))
			n := len(recovered)
			mu.Unlock()
			if n == 2 {
				resCancel()
			}
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := cReset.Run(resCtx); err != nil {
		t.Fatalf("cReset Run: %v", err)
	}
	if !slices.Equal(recovered, []string{"m-3", "m-4"}) {
		t.Fatalf("expected [m-3 m-4] after oldest reset, got %v", recovered)
	}

	// 3. Interrupted first message on fresh group with default InitialOffset (oldest) replays without skipping.
	freshTopic := uniqueName("it15-fresh")
	createTestTopic(t, brokers, freshTopic, 1)
	if err := prod.Publish(context.Background(), freshTopic, []byte("k0"), []byte("first-event")); err != nil {
		t.Fatal(err)
	}
	freshGroup := uniqueName("grp-it15-fresh")
	freshCfg := kafka.DefaultConsumerConfig()
	freshCfg.GroupID = freshGroup
	freshCfg.CommitInterval = 200 * time.Millisecond
	freshCfg.ShutdownTimeout = 200 * time.Millisecond

	cInt, err := kafka.NewConsumer(brokerCfgFor(brokers), freshCfg, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	intStarted := make(chan struct{})
	intCtx, intCancel := context.WithCancel(context.Background())
	if err := cInt.Subscribe(kafka.Subscription{
		Topic:       freshTopic,
		RetryPolicy: &kafka.RetryPolicy{Strategy: kafka.RetryStrategyInfinite, InitialDelay: 50 * time.Millisecond, MaxDelay: 100 * time.Millisecond, Multiplier: 1.5},
		Handler: func(ctx context.Context, msg *kafka.Message) error {
			select {
			case <-intStarted:
			default:
				close(intStarted)
			}
			<-ctx.Done()
			return ctx.Err()
		},
	}); err != nil {
		t.Fatal(err)
	}
	intDone := make(chan error, 1)
	go func() { intDone <- cInt.Run(intCtx) }()
	<-intStarted
	intCancel()
	if err := <-intDone; err != nil {
		t.Fatalf("cInt Run: %v", err)
	}

	cReplay, err := kafka.NewConsumer(brokerCfgFor(brokers), freshCfg, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	var replayed string
	repCtx, repCancel := context.WithCancel(context.Background())
	if err := cReplay.Subscribe(kafka.Subscription{
		Topic:       freshTopic,
		RetryPolicy: kafka.DefaultRetryPolicy(),
		Handler: func(ctx context.Context, msg *kafka.Message) error {
			replayed = string(msg.Value)
			repCancel()
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := cReplay.Run(repCtx); err != nil {
		t.Fatalf("cReplay Run: %v", err)
	}
	if replayed != "first-event" {
		t.Fatalf("expected interrupted first-event to replay under default oldest, got %q", replayed)
	}
}

func TestIT15_InvalidOffsetAboveLogEndCommitsAndResumes(t *testing.T) {
	brokers := integrationBrokers(t)
	topic := uniqueName("it15-above-log-end")
	groupID := uniqueName("grp-it15-above-log-end")
	createTestTopic(t, brokers, topic, 1)

	producer, err := kafka.NewProducer(brokerCfgFor(brokers), kafka.DefaultProducerConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer producer.Close()
	for range 5 {
		if err := producer.Publish(context.Background(), topic, nil, []byte("record")); err != nil {
			t.Fatal(err)
		}
	}

	client, err := sarama.NewClient(brokers, sarama.NewConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if end, err := client.GetOffset(topic, 0, sarama.OffsetNewest); err != nil || end != 5 {
		t.Fatalf("log end = %d, err = %v; want 5", end, err)
	}
	coordinator, err := client.Coordinator(groupID)
	if err != nil {
		t.Fatal(err)
	}
	const invalidOffset = 1000
	request := &sarama.OffsetCommitRequest{Version: 2, ConsumerGroup: groupID, ConsumerGroupGeneration: -1}
	request.AddBlock(topic, 0, invalidOffset, 0, "")
	response, err := coordinator.CommitOffset(request)
	if err != nil {
		t.Fatal(err)
	}
	if code, ok := response.Errors[topic][0]; !ok || code != sarama.ErrNoError {
		t.Fatalf("seeding invalid offset: %+v", response.Errors)
	}

	checkOffset := func(want int64) {
		t.Helper()
		if got, ok := committedOffset(t, brokers, groupID, topic, 0); !ok || got != want {
			t.Fatalf("committed offset = %d, present = %t; want %d", got, ok, want)
		}
	}
	checkOffset(invalidOffset)

	var unresolved = errors.New("unresolved record")
	run := func(reset bool, stopAt int64) ([]int64, error) {
		t.Helper()
		cfg := consumerCfgFor(groupID)
		cfg.CommitMode = kafka.CommitSync
		cfg.ResetInvalidOffsets = reset
		consumer, err := kafka.NewConsumer(brokerCfgFor(brokers), cfg, testLogger())
		if err != nil {
			t.Fatal(err)
		}
		defer consumer.Close()
		var received []int64
		if err := consumer.Subscribe(kafka.Subscription{
			Topic:       topic,
			RetryPolicy: &kafka.RetryPolicy{Strategy: kafka.RetryStrategyStop},
			Handler: func(_ context.Context, msg *kafka.Message) error {
				received = append(received, msg.Offset)
				if msg.Offset == stopAt {
					return unresolved
				}
				return nil
			},
		}); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		err = consumer.Run(ctx)
		return received, err
	}

	if received, err := run(false, 0); !errors.Is(err, sarama.ErrOffsetOutOfRange) || len(received) != 0 {
		t.Fatalf("fail-closed run received %v, err %v; want no delivery and ErrOffsetOutOfRange", received, err)
	}
	checkOffset(invalidOffset)
	if received, err := run(true, 0); !errors.Is(err, unresolved) || !slices.Equal(received, []int64{0}) {
		t.Fatalf("unresolved recovery received %v, err %v; want [0] and handler error", received, err)
	}
	checkOffset(invalidOffset)
	if received, err := run(true, 2); !errors.Is(err, unresolved) || !slices.Equal(received, []int64{0, 1, 2}) {
		t.Fatalf("recovery received %v, err %v; want [0 1 2] and handler error", received, err)
	}
	checkOffset(2)
	if received, err := run(true, 4); !errors.Is(err, unresolved) || !slices.Equal(received, []int64{2, 3, 4}) {
		t.Fatalf("restart received %v, err %v; want [2 3 4] and handler error", received, err)
	}
	checkOffset(4)
}

func TestIT16_RetryStopAndPermanentErrorIntegration(t *testing.T) {
	brokers := integrationBrokers(t)
	topic := uniqueName("it16-stop")
	createTestTopic(t, brokers, topic, 1)

	prod, err := kafka.NewProducer(brokerCfgFor(brokers), kafka.DefaultProducerConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer prod.Close()

	if err := prod.Publish(context.Background(), topic, []byte("k"), []byte("m-0")); err != nil {
		t.Fatal(err)
	}
	if err := prod.Publish(context.Background(), topic, []byte("k"), []byte("m-1")); err != nil {
		t.Fatal(err)
	}

	groupID := uniqueName("grp-it16-stop")
	cc := consumerCfgFor(groupID)
	cc.CommitMode = kafka.CommitSync

	gc, err := kafka.NewConsumer(brokerCfgFor(brokers), cc, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	var attempts atomic.Int32
	var sawM1 atomic.Bool
	sentinel := errors.New("db outage")
	if err := gc.Subscribe(kafka.Subscription{
		Topic: topic,
		RetryPolicy: &kafka.RetryPolicy{
			Strategy:     kafka.RetryStrategyStop,
			MaxRetries:   2,
			InitialDelay: 20 * time.Millisecond,
			MaxDelay:     50 * time.Millisecond,
			Multiplier:   1.5,
		},
		Handler: func(ctx context.Context, msg *kafka.Message) error {
			if string(msg.Value) == "m-1" {
				sawM1.Store(true)
			}
			attempts.Add(1)
			return sentinel
		},
	}); err != nil {
		t.Fatal(err)
	}
	runErr := gc.Run(context.Background())
	if !errors.Is(runErr, sentinel) {
		t.Fatalf("expected wrapped sentinel from RetryStrategyStop, got %v", runErr)
	}
	if attempts.Load() != 3 || sawM1.Load() {
		t.Fatalf("expected 3 attempts on m-0 and m-1 untouched, got attempts=%d sawM1=%v", attempts.Load(), sawM1.Load())
	}
	if off, ok := committedOffset(t, brokers, groupID, topic, 0); ok && off > 0 {
		t.Fatalf("expected no committed offset on stopped failure, got %d", off)
	}

	// Permanent error under Infinite also stops immediately without marking.
	gcPerm, err := kafka.NewConsumer(brokerCfgFor(brokers), cc, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	var permAttempts atomic.Int32
	permErr := errors.New("bad schema")
	if err := gcPerm.Subscribe(kafka.Subscription{
		Topic:       topic,
		RetryPolicy: &kafka.RetryPolicy{Strategy: kafka.RetryStrategyInfinite},
		Handler: func(ctx context.Context, msg *kafka.Message) error {
			permAttempts.Add(1)
			return kafka.Permanent(permErr)
		},
	}); err != nil {
		t.Fatal(err)
	}
	pRunErr := gcPerm.Run(context.Background())
	if !errors.Is(pRunErr, permErr) || permAttempts.Load() != 1 {
		t.Fatalf("expected immediate stop on Permanent error with 1 attempt, got attempts=%d err=%v", permAttempts.Load(), pRunErr)
	}
}

func TestIT17_TransactionalConsumerIsolation(t *testing.T) {
	brokers := integrationBrokers(t)
	topic := uniqueName("it17-txn")
	createTestTopic(t, brokers, topic, 1)

	sc := sarama.NewConfig()
	sc.Producer.Idempotent = true
	sc.Producer.RequiredAcks = sarama.WaitForAll
	sc.Producer.Retry.Max = 5
	sc.Net.MaxOpenRequests = 1
	sc.Producer.Return.Successes = true
	sc.Producer.Return.Errors = true
	sc.Producer.Transaction.ID = uniqueName("txn-prod")

	ap, err := sarama.NewAsyncProducer(brokers, sc)
	if err != nil {
		t.Fatalf("NewAsyncProducer transactional: %v", err)
	}
	defer ap.Close()

	sendTxnRecord := func(val string, commit bool) {
		t.Helper()
		if err := ap.BeginTxn(); err != nil {
			t.Fatalf("BeginTxn: %v", err)
		}
		ap.Input() <- &sarama.ProducerMessage{
			Topic: topic,
			Key:   sarama.StringEncoder("k"),
			Value: sarama.StringEncoder(val),
		}
		select {
		case <-ap.Successes():
		case err := <-ap.Errors():
			t.Fatalf("txn send error: %v", err)
		case <-time.After(10 * time.Second):
			t.Fatalf("timed out waiting for txn send ack")
		}
		if commit {
			if err := ap.CommitTxn(); err != nil {
				t.Fatalf("CommitTxn: %v", err)
			}
		} else {
			if err := ap.AbortTxn(); err != nil {
				t.Fatalf("AbortTxn: %v", err)
			}
		}
	}

	sendTxnRecord("committed-1", true)
	sendTxnRecord("aborted-1", false)
	sendTxnRecord("committed-2", true)

	// 1. read_committed hides aborted-1 and delivers committed-1, committed-2.
	ccRC := consumerCfgFor(uniqueName("grp-it17-rc"))
	ccRC.IsolationLevel = kafka.IsolationReadCommitted
	cRC, err := kafka.NewConsumer(brokerCfgFor(brokers), ccRC, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	var rcVals []string
	var mu sync.Mutex
	rcCtx, rcCancel := context.WithCancel(context.Background())
	if err := cRC.Subscribe(kafka.Subscription{
		Topic:       topic,
		RetryPolicy: kafka.DefaultRetryPolicy(),
		Handler: func(ctx context.Context, msg *kafka.Message) error {
			mu.Lock()
			rcVals = append(rcVals, string(msg.Value))
			done := string(msg.Value) == "committed-2"
			mu.Unlock()
			if done {
				rcCancel()
			}
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := cRC.Run(rcCtx); err != nil {
		t.Fatalf("cRC Run: %v", err)
	}
	if !slices.Equal(rcVals, []string{"committed-1", "committed-2"}) {
		t.Fatalf("read_committed expected [committed-1 committed-2], got %v", rcVals)
	}

	// 2. read_uncommitted sees aborted-1 as well.
	ccRU := consumerCfgFor(uniqueName("grp-it17-ru"))
	ccRU.IsolationLevel = kafka.IsolationReadUncommitted
	cRU, err := kafka.NewConsumer(brokerCfgFor(brokers), ccRU, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	var ruVals []string
	ruCtx, ruCancel := context.WithCancel(context.Background())
	if err := cRU.Subscribe(kafka.Subscription{
		Topic:       topic,
		RetryPolicy: kafka.DefaultRetryPolicy(),
		Handler: func(ctx context.Context, msg *kafka.Message) error {
			mu.Lock()
			ruVals = append(ruVals, string(msg.Value))
			done := string(msg.Value) == "committed-2"
			mu.Unlock()
			if done {
				ruCancel()
			}
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := cRU.Run(ruCtx); err != nil {
		t.Fatalf("cRU Run: %v", err)
	}
	if !slices.Equal(ruVals, []string{"committed-1", "aborted-1", "committed-2"}) {
		t.Fatalf("read_uncommitted expected [committed-1 aborted-1 committed-2], got %v", ruVals)
	}
}

func TestIT18_ProducerPartitionersAndSustainedMultiTopicTraffic(t *testing.T) {
	brokers := integrationBrokers(t)

	// 1. Verify CRC32 (librdkafka-compatible) and Murmur2 partitioners deliver consistently per key.
	partTopic := uniqueName("it18-part")
	createTestTopic(t, brokers, partTopic, 4)

	for _, partMode := range []string{"hash", "crc32", "murmur2"} {
		pc := kafka.DefaultProducerConfig()
		pc.Partitioner = partMode
		prod, err := kafka.NewProducer(brokerCfgFor(brokers), pc)
		if err != nil {
			t.Fatalf("NewProducer %s: %v", partMode, err)
		}
		for i := 0; i < 4; i++ {
			if err := prod.Publish(context.Background(), partTopic, []byte("gw-"+partMode), []byte(fmt.Sprintf("%s-%d", partMode, i))); err != nil {
				_ = prod.Close()
				t.Fatalf("Publish %s: %v", partMode, err)
			}
		}
		_ = prod.Close()
	}

	// Verify identical keys for each partitioner landed on a single partition.
	partByMode := map[string]map[int32]int{}
	var mu sync.Mutex
	pGroup := uniqueName("grp-it18-part")
	cPart, err := kafka.NewConsumer(brokerCfgFor(brokers), consumerCfgFor(pGroup), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	var totalPartMsgs atomic.Int32
	pCtx, pCancel := context.WithCancel(context.Background())
	if err := cPart.Subscribe(kafka.Subscription{
		Topic:       partTopic,
		RetryPolicy: kafka.DefaultRetryPolicy(),
		Handler: func(ctx context.Context, msg *kafka.Message) error {
			mode := strings.SplitN(string(msg.Value), "-", 2)[0]
			mu.Lock()
			if partByMode[mode] == nil {
				partByMode[mode] = map[int32]int{}
			}
			partByMode[mode][msg.Partition]++
			mu.Unlock()
			if totalPartMsgs.Add(1) == 12 {
				pCancel()
			}
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := cPart.Run(pCtx); err != nil {
		t.Fatalf("cPart Run: %v", err)
	}
	for _, mode := range []string{"hash", "crc32", "murmur2"} {
		if len(partByMode[mode]) != 1 {
			t.Fatalf("expected all keyed messages for partitioner %s on 1 partition, got %+v", mode, partByMode[mode])
		}
	}

	// 2. Sustained multi-topic concurrent producers + rebalancing consumer group.
	topics := []string{uniqueName("it18-t0"), uniqueName("it18-t1"), uniqueName("it18-t2")}
	for _, tp := range topics {
		createTestTopic(t, brokers, tp, 2)
	}

	const perTopic = 60
	totalExpected := len(topics) * perTopic

	var prodWg sync.WaitGroup
	for _, tp := range topics {
		topicName := tp
		prodWg.Add(1)
		go func() {
			defer prodWg.Done()
			p, err := kafka.NewProducer(brokerCfgFor(brokers), kafka.DefaultProducerConfig())
			if err != nil {
				t.Errorf("NewProducer: %v", err)
				return
			}
			defer p.Close()
			for i := 0; i < perTopic; i++ {
				if err := p.Publish(context.Background(), topicName, []byte(fmt.Sprintf("k-%d", i%4)), []byte(fmt.Sprintf("%s:%d", topicName, i))); err != nil {
					t.Errorf("Publish %s: %v", topicName, err)
					return
				}
			}
		}()
	}
	prodWg.Wait()

	groupID := uniqueName("grp-it18-sustained")
	cc := consumerCfgFor(groupID)
	cc.SessionTimeout = 6 * time.Second
	cc.HeartbeatInterval = 1500 * time.Millisecond
	cc.CommitInterval = 200 * time.Millisecond

	received := map[string]int{}
	makeConsumer := func() kafka.Consumer {
		gc, err := kafka.NewConsumer(brokerCfgFor(brokers), cc, testLogger())
		if err != nil {
			t.Fatal(err)
		}
		for _, tp := range topics {
			topicName := tp
			if err := gc.Subscribe(kafka.Subscription{
				Topic:       topicName,
				RetryPolicy: kafka.DefaultRetryPolicy(),
				Handler: func(ctx context.Context, msg *kafka.Message) error {
					mu.Lock()
					received[string(msg.Value)]++
					mu.Unlock()
					return nil
				},
			}); err != nil {
				t.Fatal(err)
			}
		}
		return gc
	}

	m1 := makeConsumer()
	ctx1, cancel1 := context.WithCancel(context.Background())
	d1 := make(chan error, 1)
	go func() { d1 <- m1.Run(ctx1) }()

	waitForCondition(t, 15*time.Second, "m1 consumes initial records", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(received) >= 10
	})

	// Trigger rebalance by adding m2 mid-stream, then stop m1 so m2 finishes remaining partitions.
	m2 := makeConsumer()
	ctx2, cancel2 := context.WithCancel(context.Background())
	d2 := make(chan error, 1)
	go func() { d2 <- m2.Run(ctx2) }()

	waitForCondition(t, 15*time.Second, "m2 ready after rebalance", func() bool {
		return m2.Health().Ready
	})

	cancel1()
	if err := <-d1; err != nil {
		t.Fatalf("m1 Run: %v", err)
	}

	waitForCondition(t, 30*time.Second, "all sustained multi-topic records consumed across rebalance", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(received) == totalExpected
	})
	cancel2()
	if err := <-d2; err != nil {
		t.Fatalf("m2 Run: %v", err)
	}
}
