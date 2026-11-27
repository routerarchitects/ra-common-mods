package kafka

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IBM/sarama"
)

type markedRecord struct {
	Topic     string
	Partition int32
	Offset    int64
}

type fakeSession struct {
	ctx       context.Context
	claims    map[string][]int32
	memberID  string
	genID     int32
	mu        sync.Mutex
	marked    []markedRecord
	commitCnt int
}

func newFakeSession(ctx context.Context, claims map[string][]int32) *fakeSession {
	return &fakeSession{
		ctx:      ctx,
		claims:   claims,
		memberID: "member-1",
		genID:    1,
	}
}

func (s *fakeSession) Claims() map[string][]int32 { return s.claims }
func (s *fakeSession) MemberID() string           { return s.memberID }
func (s *fakeSession) GenerationID() int32        { return s.genID }
func (s *fakeSession) MarkOffset(topic string, partition int32, offset int64, metadata string) {
	s.mu.Lock()
	s.marked = append(s.marked, markedRecord{Topic: topic, Partition: partition, Offset: offset})
	s.mu.Unlock()
}
func (s *fakeSession) Commit() {
	s.mu.Lock()
	s.commitCnt++
	s.mu.Unlock()
}
func (s *fakeSession) ResetOffset(string, int32, int64, string) {}
func (s *fakeSession) MarkMessage(msg *sarama.ConsumerMessage, metadata string) {
	s.MarkOffset(msg.Topic, msg.Partition, msg.Offset+1, metadata)
}
func (s *fakeSession) Context() context.Context { return s.ctx }

func (s *fakeSession) snapshotMarks() ([]markedRecord, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]markedRecord, len(s.marked))
	copy(out, s.marked)
	return out, s.commitCnt
}

type fakeClaim struct {
	topic     string
	partition int32
	initial   int64
	hwm       atomic.Int64
	msgs      chan *sarama.ConsumerMessage
}

func newFakeClaim(topic string, partition int32, msgs ...*sarama.ConsumerMessage) *fakeClaim {
	ch := make(chan *sarama.ConsumerMessage, len(msgs)+4)
	for _, m := range msgs {
		ch <- m
	}
	c := &fakeClaim{
		topic:     topic,
		partition: partition,
		msgs:      ch,
	}
	if len(msgs) > 0 {
		c.hwm.Store(msgs[len(msgs)-1].Offset + 1)
	}
	return c
}

func (c *fakeClaim) Topic() string                            { return c.topic }
func (c *fakeClaim) Partition() int32                         { return c.partition }
func (c *fakeClaim) InitialOffset() int64                     { return c.initial }
func (c *fakeClaim) HighWaterMarkOffset() int64               { return c.hwm.Load() }
func (c *fakeClaim) Messages() <-chan *sarama.ConsumerMessage { return c.msgs }

type fakeConsumerGroup struct {
	mu          sync.Mutex
	consumeFn   func(ctx context.Context, topics []string, handler sarama.ConsumerGroupHandler) error
	consumeRuns [][]string
	errCh       chan error
	closeCnt    atomic.Int32
	closeErr    error
	closedCh    chan struct{}
	closeOnce   sync.Once
}

func newFakeConsumerGroup() *fakeConsumerGroup {
	return &fakeConsumerGroup{
		errCh:    make(chan error, 16),
		closedCh: make(chan struct{}),
	}
}

func (f *fakeConsumerGroup) Consume(ctx context.Context, topics []string, handler sarama.ConsumerGroupHandler) error {
	f.mu.Lock()
	f.consumeRuns = append(f.consumeRuns, append([]string(nil), topics...))
	fn := f.consumeFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, topics, handler)
	}
	<-ctx.Done()
	return nil
}

func (f *fakeConsumerGroup) Errors() <-chan error { return f.errCh }

func (f *fakeConsumerGroup) Close() error {
	f.closeCnt.Add(1)
	f.closeOnce.Do(func() {
		close(f.closedCh)
		close(f.errCh)
	})
	return f.closeErr
}

// runFakeSession simulates Sarama's ConsumerGroup session lifecycle for the provided claims.
func runFakeSession(ctx context.Context, handler sarama.ConsumerGroupHandler, claims ...*fakeClaim) (*fakeSession, error) {
	claimMap := make(map[string][]int32)
	for _, cl := range claims {
		claimMap[cl.topic] = append(claimMap[cl.topic], cl.partition)
	}
	sessCtx, sessCancel := context.WithCancel(ctx)
	defer sessCancel()

	sess := newFakeSession(sessCtx, claimMap)
	if err := handler.Setup(sess); err != nil {
		return sess, err
	}

	var wg sync.WaitGroup
	var errMu sync.Mutex
	var firstErr error

	for _, cl := range claims {
		wg.Add(1)
		go func(claim *fakeClaim) {
			defer wg.Done()
			defer sessCancel() // Sarama cancels session as soon as any ConsumeClaim exits.
			if err := handler.ConsumeClaim(sess, claim); err != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				errMu.Unlock()
			}
		}(cl)
	}

	wg.Wait()
	if err := handler.Cleanup(sess); err != nil && firstErr == nil {
		firstErr = err
	}
	return sess, firstErr
}

func testBrokerConfig() BrokerConfig {
	b := DefaultBrokerConfig()
	b.Brokers = []string{"localhost:9092"}
	return b
}

func testConsumerConfig(groupID string) ConsumerConfig {
	c := DefaultConsumerConfig()
	c.GroupID = groupID
	return c
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestSubscriptionValidationAndMultiTopicIsolation(t *testing.T) {
	t.Run("UT17 validation rejects invalid subscriptions with zero broker IO", func(t *testing.T) {
		var factoryCalls atomic.Int32
		gc, err := NewConsumer(testBrokerConfig(), testConsumerConfig("grp-1"), discardLogger(), func(o *consumerOptions) {
			o.consumerGroupFactory = func([]string, string, *sarama.Config) (consumerGroupClient, error) {
				factoryCalls.Add(1)
				return nil, nil
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		defer gc.Close()

		noop := func(context.Context, *Message) error { return nil }
		if err := gc.Subscribe(Subscription{Topic: "", Handler: noop, RetryPolicy: DefaultRetryPolicy()}); err == nil {
			t.Fatalf("expected error for empty topic")
		}
		if err := gc.Subscribe(Subscription{Topic: "a/b", Handler: noop, RetryPolicy: DefaultRetryPolicy()}); err == nil {
			t.Fatalf("expected error for invalid topic name")
		}
		if err := gc.Subscribe(Subscription{Topic: "t1", Handler: nil, RetryPolicy: DefaultRetryPolicy()}); err == nil {
			t.Fatalf("expected error for nil handler")
		}
		if err := gc.Subscribe(Subscription{Topic: "t1", Handler: noop, RetryPolicy: nil}); err == nil {
			t.Fatalf("expected error for nil retry policy")
		}
		if err := gc.Subscribe(Subscription{Topic: "t1", Handler: noop, RetryPolicy: DefaultRetryPolicy(), Interceptors: []Interceptor{nil}}); err == nil {
			t.Fatalf("expected error for nil interceptor")
		}
		if err := gc.Subscribe(Subscription{Topic: "t1", Handler: noop, RetryPolicy: DefaultRetryPolicy(), HandlerTimeout: -time.Millisecond}); err == nil {
			t.Fatalf("expected error for negative handler timeout")
		}
		if err := gc.Subscribe(Subscription{Topic: "t1", Handler: noop, RetryPolicy: &RetryPolicy{Strategy: "bogus"}}); err == nil {
			t.Fatalf("expected error for invalid retry policy")
		}
		if err := gc.Subscribe(Subscription{Topic: "t1", Handler: noop, RetryPolicy: DefaultRetryPolicy()}); err != nil {
			t.Fatalf("valid subscription failed: %v", err)
		}
		if err := gc.Subscribe(Subscription{Topic: "t1", Handler: noop, RetryPolicy: DefaultRetryPolicy()}); err == nil {
			t.Fatalf("expected error for duplicate topic")
		}
		if factoryCalls.Load() != 0 {
			t.Fatalf("Subscribe must not perform broker IO")
		}
	})

	t.Run("UT18_UT20 multiple topics use single client and isolated handlers/policies/interceptors", func(t *testing.T) {
		fg := newFakeConsumerGroup()
		var factoryCalls atomic.Int32
		var capturedSess *fakeSession

		fg.consumeFn = func(ctx context.Context, topics []string, handler sarama.ConsumerGroupHandler) error {
			c1 := newFakeClaim("topic-a", 0, &sarama.ConsumerMessage{Topic: "topic-a", Partition: 0, Offset: 10, Value: []byte("a")})
			c2 := newFakeClaim("topic-b", 1, &sarama.ConsumerMessage{Topic: "topic-b", Partition: 1, Offset: 20, Value: []byte("b")})
			var err error
			capturedSess, err = runFakeSession(ctx, handler, c1, c2)
			return err
		}

		gc, err := NewConsumer(testBrokerConfig(), testConsumerConfig("grp-multi"), discardLogger(), func(o *consumerOptions) {
			o.consumerGroupFactory = func([]string, string, *sarama.Config) (consumerGroupClient, error) {
				factoryCalls.Add(1)
				return fg, nil
			}
			o.sleepFn = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
			o.jitterFn = func(d time.Duration) time.Duration { return d }
		})
		if err != nil {
			t.Fatal(err)
		}

		var aCalls, bCalls atomic.Int32
		var doneCount atomic.Int32
		var traceOrder []string
		var mu sync.Mutex

		interceptors := []Interceptor{
			func(next Handler) Handler {
				return func(ctx context.Context, msg *Message) error {
					mu.Lock()
					traceOrder = append(traceOrder, "ic:"+msg.Topic)
					mu.Unlock()
					return next(ctx, msg)
				}
			},
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		policyA := &RetryPolicy{Strategy: RetryStrategyLogAndIgnore, MaxRetries: 2}
		if err := gc.Subscribe(Subscription{
			Topic:        "topic-a",
			RetryPolicy:  policyA,
			Interceptors: interceptors,
			Handler: func(ctx context.Context, msg *Message) error {
				if aCalls.Add(1) < 3 {
					return errors.New("transient")
				}
				if doneCount.Add(1) == 2 {
					cancel()
				}
				return nil
			},
		}); err != nil {
			t.Fatal(err)
		}

		// Mutating caller-owned slice and policy after Subscribe must not affect registered subscription.
		interceptors[0] = nil
		policyA.MaxRetries = 0

		if err := gc.Subscribe(Subscription{
			Topic:       "topic-b",
			RetryPolicy: &RetryPolicy{Strategy: RetryStrategyLogAndIgnore, MaxRetries: 0},
			Handler: func(ctx context.Context, msg *Message) error {
				bCalls.Add(1)
				if doneCount.Add(1) == 2 {
					cancel()
				}
				return nil
			},
		}); err != nil {
			t.Fatal(err)
		}

		if err := gc.Run(ctx); err != nil {
			t.Fatalf("Run failed: %v", err)
		}

		if factoryCalls.Load() != 1 {
			t.Fatalf("expected 1 client factory call, got %d", factoryCalls.Load())
		}
		if len(fg.consumeRuns) == 0 || !slices.Equal(fg.consumeRuns[0], []string{"topic-a", "topic-b"}) {
			t.Fatalf("unexpected topics passed to Consume: %+v", fg.consumeRuns)
		}
		if aCalls.Load() != 3 || bCalls.Load() != 1 {
			t.Fatalf("unexpected handler counts a=%d b=%d", aCalls.Load(), bCalls.Load())
		}
		marks, commitCnt := capturedSess.snapshotMarks()
		if commitCnt != 0 || len(marks) != 2 {
			t.Fatalf("expected 2 marks and 0 sync commits in periodic mode, got marks=%+v commits=%d", marks, commitCnt)
		}
	})
}

func TestCommitModesAndUnresolvedFailures(t *testing.T) {
	t.Run("UT01 sync commit mode calls Commit per resolved message and in Cleanup", func(t *testing.T) {
		fg := newFakeConsumerGroup()
		var capturedSess *fakeSession
		ctx, cancel := context.WithCancel(context.Background())

		fg.consumeFn = func(runCtx context.Context, topics []string, handler sarama.ConsumerGroupHandler) error {
			c := newFakeClaim("orders", 0,
				&sarama.ConsumerMessage{Topic: "orders", Partition: 0, Offset: 1, Value: []byte("1")},
				&sarama.ConsumerMessage{Topic: "orders", Partition: 0, Offset: 2, Value: []byte("2")},
			)
			var err error
			capturedSess, err = runFakeSession(runCtx, handler, c)
			return err
		}

		cfg := testConsumerConfig("grp-sync")
		cfg.CommitMode = CommitSync
		gc, err := NewConsumer(testBrokerConfig(), cfg, discardLogger(), func(o *consumerOptions) {
			o.consumerGroupFactory = func([]string, string, *sarama.Config) (consumerGroupClient, error) {
				return fg, nil
			}
		})
		if err != nil {
			t.Fatal(err)
		}

		var count atomic.Int32
		if err := gc.Subscribe(Subscription{
			Topic:       "orders",
			RetryPolicy: DefaultRetryPolicy(),
			Handler: func(ctx context.Context, msg *Message) error {
				if count.Add(1) == 2 {
					cancel()
				}
				return nil
			},
		}); err != nil {
			t.Fatal(err)
		}

		if err := gc.Run(ctx); err != nil {
			t.Fatalf("Run failed: %v", err)
		}
		marks, commits := capturedSess.snapshotMarks()
		if len(marks) != 2 || marks[0].Offset != 2 || marks[1].Offset != 3 {
			t.Fatalf("unexpected marks: %+v", marks)
		}
		// 2 per-message sync commits + 1 cleanup commit
		if commits != 3 {
			t.Fatalf("expected 3 sync commits (2 per-msg + 1 cleanup), got %d", commits)
		}
	})

	t.Run("UT05 unresolved DLQFailureStop stops claim before later offset and Run returns error", func(t *testing.T) {
		fg := newFakeConsumerGroup()
		var capturedSess *fakeSession
		fg.consumeFn = func(runCtx context.Context, topics []string, handler sarama.ConsumerGroupHandler) error {
			c := newFakeClaim("orders", 0,
				&sarama.ConsumerMessage{Topic: "orders", Partition: 0, Offset: 10, Value: []byte("fail")},
				&sarama.ConsumerMessage{Topic: "orders", Partition: 0, Offset: 11, Value: []byte("must-not-run")},
			)
			var err error
			capturedSess, err = runFakeSession(runCtx, handler, c)
			return err
		}

		gc, err := NewConsumer(testBrokerConfig(), testConsumerConfig("grp-stop"), discardLogger(), func(o *consumerOptions) {
			o.consumerGroupFactory = func([]string, string, *sarama.Config) (consumerGroupClient, error) {
				return fg, nil
			}
		})
		if err != nil {
			t.Fatal(err)
		}

		dlq := &mockDLQProducer{
			publishF: func(context.Context, string, []byte, []byte, []RecordHeader) error {
				return errors.New("dlq broker unreachable")
			},
		}

		var processedOffsets []int64
		if err := gc.Subscribe(Subscription{
			Topic: "orders",
			RetryPolicy: &RetryPolicy{
				Strategy:         RetryStrategyDLQ,
				MaxRetries:       0,
				DLQTopic:         "orders-dlq",
				DLQProducer:      dlq,
				DLQFailurePolicy: DLQFailureStop,
			},
			Handler: func(ctx context.Context, msg *Message) error {
				processedOffsets = append(processedOffsets, msg.Offset)
				return errors.New("handler failed")
			},
		}); err != nil {
			t.Fatal(err)
		}

		runErr := gc.Run(context.Background())
		if runErr == nil {
			t.Fatalf("expected Run to return terminal error")
		}
		if !slices.Equal(processedOffsets, []int64{10}) {
			t.Fatalf("expected only offset 10 processed, got %v", processedOffsets)
		}
		marks, commits := capturedSess.snapshotMarks()
		if len(marks) != 0 || commits != 0 {
			t.Fatalf("unresolved failure must not mark or commit: marks=%+v commits=%d", marks, commits)
		}
		if h := gc.Health(); h.State != ConsumerStateFailed || h.Ready {
			t.Fatalf("expected Failed and unready health, got %+v", h)
		}
	})

	t.Run("UT19 unexpected unknown topic returns terminal failure without marking", func(t *testing.T) {
		fg := newFakeConsumerGroup()
		var capturedSess *fakeSession
		fg.consumeFn = func(runCtx context.Context, topics []string, handler sarama.ConsumerGroupHandler) error {
			c := newFakeClaim("unregistered-topic", 0,
				&sarama.ConsumerMessage{Topic: "unregistered-topic", Partition: 0, Offset: 1},
			)
			var err error
			capturedSess, err = runFakeSession(runCtx, handler, c)
			return err
		}

		gc, err := NewConsumer(testBrokerConfig(), testConsumerConfig("grp-unk"), discardLogger(), func(o *consumerOptions) {
			o.consumerGroupFactory = func([]string, string, *sarama.Config) (consumerGroupClient, error) {
				return fg, nil
			}
		})
		if err != nil {
			t.Fatal(err)
		}

		var called atomic.Bool
		if err := gc.Subscribe(Subscription{
			Topic:       "expected-topic",
			RetryPolicy: DefaultRetryPolicy(),
			Handler: func(context.Context, *Message) error {
				called.Store(true)
				return nil
			},
		}); err != nil {
			t.Fatal(err)
		}

		if err := gc.Run(context.Background()); err == nil {
			t.Fatalf("expected terminal error for unexpected topic")
		}
		if called.Load() {
			t.Fatalf("handler must not be invoked for unknown topic")
		}
		marks, _ := capturedSess.snapshotMarks()
		if len(marks) != 0 {
			t.Fatalf("unexpected topic must not be marked")
		}
	})
}

func TestConsumerLifecycleCloseAndGracefulShutdown(t *testing.T) {
	t.Run("UT21 Run without topics leaves Registered; duplicate Run and late Subscribe rejected", func(t *testing.T) {
		fg := newFakeConsumerGroup()
		started := make(chan struct{})
		fg.consumeFn = func(ctx context.Context, topics []string, handler sarama.ConsumerGroupHandler) error {
			close(started)
			<-ctx.Done()
			return nil
		}

		gc, err := NewConsumer(testBrokerConfig(), testConsumerConfig("grp-life"), discardLogger(), func(o *consumerOptions) {
			o.consumerGroupFactory = func([]string, string, *sarama.Config) (consumerGroupClient, error) {
				return fg, nil
			}
		})
		if err != nil {
			t.Fatal(err)
		}

		if err := gc.Run(context.Background()); !errors.Is(err, ErrNoSubscriptions) {
			t.Fatalf("expected ErrNoSubscriptions, got %v", err)
		}
		if gc.Health().State != ConsumerStateRegistered {
			t.Fatalf("expected state Registered after ErrNoSubscriptions, got %s", gc.Health().State)
		}

		if err := gc.Subscribe(Subscription{Topic: "t1", RetryPolicy: DefaultRetryPolicy(), Handler: func(context.Context, *Message) error { return nil }}); err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		runErrCh := make(chan error, 1)
		go func() { runErrCh <- gc.Run(ctx) }()

		<-started
		if err := gc.Subscribe(Subscription{Topic: "t2", RetryPolicy: DefaultRetryPolicy(), Handler: func(context.Context, *Message) error { return nil }}); !errors.Is(err, ErrConsumerAlreadyRunning) {
			t.Fatalf("expected ErrConsumerAlreadyRunning on late Subscribe, got %v", err)
		}
		if err := gc.Run(context.Background()); !errors.Is(err, ErrConsumerAlreadyRunning) {
			t.Fatalf("expected ErrConsumerAlreadyRunning on duplicate Run, got %v", err)
		}

		cancel()
		if err := <-runErrCh; err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
		if err := gc.Run(context.Background()); !errors.Is(err, ErrConsumerAlreadyRunning) {
			t.Fatalf("expected ErrConsumerAlreadyRunning after stop, got %v", err)
		}
	})

	t.Run("UT22 Close before Run allocates nothing and repeated concurrent Close is safe", func(t *testing.T) {
		var factoryCalls atomic.Int32
		gc, err := NewConsumer(testBrokerConfig(), testConsumerConfig("grp-close-pre"), discardLogger(), func(o *consumerOptions) {
			o.consumerGroupFactory = func([]string, string, *sarama.Config) (consumerGroupClient, error) {
				factoryCalls.Add(1)
				return nil, nil
			}
		})
		if err != nil {
			t.Fatal(err)
		}

		if err := gc.Close(); err != nil {
			t.Fatalf("first Close: %v", err)
		}
		if err := gc.Close(); err != nil {
			t.Fatalf("second Close: %v", err)
		}
		if gc.Health().State != ConsumerStateClosed {
			t.Fatalf("expected ConsumerStateClosed, got %s", gc.Health().State)
		}
		if err := gc.Run(context.Background()); !errors.Is(err, ErrConsumerClosed) {
			t.Fatalf("expected ErrConsumerClosed, got %v", err)
		}
		if factoryCalls.Load() != 0 {
			t.Fatalf("expected 0 factory calls, got %d", factoryCalls.Load())
		}
	})

	t.Run("UT23 Close during client creation releases created client and prevents Run loop", func(t *testing.T) {
		fg := newFakeConsumerGroup()
		inFactory := make(chan struct{})
		releaseFactory := make(chan struct{})

		gc, err := NewConsumer(testBrokerConfig(), testConsumerConfig("grp-close-create"), discardLogger(), func(o *consumerOptions) {
			o.consumerGroupFactory = func([]string, string, *sarama.Config) (consumerGroupClient, error) {
				close(inFactory)
				<-releaseFactory
				return fg, nil
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := gc.Subscribe(Subscription{Topic: "t1", RetryPolicy: DefaultRetryPolicy(), Handler: func(context.Context, *Message) error { return nil }}); err != nil {
			t.Fatal(err)
		}

		runErr := make(chan error, 1)
		go func() { runErr <- gc.Run(context.Background()) }()

		<-inFactory
		closeErr := make(chan error, 3)
		for i := 0; i < 3; i++ {
			go func() { closeErr <- gc.Close() }()
		}
		time.Sleep(15 * time.Millisecond)
		close(releaseFactory)

		if err := <-runErr; err != nil {
			t.Fatalf("Run error: %v", err)
		}
		for i := 0; i < 3; i++ {
			if err := <-closeErr; err != nil {
				t.Fatalf("Close error: %v", err)
			}
		}
		if fg.closeCnt.Load() != 1 || len(fg.consumeRuns) != 0 {
			t.Fatalf("expected single Close and 0 Consume runs, got close=%d runs=%d", fg.closeCnt.Load(), len(fg.consumeRuns))
		}
	})

	t.Run("UT24 graceful shutdown drains active partition without idle claim interrupting it", func(t *testing.T) {
		fg := newFakeConsumerGroup()
		var capturedSess *fakeSession
		p1Started := make(chan struct{})
		p1AllowFinish := make(chan struct{})

		fg.consumeFn = func(runCtx context.Context, topics []string, handler sarama.ConsumerGroupHandler) error {
			// Partition 0 is idle (no messages); Partition 1 has 2 messages.
			idleClaim := newFakeClaim("t1", 0)
			activeClaim := newFakeClaim("t1", 1,
				&sarama.ConsumerMessage{Topic: "t1", Partition: 1, Offset: 100},
				&sarama.ConsumerMessage{Topic: "t1", Partition: 1, Offset: 101},
			)
			var err error
			capturedSess, err = runFakeSession(runCtx, handler, idleClaim, activeClaim)
			return err
		}

		cfg := testConsumerConfig("grp-drain")
		cfg.ShutdownTimeout = 2 * time.Second
		gc, err := NewConsumer(testBrokerConfig(), cfg, discardLogger(), func(o *consumerOptions) {
			o.consumerGroupFactory = func([]string, string, *sarama.Config) (consumerGroupClient, error) {
				return fg, nil
			}
		})
		if err != nil {
			t.Fatal(err)
		}

		var processed []int64
		var mu sync.Mutex
		if err := gc.Subscribe(Subscription{
			Topic:       "t1",
			RetryPolicy: DefaultRetryPolicy(),
			Handler: func(ctx context.Context, msg *Message) error {
				close(p1Started)
				<-p1AllowFinish
				// Session context must STILL be active during grace window!
				if ctx.Err() != nil {
					return fmt.Errorf("unexpected premature context cancellation: %w", ctx.Err())
				}
				mu.Lock()
				processed = append(processed, msg.Offset)
				mu.Unlock()
				return nil
			},
		}); err != nil {
			t.Fatal(err)
		}

		runCtx, runCancel := context.WithCancel(context.Background())
		runDone := make(chan error, 1)
		go func() { runDone <- gc.Run(runCtx) }()

		<-p1Started
		runCancel() // Initiate graceful shutdown while p1 is active and p0 is idle!
		time.Sleep(30 * time.Millisecond)

		close(p1AllowFinish)
		if err := <-runDone; err != nil {
			t.Fatalf("Run error: %v", err)
		}

		// Offset 100 must finish and be marked; offset 101 must NOT be admitted after shutdown started.
		if !slices.Equal(processed, []int64{100}) {
			t.Fatalf("expected only offset 100 processed during drain, got %v", processed)
		}
		marks, _ := capturedSess.snapshotMarks()
		if len(marks) != 1 || marks[0].Offset != 101 {
			t.Fatalf("expected mark at 101 (offset 100+1), got %+v", marks)
		}
	})

	t.Run("UT25 grace expiry interrupts unresolved handler without marking", func(t *testing.T) {
		fg := newFakeConsumerGroup()
		var capturedSess *fakeSession
		started := make(chan struct{})

		fg.consumeFn = func(runCtx context.Context, topics []string, handler sarama.ConsumerGroupHandler) error {
			c := newFakeClaim("t1", 0,
				&sarama.ConsumerMessage{Topic: "t1", Partition: 0, Offset: 5},
				&sarama.ConsumerMessage{Topic: "t1", Partition: 0, Offset: 6},
			)
			var err error
			capturedSess, err = runFakeSession(runCtx, handler, c)
			return err
		}

		cfg := testConsumerConfig("grp-expire")
		cfg.ShutdownTimeout = 25 * time.Millisecond
		gc, err := NewConsumer(testBrokerConfig(), cfg, discardLogger(), func(o *consumerOptions) {
			o.consumerGroupFactory = func([]string, string, *sarama.Config) (consumerGroupClient, error) {
				return fg, nil
			}
		})
		if err != nil {
			t.Fatal(err)
		}

		var calls atomic.Int32
		if err := gc.Subscribe(Subscription{
			Topic:       "t1",
			RetryPolicy: DefaultRetryPolicy(),
			Handler: func(ctx context.Context, msg *Message) error {
				calls.Add(1)
				close(started)
				<-ctx.Done()
				return ctx.Err()
			},
		}); err != nil {
			t.Fatal(err)
		}

		runCtx, runCancel := context.WithCancel(context.Background())
		runDone := make(chan error, 1)
		go func() { runDone <- gc.Run(runCtx) }()

		<-started
		runCancel()
		if err := <-runDone; err != nil {
			t.Fatalf("Run error: %v", err)
		}

		if calls.Load() != 1 {
			t.Fatalf("expected 1 handler call, got %d", calls.Load())
		}
		marks, _ := capturedSess.snapshotMarks()
		if len(marks) != 0 {
			t.Fatalf("unresolved interrupted message must not be marked, got %+v", marks)
		}
	})

	t.Run("UT26_UT30 error drain, transient retry, terminal error propagation, and readiness", func(t *testing.T) {
		fg := newFakeConsumerGroup()
		var attempts atomic.Int32
		sessionReadyObserved := make(chan bool, 1)

		fg.consumeFn = func(runCtx context.Context, topics []string, handler sarama.ConsumerGroupHandler) error {
			n := attempts.Add(1)
			if n == 1 {
				return errors.New("temporary network blip")
			}
			if n == 2 {
				// Valid session with 0 claims must still report Ready=true.
				sess := newFakeSession(runCtx, map[string][]int32{})
				if err := handler.Setup(sess); err != nil {
					return err
				}
				// Push a non-terminal error on errCh and then a terminal authorization error.
				fg.errCh <- &sarama.ConsumerError{Topic: "t1", Partition: 0, Err: sarama.ErrOffsetMetadataTooLarge}
				// Wait briefly for error drainer to record commit error
				time.Sleep(15 * time.Millisecond)
				fg.errCh <- &sarama.ConsumerError{Topic: "t1", Partition: 0, Err: sarama.ErrTopicAuthorizationFailed}
				<-runCtx.Done()
				_ = handler.Cleanup(sess)
				return nil
			}
			return nil
		}

		gc, err := NewConsumer(testBrokerConfig(), testConsumerConfig("grp-err"), discardLogger(), func(o *consumerOptions) {
			o.consumerGroupFactory = func([]string, string, *sarama.Config) (consumerGroupClient, error) {
				return fg, nil
			}
			o.sleepFn = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
			o.jitterFn = func(d time.Duration) time.Duration { return d }
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := gc.Subscribe(Subscription{Topic: "t1", RetryPolicy: DefaultRetryPolicy(), Handler: func(context.Context, *Message) error { return nil }}); err != nil {
			t.Fatal(err)
		}

		// Poll Health concurrently to verify race safety.
		stopPoll := make(chan struct{})
		go func() {
			for {
				select {
				case <-stopPoll:
					return
				default:
					h := gc.Health()
					if h.Ready && h.AssignedClaims == 0 {
						select {
						case sessionReadyObserved <- true:
						default:
						}
					}
					time.Sleep(time.Millisecond)
				}
			}
		}()

		err = gc.Run(context.Background())
		close(stopPoll)

		if !errors.Is(err, sarama.ErrTopicAuthorizationFailed) {
			t.Fatalf("expected ErrTopicAuthorizationFailed, got %v", err)
		}
		select {
		case <-sessionReadyObserved:
		default:
			t.Fatalf("expected Ready=true to be observed even with 0 claims")
		}
		h := gc.Health()
		if h.State != ConsumerStateFailed || h.Ready || h.LastError == "" {
			t.Fatalf("unexpected final health: %+v", h)
		}
	})
}

func TestShutdownAdmissionCutoffDeterministic(t *testing.T) {
	t.Run("R01 graceful shutdown closes gate before stop notification and blocks new admissions", func(t *testing.T) {
		fg := newFakeConsumerGroup()
		var capturedSess *fakeSession
		p0InHandler := make(chan struct{})
		allowP0Finish := make(chan struct{})
		p1Claim := newFakeClaim("t1", 1)

		fg.consumeFn = func(runCtx context.Context, topics []string, handler sarama.ConsumerGroupHandler) error {
			p0Claim := newFakeClaim("t1", 0, &sarama.ConsumerMessage{Topic: "t1", Partition: 0, Offset: 10})
			var err error
			capturedSess, err = runFakeSession(runCtx, handler, p0Claim, p1Claim)
			return err
		}

		cfg := testConsumerConfig("grp-r01-grace")
		cfg.ShutdownTimeout = 2 * time.Second
		gc, err := NewConsumer(testBrokerConfig(), cfg, discardLogger(), func(o *consumerOptions) {
			o.consumerGroupFactory = func([]string, string, *sarama.Config) (consumerGroupClient, error) { return fg, nil }
		})
		if err != nil {
			t.Fatal(err)
		}

		var p0Count, p1Count atomic.Int32
		if err := gc.Subscribe(Subscription{
			Topic:       "t1",
			RetryPolicy: DefaultRetryPolicy(),
			Handler: func(ctx context.Context, msg *Message) error {
				if msg.Partition == 0 {
					p0Count.Add(1)
					close(p0InHandler)
					<-allowP0Finish
					return nil
				}
				p1Count.Add(1)
				return nil
			},
		}); err != nil {
			t.Fatal(err)
		}

		runDone := make(chan error, 1)
		go func() { runDone <- gc.Run(context.Background()) }()

		<-p0InHandler
		cons := gc.(*consumer)
		closeDone := make(chan error, 1)
		go func() { closeDone <- gc.Close() }()

		// Deterministic cutoff sync: stopCh closes strictly AFTER gate.CloseAndDrainedCh().
		<-cons.stopCh
		if !cons.gate.IsStopping() {
			t.Fatalf("expected admission gate closed before stopCh closed")
		}
		// Feed a message into partition 1 after cutoff; it must never be admitted or marked.
		p1Claim.msgs <- &sarama.ConsumerMessage{Topic: "t1", Partition: 1, Offset: 99}
		// Verify direct TryAdmit fails deterministically too.
		if cons.gate.TryAdmit(context.Background()) {
			t.Fatalf("expected TryAdmit to fail after gate closure")
		}

		close(allowP0Finish)
		if err := <-runDone; err != nil {
			t.Fatalf("Run: %v", err)
		}
		if err := <-closeDone; err != nil {
			t.Fatalf("Close: %v", err)
		}
		if p0Count.Load() != 1 || p1Count.Load() != 0 {
			t.Fatalf("expected p0=1 p1=0, got p0=%d p1=%d", p0Count.Load(), p1Count.Load())
		}
		marks, _ := capturedSess.snapshotMarks()
		if len(marks) != 1 || marks[0].Partition != 0 || marks[0].Offset != 11 {
			t.Fatalf("expected only p0 offset 11 marked, got %+v", marks)
		}
	})

	t.Run("R01 terminal failure closes gate before stop notification and cancels without marking", func(t *testing.T) {
		fg := newFakeConsumerGroup()
		var capturedSess *fakeSession
		p1Claim := newFakeClaim("t1", 1)
		consumeStarted := make(chan struct{})

		fg.consumeFn = func(runCtx context.Context, topics []string, handler sarama.ConsumerGroupHandler) error {
			close(consumeStarted)
			p0Claim := newFakeClaim("t1", 0, &sarama.ConsumerMessage{Topic: "t1", Partition: 0, Offset: 20})
			var err error
			capturedSess, err = runFakeSession(runCtx, handler, p0Claim, p1Claim)
			return err
		}

		gc, err := NewConsumer(testBrokerConfig(), testConsumerConfig("grp-r01-term"), discardLogger(), func(o *consumerOptions) {
			o.consumerGroupFactory = func([]string, string, *sarama.Config) (consumerGroupClient, error) { return fg, nil }
		})
		if err != nil {
			t.Fatal(err)
		}

		dlq := &mockDLQProducer{
			publishF: func(context.Context, string, []byte, []byte, []RecordHeader) error {
				return errors.New("dlq hard fail")
			},
		}
		var p1Called atomic.Bool
		if err := gc.Subscribe(Subscription{
			Topic: "t1",
			RetryPolicy: &RetryPolicy{
				Strategy:         RetryStrategyDLQ,
				MaxRetries:       0,
				DLQTopic:         "t1-dlq",
				DLQProducer:      dlq,
				DLQFailurePolicy: DLQFailureStop,
			},
			Handler: func(ctx context.Context, msg *Message) error {
				if msg.Partition == 1 {
					p1Called.Store(true)
				}
				return errors.New("handler error")
			},
		}); err != nil {
			t.Fatal(err)
		}

		runDone := make(chan error, 1)
		go func() { runDone <- gc.Run(context.Background()) }()

		// Wait deterministically for Consume to start, then for stopCh (closed after gate cutoff).
		<-consumeStarted
		cons := gc.(*consumer)
		<-cons.stopCh
		if !cons.gate.IsStopping() {
			t.Fatalf("expected gate closed before stopCh closed on terminal failure")
		}
		// Even if a message arrives on partition 1 right at terminal cutoff, it must not be admitted.
		select {
		case p1Claim.msgs <- &sarama.ConsumerMessage{Topic: "t1", Partition: 1, Offset: 50}:
		default:
		}

		runErr := <-runDone
		if runErr == nil {
			t.Fatalf("expected terminal error from Run")
		}
		if p1Called.Load() {
			t.Fatalf("partition 1 must not process message after terminal cutoff")
		}
		marks, _ := capturedSess.snapshotMarks()
		if len(marks) != 0 {
			t.Fatalf("terminal failure must not mark any unresolved message, got %+v", marks)
		}
	})
}

func TestConsumerStartupFactoryFailureContract(t *testing.T) {
	// R06: Initial Sarama client creation failure fails fast without startup retry loop.
	var factoryCalls atomic.Int32
	startupErr := errors.New("dial tcp 127.0.0.1:9092: connection refused")

	gc, err := NewConsumer(testBrokerConfig(), testConsumerConfig("grp-startup-fail"), discardLogger(), func(o *consumerOptions) {
		o.consumerGroupFactory = func([]string, string, *sarama.Config) (consumerGroupClient, error) {
			factoryCalls.Add(1)
			return nil, startupErr
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := gc.Subscribe(Subscription{
		Topic:       "orders",
		RetryPolicy: DefaultRetryPolicy(),
		Handler:     func(context.Context, *Message) error { return nil },
	}); err != nil {
		t.Fatal(err)
	}

	err = gc.Run(context.Background())
	if !errors.Is(err, startupErr) {
		t.Fatalf("expected wrapped startup error, got %v", err)
	}
	if factoryCalls.Load() != 1 {
		t.Fatalf("expected single fail-fast factory call without retry loop, got %d", factoryCalls.Load())
	}
	h := gc.Health()
	if h.State != ConsumerStateFailed || h.Ready || h.LastError == "" {
		t.Fatalf("expected Failed, Ready=false, non-empty LastError; got %+v", h)
	}
	if closeErr := gc.Close(); closeErr != nil {
		t.Fatalf("Close after startup failure: %v", closeErr)
	}
}

func stalledBrokerListener(t *testing.T) *net.TCPListener {
	t.Helper()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if err := listener.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return listener
}

func TestConsumerShutdownDuringBrokerStartup(t *testing.T) {
	listener := stalledBrokerListener(t)
	broker := testBrokerConfig()
	broker.Brokers = []string{listener.Addr().String()}
	broker.ReadTimeout = 5 * time.Second
	cfg := testConsumerConfig("startup-cancel")
	cfg.CloseTimeout = 25 * time.Millisecond
	gc, err := NewConsumer(broker, cfg, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := gc.Subscribe(Subscription{
		Topic:       "orders",
		Handler:     func(context.Context, *Message) error { return nil },
		RetryPolicy: DefaultRetryPolicy(),
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- gc.Run(ctx) }()
	conn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Keep the broker socket open and silent until Run finishes.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after startup cancellation: %v", err)
		}
	case <-time.After(400 * time.Millisecond):
		t.Fatalf("Run did not exit promptly after startup cancellation: %+v", gc.Health())
	}
	if health := gc.Health(); health.State != ConsumerStateStopped || health.Ready || health.LastError != "" {
		t.Fatalf("expected a clean stop after startup cancellation, got %+v", health)
	}
}

type blockingCommitSession struct {
	*fakeSession
	commit func()
}

func (s *blockingCommitSession) Commit() { s.commit() }

func TestConsumerCloseDuringStalledSyncCommit(t *testing.T) {
	listener := stalledBrokerListener(t)
	fg := newFakeConsumerGroup()
	commitStarted := make(chan struct{})
	cfg := testConsumerConfig("commit-stall")
	cfg.CommitMode = CommitSync
	cfg.ShutdownTimeout = 25 * time.Millisecond
	cfg.CloseTimeout = 25 * time.Millisecond
	gc, err := NewConsumer(testBrokerConfig(), cfg, discardLogger(), func(o *consumerOptions) {
		o.consumerGroupFactory = func(_ []string, _ string, sc *sarama.Config) (consumerGroupClient, error) {
			conn, err := sc.Net.Proxy.Dialer.Dial("tcp", listener.Addr().String())
			if err != nil {
				return nil, err
			}
			fg.consumeFn = func(ctx context.Context, _ []string, handler sarama.ConsumerGroupHandler) error {
				defer conn.Close()
				var firstCommit sync.Once
				session := &blockingCommitSession{
					fakeSession: newFakeSession(ctx, map[string][]int32{"orders": {0}}),
					commit: func() {
						firstCommit.Do(func() {
							close(commitStarted)
							_, _ = io.Copy(io.Discard, conn)
						})
					},
				}
				if err := handler.Setup(session); err != nil {
					return err
				}
				claim := newFakeClaim("orders", 0, &sarama.ConsumerMessage{Topic: "orders"})
				if err := handler.ConsumeClaim(session, claim); err != nil {
					return err
				}
				return handler.Cleanup(session)
			}
			return fg, nil
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := gc.Subscribe(Subscription{
		Topic:       "orders",
		Handler:     func(context.Context, *Message) error { return nil },
		RetryPolicy: DefaultRetryPolicy(),
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- gc.Run(ctx) }()
	peer, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	select {
	case <-commitStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("claim did not reach the synchronous commit")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- gc.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close after stalled commit: %v", err)
		}
	case <-time.After(400 * time.Millisecond):
		t.Fatal("Close remained blocked after the shutdown and teardown timeouts")
	}
	if err := <-runDone; err != nil {
		t.Fatalf("Run after stalled commit: %v", err)
	}
}

func TestReconnectBackoffWaitsForSessionReadiness(t *testing.T) {
	const healthy, zeroAssignment = 8, 10
	const cancelAt = 11
	fg := newFakeConsumerGroup()
	var attempts int
	var backoffAttempts []int
	var delays []time.Duration
	backoffStarted := make(chan struct{})
	fg.consumeFn = func(ctx context.Context, _ []string, handler sarama.ConsumerGroupHandler) error {
		attempts++
		if attempts > cancelAt {
			return errors.New("rejoined without entering cancellable backoff")
		}
		claims := map[string][]int32{"orders": {0, 1}}
		switch attempts {
		case healthy:
			claims["orders"] = []int32{0}
		case zeroAssignment:
			claims = nil
		}
		sessionCtx, cancelSession := context.WithCancel(ctx)
		defer cancelSession()
		session := newFakeSession(sessionCtx, claims)
		if err := handler.Setup(session); err != nil {
			return err
		}
		if len(claims) != 0 {
			// Partition 0 enters ConsumeClaim. On failed sessions, Sarama never
			// launches ConsumeClaim for partition 1 because its initialization failed.
			claim := newFakeClaim("orders", 0)
			close(claim.msgs)
			if err := handler.ConsumeClaim(session, claim); err != nil {
				return err
			}
		}
		cancelSession()
		return handler.Cleanup(session)
	}
	gc, err := NewConsumer(testBrokerConfig(), testConsumerConfig("reconnect"), discardLogger(), func(o *consumerOptions) {
		o.consumerGroupFactory = func([]string, string, *sarama.Config) (consumerGroupClient, error) { return fg, nil }
		o.jitterFn = func(d time.Duration) time.Duration { return d / 2 }
		o.sleepFn = func(ctx context.Context, d time.Duration) error {
			backoffAttempts = append(backoffAttempts, attempts)
			delays = append(delays, d)
			if attempts >= cancelAt {
				close(backoffStarted)
				return defaultSleep(ctx, time.Hour)
			}
			return nil
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := gc.Close(); err != nil {
			t.Errorf("close reconnect consumer: %v", err)
		}
	}()
	if err := gc.Subscribe(Subscription{
		Topic: "orders", RetryPolicy: DefaultRetryPolicy(),
		Handler: func(context.Context, *Message) error { return nil },
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- gc.Run(ctx) }()
	select {
	case <-backoffStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("failed session did not enter reconnect backoff")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not cancel reconnect backoff")
	}
	if want := []int{1, 2, 3, 4, 5, 6, 7, 9, 11}; !slices.Equal(backoffAttempts, want) || attempts != cancelAt {
		t.Fatalf("backoff after attempts %v, consumed %d sessions; want %v and %d sessions", backoffAttempts, attempts, want, cancelAt)
	}
	if want := []time.Duration{125 * time.Millisecond, 250 * time.Millisecond, 500 * time.Millisecond,
		time.Second, 2 * time.Second, 2500 * time.Millisecond, 2500 * time.Millisecond,
		125 * time.Millisecond, 125 * time.Millisecond}; !slices.Equal(delays, want) {
		t.Fatalf("reconnect delays %v; want %v", delays, want)
	}
}

func TestReadinessInvalidatesOnSessionCancellationBeforeCleanup(t *testing.T) {
	gc, err := NewConsumer(testBrokerConfig(), testConsumerConfig("session-cancellation"), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	c := gc.(*consumer)
	h := &consumerGroupHandler{consumer: c, commitMode: CommitSync}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	commitStarted := make(chan struct{})
	allowCommit := make(chan struct{})
	releaseCommit := sync.OnceFunc(func() { close(allowCommit) })
	defer releaseCommit()
	first := &blockingCommitSession{
		fakeSession: newFakeSession(ctx, map[string][]int32{"orders": {0}}),
		commit: func() {
			close(commitStarted)
			<-allowCommit
		},
	}
	if err := h.Setup(first); err != nil {
		t.Fatal(err)
	}
	seq := h.currentSessionSeq.Load()
	c.markClaimInitialized(ctx, seq, "orders", 0)
	if health := c.Health(); !health.Ready || health.State != ConsumerStateRunning {
		t.Fatalf("initialized session should be ready: %+v", health)
	}
	waitUnready := func() {
		t.Helper()
		deadline := time.NewTimer(2 * time.Second)
		defer deadline.Stop()
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for c.Health().Ready {
			select {
			case <-deadline.C:
				t.Fatal("session cancellation did not invalidate readiness before Cleanup")
			case <-ticker.C:
			}
		}
	}
	cancel()
	waitUnready()
	c.markClaimInitialized(ctx, seq, "orders", 0)
	h.hasMarked.Store(true)
	cleanupDone := make(chan error, 1)
	go func() { cleanupDone <- h.Cleanup(first) }()
	select {
	case <-commitStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("Cleanup did not enter the synchronous commit")
	}
	if health := c.Health(); health.Ready || health.State != ConsumerStateRebalancing || health.AssignedClaims != 1 {
		t.Fatalf("cancelled session should be unready while cleanup commits: %+v", health)
	}
	releaseCommit()
	if err := <-cleanupDone; err != nil {
		t.Fatal(err)
	}
	if h.stopSessionWatcher != nil {
		t.Fatal("Cleanup retained the session cancellation callback")
	}

	nextCtx, nextCancel := context.WithCancel(t.Context())
	defer nextCancel()
	next := newFakeSession(nextCtx, nil)
	if err := h.Setup(next); err != nil {
		t.Fatal(err)
	}
	c.invalidateSessionReadiness(seq) // An obsolete callback must leave the new session alone.
	if health := c.Health(); !health.Ready || health.State != ConsumerStateRunning || health.AssignedClaims != 0 {
		t.Fatalf("new zero-claim session should be ready: %+v", health)
	}
	nextCancel()
	waitUnready()
	if err := h.Cleanup(next); err != nil {
		t.Fatal(err)
	}
}

func TestDLQRetryStopsOnPermanentPublicationFailure(t *testing.T) {
	const source, dlq = "orders", "orders-dlq"
	const maxMessageBytes = 512
	payload := make([]byte, 400)
	handlerErr := errors.New("sensitive handler details")
	customErr := fmt.Errorf("custom wrapper: %w", Permanent(errors.New("sensitive producer details")))
	authErr := sarama.Wrap(sarama.ErrOutOfBrokers, sarama.ErrBrokerNotAvailable, sarama.ErrSASLAuthenticationFailed)
	configurationErr := sarama.ConfigurationError("sensitive configuration details")
	tlsErr := &tls.CertificateVerificationError{Err: errors.New("sensitive verification details")}
	for _, tc := range []struct {
		name       string
		wire       bool
		localLimit bool
		publishErr error
		diagnostic string
	}{
		{name: "headers exceed local limit", wire: true, localLimit: true, diagnostic: "dlq_configuration_error"},
		{name: "broker rejects size", wire: true, publishErr: sarama.ErrMessageSizeTooLarge, diagnostic: "dlq_message_too_large"},
		{name: "closed producer", wire: true, publishErr: ErrProducerClosed, diagnostic: "dlq_producer_closed"},
		{name: "authorization", wire: true, publishErr: sarama.ErrTopicAuthorizationFailed, diagnostic: "dlq_authorization_failed"},
		{name: "wrapped authentication", publishErr: authErr, diagnostic: "dlq_authentication_failed"},
		{name: "TLS verification", publishErr: tlsErr, diagnostic: "dlq_tls_verification_failed"},
		{name: "invalid topic", publishErr: sarama.ErrInvalidTopic, diagnostic: "dlq_invalid_topic"},
		{name: "configuration", publishErr: configurationErr, diagnostic: "dlq_configuration_error"},
		{name: "custom permanent", publishErr: customErr, diagnostic: "dlq_permanent_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var producer Producer
			var broker *sarama.MockBroker
			if tc.wire {
				broker = sarama.NewMockBroker(t, 1)
				defer broker.Close()
				produce := sarama.NewMockProduceResponse(t)
				if code, ok := tc.publishErr.(sarama.KError); ok {
					produce.SetError(dlq, 0, code)
				}
				broker.SetHandlerByMap(map[string]sarama.MockResponse{
					"ApiVersionsRequest": mockApiVersionsWithSASL(t),
					"MetadataRequest": sarama.NewMockMetadataResponse(t).SetBroker(broker.Addr(), broker.BrokerID()).
						SetLeader(source, 0, broker.BrokerID()).SetLeader(dlq, 0, broker.BrokerID()),
					"ProduceRequest": produce,
				})
				bc := testBrokerConfig()
				bc.Brokers = []string{broker.Addr()}
				pc := DefaultProducerConfig()
				pc.Idempotent = false
				pc.MaxRetries = 0
				if tc.localLimit {
					pc.MaxMessageBytes = maxMessageBytes
				}
				var err error
				producer, err = NewProducer(bc, pc)
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := producer.Close(); err != nil {
						t.Errorf("close DLQ producer: %v", err)
					}
				}()
				if tc.publishErr == ErrProducerClosed {
					if err := producer.Close(); err != nil {
						t.Fatal(err)
					}
				}
				if tc.localLimit {
					if err := producer.Publish(t.Context(), source, nil, payload); err != nil {
						t.Fatalf("source record without DLQ headers should fit: %v", err)
					}
				}
			} else {
				producer = &mockDLQProducer{publishF: func(context.Context, string, []byte, []byte, []RecordHeader) error {
					return tc.publishErr
				}}
			}

			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			fg := newFakeConsumerGroup()
			var session *fakeSession
			fg.consumeFn = func(ctx context.Context, _ []string, h sarama.ConsumerGroupHandler) (err error) {
				session, err = runFakeSession(ctx, h, newFakeClaim(source, 0,
					&sarama.ConsumerMessage{Topic: source, Offset: 0, Value: payload},
					&sarama.ConsumerMessage{Topic: source, Offset: 1},
				))
				return err
			}
			cfg := testConsumerConfig("dlq-permanent")
			cfg.CommitMode = CommitSync
			retries := 0
			var logs bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logs, nil))
			gc, err := NewConsumer(testBrokerConfig(), cfg, logger, func(o *consumerOptions) {
				o.consumerGroupFactory = func([]string, string, *sarama.Config) (consumerGroupClient, error) { return fg, nil }
				o.sleepFn = func(context.Context, time.Duration) error {
					retries++
					cancel()
					return context.Canceled
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := gc.Close(); err != nil {
					t.Errorf("close DLQ consumer: %v", err)
				}
			}()
			var handled []int64
			if err := gc.Subscribe(Subscription{
				Topic: source,
				RetryPolicy: &RetryPolicy{Strategy: RetryStrategyDLQ, DLQTopic: dlq,
					DLQProducer: producer, DLQFailurePolicy: DLQFailureRetry},
				Handler: func(_ context.Context, msg *Message) error {
					handled = append(handled, msg.Offset)
					return handlerErr
				},
			}); err != nil {
				t.Fatal(err)
			}
			err = gc.Run(ctx)
			if retries != 0 || !errors.Is(err, handlerErr) {
				t.Fatalf("expected terminal outcome with handler error and no retry, got %v and %d retries", err, retries)
			}
			if tc.localLimit {
				var configErr sarama.ConfigurationError
				if !errors.As(err, &configErr) {
					t.Fatalf("missing local size configuration error in %v", err)
				}
			} else if !errors.Is(err, tc.publishErr) {
				t.Fatalf("missing publication error %v in %v", tc.publishErr, err)
			}
			if marks, commits := session.snapshotMarks(); len(marks) != 0 || commits != 0 || !slices.Equal(handled, []int64{0}) {
				t.Fatalf("unresolved partition advanced: marks=%v commits=%d handled=%v", marks, commits, handled)
			}
			if health := gc.Health(); health.LastError != "terminal: "+tc.diagnostic || health.State != ConsumerStateFailed {
				t.Fatalf("unexpected terminal health: %+v", health)
			}
			if entries := logs.String(); !strings.Contains(entries, `"failure_category":"`+tc.diagnostic+`"`) || strings.Contains(entries, "sensitive") {
				t.Fatalf("expected safe DLQ failure category %q in logs: %s", tc.diagnostic, entries)
			}
		})
	}
}

func TestTerminalProcessingDiagnostics(t *testing.T) {
	handlerErr := errors.New("sensitive handler details")
	publishErr := errors.New("sensitive publish details")
	for _, tc := range []struct {
		name       string
		handlerErr error
		strategy   RetryStrategy
		diagnostic string
	}{
		{"exhausted retries", handlerErr, RetryStrategyStop, "terminal: consumer_error"},
		{"handler cancellation", context.Canceled, RetryStrategyStop, "terminal: context_canceled"},
		{"permanent handler error", Permanent(WithErrorCode(handlerErr, "invalid_payload")), RetryStrategyInfinite, "terminal: invalid_payload"},
		{"DLQ stop", handlerErr, RetryStrategyDLQ, "terminal: dlq_publish_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fg := newFakeConsumerGroup()
			var healthAtFailure ConsumerHealth
			gc, err := NewConsumer(testBrokerConfig(), testConsumerConfig("terminal-diagnostic"), discardLogger(), func(o *consumerOptions) {
				o.consumerGroupFactory = func([]string, string, *sarama.Config) (consumerGroupClient, error) { return fg, nil }
				o.sleepFn = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
			})
			if err != nil {
				t.Fatal(err)
			}
			defer gc.Close()
			fg.consumeFn = func(ctx context.Context, _ []string, handler sarama.ConsumerGroupHandler) error {
				claim := newFakeClaim("orders", 0, &sarama.ConsumerMessage{Topic: "orders"})
				_, err := runFakeSession(ctx, handler, claim)
				healthAtFailure = gc.Health()
				fg.errCh <- &sarama.ConsumerError{Topic: "orders", Err: sarama.ErrNotConnected}
				return err
			}
			policy := &RetryPolicy{Strategy: tc.strategy, MaxRetries: 1}
			if tc.strategy == RetryStrategyDLQ {
				policy.DLQTopic = "orders-dlq"
				policy.DLQProducer = &mockDLQProducer{publishF: func(context.Context, string, []byte, []byte, []RecordHeader) error {
					return publishErr
				}}
				policy.DLQFailurePolicy = DLQFailureStop
			}
			if err := gc.Subscribe(Subscription{
				Topic: "orders", RetryPolicy: policy,
				Handler: func(context.Context, *Message) error { return tc.handlerErr },
			}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			err = gc.Run(ctx)
			if !errors.Is(err, tc.handlerErr) || (tc.strategy == RetryStrategyDLQ && !errors.Is(err, publishErr)) {
				t.Fatalf("Run did not preserve the original error chain: %v", err)
			}
			if health := gc.Health(); health.State != ConsumerStateFailed || health.Ready || health.LastError != tc.diagnostic ||
				health.LastErrorAt.IsZero() || !health.LastErrorAt.Equal(healthAtFailure.LastErrorAt) {
				t.Fatalf("expected %q to survive the delayed transient error; original %+v, final %+v", tc.diagnostic, healthAtFailure, health)
			}
		})
	}
}

func TestReadinessClaimInitializationAndHealthTimestamps(t *testing.T) {
	t.Run("F04 readiness waits for all assigned claims to enter ConsumeClaim even on idle partitions", func(t *testing.T) {
		fg := newFakeConsumerGroup()
		setupDone := make(chan struct{})
		p0Entered := make(chan struct{})
		allowP1Start := make(chan struct{})
		p1Entered := make(chan struct{})
		allowSessionExit := make(chan struct{})

		fg.consumeFn = func(runCtx context.Context, topics []string, handler sarama.ConsumerGroupHandler) error {
			sess := newFakeSession(runCtx, map[string][]int32{"t1": {0, 1}})
			if err := handler.Setup(sess); err != nil {
				return err
			}
			close(setupDone)

			// Idle claims (no messages sent) still mark readiness once ConsumeClaim begins.
			c0 := newFakeClaim("t1", 0)
			c1 := newFakeClaim("t1", 1)

			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				close(p0Entered)
				_ = handler.ConsumeClaim(sess, c0)
			}()
			go func() {
				defer wg.Done()
				<-allowP1Start
				close(p1Entered)
				_ = handler.ConsumeClaim(sess, c1)
			}()

			<-allowSessionExit
			<-runCtx.Done()
			wg.Wait()
			return handler.Cleanup(sess)
		}

		gc, err := NewConsumer(testBrokerConfig(), testConsumerConfig("grp-f04"), discardLogger(), func(o *consumerOptions) {
			o.consumerGroupFactory = func([]string, string, *sarama.Config) (consumerGroupClient, error) { return fg, nil }
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := gc.Subscribe(Subscription{
			Topic:       "t1",
			RetryPolicy: DefaultRetryPolicy(),
			Handler:     func(context.Context, *Message) error { return nil },
		}); err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		runDone := make(chan error, 1)
		go func() { runDone <- gc.Run(ctx) }()

		<-setupDone
		// Immediately after Setup with 2 assigned claims, Ready must still be false.
		if h := gc.Health(); h.Ready || h.AssignedClaims != 2 {
			t.Fatalf("expected Ready=false and AssignedClaims=2 right after Setup, got %+v", h)
		}

		// After only 1 of 2 claims enters ConsumeClaim, Ready must still be false.
		<-p0Entered
		time.Sleep(15 * time.Millisecond)
		if h := gc.Health(); h.Ready {
			t.Fatalf("expected Ready=false while claim 1 is not yet initialized, got %+v", h)
		}

		// Once claim 1 enters ConsumeClaim (even with zero messages), Ready becomes true.
		close(allowP1Start)
		<-p1Entered
		deadline := time.Now().Add(2 * time.Second)
		for !gc.Health().Ready {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for Ready=true after all claims initialized, got %+v", gc.Health())
			}
			time.Sleep(5 * time.Millisecond)
		}

		close(allowSessionExit)
		cancel()
		if err := <-runDone; err != nil {
			t.Fatalf("Run: %v", err)
		}
		if gc.Health().Ready {
			t.Fatalf("expected Ready=false after shutdown")
		}
	})

	t.Run("F09 LastResolvedAt vs LastSuccessfulAt distinguish discard/DLQ from business success", func(t *testing.T) {
		fg := newFakeConsumerGroup()
		fg.consumeFn = func(runCtx context.Context, topics []string, handler sarama.ConsumerGroupHandler) error {
			c := newFakeClaim("t1", 0,
				&sarama.ConsumerMessage{Topic: "t1", Partition: 0, Offset: 1, Value: []byte("discard")},
				&sarama.ConsumerMessage{Topic: "t1", Partition: 0, Offset: 2, Value: []byte("ok")},
			)
			_, err := runFakeSession(runCtx, handler, c)
			return err
		}

		gc, err := NewConsumer(testBrokerConfig(), testConsumerConfig("grp-f09"), discardLogger(), func(o *consumerOptions) {
			o.consumerGroupFactory = func([]string, string, *sarama.Config) (consumerGroupClient, error) { return fg, nil }
		})
		if err != nil {
			t.Fatal(err)
		}

		var snapAfterDiscard ConsumerHealth
		ctx, cancel := context.WithCancel(context.Background())
		var step atomic.Int32
		if err := gc.Subscribe(Subscription{
			Topic:       "t1",
			RetryPolicy: &RetryPolicy{Strategy: RetryStrategyLogAndIgnore, MaxRetries: 0},
			Handler: func(c context.Context, msg *Message) error {
				if step.Add(1) == 1 {
					return errors.New("fail first")
				}
				// Captured right before msg 2 resolves: msg 1 was discarded, so LastResolvedAt is set, LastSuccessfulAt is zero.
				snapAfterDiscard = gc.Health()
				go cancel()
				return nil
			},
		}); err != nil {
			t.Fatal(err)
		}

		if err := gc.Run(ctx); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if snapAfterDiscard.LastResolvedAt.IsZero() || !snapAfterDiscard.LastSuccessfulAt.IsZero() {
			t.Fatalf("after discard expected LastResolvedAt non-zero and LastSuccessfulAt zero, got %+v", snapAfterDiscard)
		}
		finalH := gc.Health()
		if finalH.LastResolvedAt.IsZero() || finalH.LastSuccessfulAt.IsZero() {
			t.Fatalf("after success expected both LastResolvedAt and LastSuccessfulAt set, got %+v", finalH)
		}
	})
}

func TestInvalidOffsetFailClosedAndRetryStopAndShutdownBound(t *testing.T) {
	t.Run("F05 ResetInvalidOffsets=false fails fast on ErrOffsetOutOfRange instead of looping", func(t *testing.T) {
		fg := newFakeConsumerGroup()
		var consumeCalls atomic.Int32
		fg.consumeFn = func(runCtx context.Context, topics []string, handler sarama.ConsumerGroupHandler) error {
			consumeCalls.Add(1)
			fg.errCh <- &sarama.ConsumerError{Topic: "t1", Partition: 0, Err: sarama.ErrOffsetOutOfRange}
			<-runCtx.Done()
			return nil
		}

		cfg := testConsumerConfig("grp-f05")
		cfg.ResetInvalidOffsets = false
		gc, err := NewConsumer(testBrokerConfig(), cfg, discardLogger(), func(o *consumerOptions) {
			o.consumerGroupFactory = func([]string, string, *sarama.Config) (consumerGroupClient, error) { return fg, nil }
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := gc.Subscribe(Subscription{
			Topic:       "t1",
			RetryPolicy: DefaultRetryPolicy(),
			Handler:     func(context.Context, *Message) error { return nil },
		}); err != nil {
			t.Fatal(err)
		}

		err = gc.Run(context.Background())
		if !errors.Is(err, sarama.ErrOffsetOutOfRange) {
			t.Fatalf("expected ErrOffsetOutOfRange, got %v", err)
		}
		if consumeCalls.Load() != 1 {
			t.Fatalf("expected single Consume without rebalance loop, got %d", consumeCalls.Load())
		}
		if h := gc.Health(); h.State != ConsumerStateFailed || h.Ready {
			t.Fatalf("expected Failed/unready health, got %+v", h)
		}
	})

	t.Run("F07_F08 RetryStrategyStop and Permanent under Infinite stop without marking", func(t *testing.T) {
		for _, tc := range []struct {
			name        string
			policy      *RetryPolicy
			err         error
			wantOffsets []int64
		}{
			{
				name:        "RetryStrategyStop",
				policy:      &RetryPolicy{Strategy: RetryStrategyStop, MaxRetries: 1, InitialDelay: time.Millisecond, MaxDelay: time.Millisecond, Multiplier: 1},
				err:         errors.New("transient exhausted"),
				wantOffsets: []int64{10, 10},
			},
			{
				name:        "Infinite+Permanent",
				policy:      &RetryPolicy{Strategy: RetryStrategyInfinite},
				err:         Permanent(errors.New("bad payload")),
				wantOffsets: []int64{10},
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				fg := newFakeConsumerGroup()
				var capturedSess *fakeSession
				fg.consumeFn = func(runCtx context.Context, topics []string, handler sarama.ConsumerGroupHandler) error {
					c := newFakeClaim("orders", 0,
						&sarama.ConsumerMessage{Topic: "orders", Partition: 0, Offset: 10},
						&sarama.ConsumerMessage{Topic: "orders", Partition: 0, Offset: 11},
					)
					var err error
					capturedSess, err = runFakeSession(runCtx, handler, c)
					return err
				}
				gc, err := NewConsumer(testBrokerConfig(), testConsumerConfig("grp-"+tc.name), discardLogger(), func(o *consumerOptions) {
					o.consumerGroupFactory = func([]string, string, *sarama.Config) (consumerGroupClient, error) { return fg, nil }
					o.sleepFn = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
					o.jitterFn = func(d time.Duration) time.Duration { return d }
				})
				if err != nil {
					t.Fatal(err)
				}
				var seenOffsets []int64
				if err := gc.Subscribe(Subscription{
					Topic:       "orders",
					RetryPolicy: tc.policy,
					Handler: func(ctx context.Context, msg *Message) error {
						seenOffsets = append(seenOffsets, msg.Offset)
						return tc.err
					},
				}); err != nil {
					t.Fatal(err)
				}
				runErr := gc.Run(context.Background())
				if !errors.Is(runErr, tc.err) {
					t.Fatalf("expected error %v from Run, got %v", tc.err, runErr)
				}
				if !slices.Equal(seenOffsets, tc.wantOffsets) {
					t.Fatalf("expected offsets %v processed (never offset 11), got %v", tc.wantOffsets, seenOffsets)
				}
				marks, _ := capturedSess.snapshotMarks()
				if len(marks) != 0 {
					t.Fatalf("failed offset must not be marked, got %+v", marks)
				}
			})
		}
	})

	t.Run("F01 repeated shutdowns under stalled CloseTimeout bound exit and leak zero goroutines", func(t *testing.T) {
		// Warm up any background stdlib goroutines before taking baseline.
		baseline := runtime.NumGoroutine()
		for i := 0; i < 5; i++ {
			fg := newFakeConsumerGroup()
			consumed := make(chan struct{})
			fg.consumeFn = func(runCtx context.Context, topics []string, handler sarama.ConsumerGroupHandler) error {
				close(consumed)
				<-runCtx.Done()
				return nil
			}
			cfg := testConsumerConfig(fmt.Sprintf("grp-repeat-%d", i))
			cfg.ShutdownTimeout = 20 * time.Millisecond
			cfg.CloseTimeout = 30 * time.Millisecond
			gc, err := NewConsumer(testBrokerConfig(), cfg, discardLogger(), func(o *consumerOptions) {
				o.consumerGroupFactory = func([]string, string, *sarama.Config) (consumerGroupClient, error) { return fg, nil }
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := gc.Subscribe(Subscription{
				Topic:       "t1",
				RetryPolicy: DefaultRetryPolicy(),
				Handler:     func(context.Context, *Message) error { return nil },
			}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- gc.Run(ctx) }()
			<-consumed
			cancel()
			if err := <-done; err != nil {
				t.Fatalf("iter %d Run: %v", i, err)
			}
			if err := gc.Close(); err != nil {
				t.Fatalf("iter %d Close: %v", i, err)
			}
		}

		// Allow scheduler to settle and verify no goroutine growth.
		deadline := time.Now().Add(time.Second)
		for runtime.NumGoroutine() > baseline+2 && time.Now().Before(deadline) {
			runtime.Gosched()
			time.Sleep(10 * time.Millisecond)
		}
		if got := runtime.NumGoroutine(); got > baseline+2 {
			t.Fatalf("goroutine leak suspected after repeated shutdowns: baseline=%d current=%d", baseline, got)
		}
	})
}
