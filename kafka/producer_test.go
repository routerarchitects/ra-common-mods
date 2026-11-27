package kafka

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IBM/sarama"
)

type fakeSyncProducer struct {
	mu       sync.Mutex
	sent     []*sarama.ProducerMessage
	sendFn   func(*sarama.ProducerMessage) (int32, int64, error)
	closed   atomic.Bool
	closeCnt atomic.Int32
	closeErr error
}

func (f *fakeSyncProducer) SendMessage(msg *sarama.ProducerMessage) (int32, int64, error) {
	f.mu.Lock()
	f.sent = append(f.sent, msg)
	offset := int64(len(f.sent))
	fn := f.sendFn
	f.mu.Unlock()
	if fn != nil {
		return fn(msg)
	}
	return 0, offset, nil
}

func (f *fakeSyncProducer) sentMessages() []*sarama.ProducerMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*sarama.ProducerMessage, len(f.sent))
	copy(out, f.sent)
	return out
}

func (f *fakeSyncProducer) sentCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func (f *fakeSyncProducer) Close() error {
	f.closed.Store(true)
	f.closeCnt.Add(1)
	return f.closeErr
}

func newTestProducer(t *testing.T, f *fakeSyncProducer, mutate func(*ProducerConfig)) Producer {
	t.Helper()
	b := DefaultBrokerConfig()
	b.Brokers = []string{"localhost:9092"}
	p := DefaultProducerConfig()
	if mutate != nil {
		mutate(&p)
	}
	prod, err := NewProducer(b, p, func(o *producerOptions) {
		o.syncProducerFactory = func([]string, *sarama.Config) (syncProducerClient, error) {
			return f, nil
		}
	})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	return prod
}

func TestProducerNilEmptyAndHashKeys(t *testing.T) {
	f := &fakeSyncProducer{}
	p := newTestProducer(t, f, nil)
	defer p.Close()

	// 1. nil key must produce a true nil sarama.Encoder interface (not typed nil ByteEncoder).
	if err := p.Publish(context.Background(), "topic-a", nil, []byte("v-nil")); err != nil {
		t.Fatalf("Publish nil key: %v", err)
	}
	// 2. Non-nil empty slice must be preserved as a non-nil Encoder encoding to 0 bytes.
	if err := p.Publish(context.Background(), "topic-a", []byte{}, []byte("v-empty")); err != nil {
		t.Fatalf("Publish empty key: %v", err)
	}
	// 3. Non-empty key must preserve bytes and partition deterministically with Sarama's hash partitioner.
	if err := p.Publish(context.Background(), "topic-a", []byte("device-42"), []byte("v-keyed")); err != nil {
		t.Fatalf("Publish keyed: %v", err)
	}
	if err := p.Publish(context.Background(), "topic-a", []byte("device-42"), []byte("v-keyed-2")); err != nil {
		t.Fatalf("Publish keyed 2: %v", err)
	}

	msgs := f.sentMessages()
	if len(msgs) != 4 {
		t.Fatalf("expected 4 sent messages, got %d", len(msgs))
	}

	if msgs[0].Key != nil {
		t.Fatalf("expected nil Key interface for nil byte slice, got %T (%v)", msgs[0].Key, msgs[0].Key)
	}
	if msgs[1].Key == nil {
		t.Fatalf("expected non-nil Key interface for explicit empty []byte{}")
	}
	if enc, err := msgs[1].Key.Encode(); err != nil || len(enc) != 0 {
		t.Fatalf("expected empty encoded key, got len=%d err=%v", len(enc), err)
	}
	if enc, err := msgs[2].Key.Encode(); err != nil || string(enc) != "device-42" {
		t.Fatalf("expected key device-42, got %q err=%v", string(enc), err)
	}

	// Verify Sarama's default HashPartitioner treats nil key as random and non-nil key as deterministic.
	part := sarama.NewHashPartitioner("topic-a")
	const numPartitions = int32(16)
	nilPartitions := make(map[int32]bool)
	for i := 0; i < 64; i++ {
		pIdx, err := part.Partition(msgs[0], numPartitions)
		if err != nil {
			t.Fatal(err)
		}
		nilPartitions[pIdx] = true
	}
	if len(nilPartitions) <= 1 {
		t.Fatalf("expected nil Key to distribute across partitions via random partitioner, got %v", nilPartitions)
	}

	firstPart, err := part.Partition(msgs[2], numPartitions)
	if err != nil {
		t.Fatal(err)
	}
	secondPart, err := part.Partition(msgs[3], numPartitions)
	if err != nil {
		t.Fatal(err)
	}
	if firstPart != secondPart {
		t.Fatalf("expected deterministic partition for identical non-nil key, got %d vs %d", firstPart, secondPart)
	}

	// F15: Verify CRC32 (librdkafka compatible) and Murmur2 partitioners handle nil vs empty vs keyed consistently.
	for name, ctor := range map[string]sarama.PartitionerConstructor{
		"crc32":   sarama.NewConsistentCRCHashPartitioner,
		"murmur2": sarama.NewMurmur2Partitioner,
	} {
		part := ctor("topic-a")
		p1, err := part.Partition(msgs[2], numPartitions)
		if err != nil {
			t.Fatalf("%s Partition 1: %v", name, err)
		}
		p2, err := part.Partition(msgs[3], numPartitions)
		if err != nil {
			t.Fatalf("%s Partition 2: %v", name, err)
		}
		if p1 != p2 {
			t.Fatalf("%s deterministic partition mismatch: %d vs %d", name, p1, p2)
		}
		pe1, err := part.Partition(msgs[1], numPartitions)
		if err != nil {
			t.Fatalf("%s empty key: %v", name, err)
		}
		pe2, err := part.Partition(msgs[1], numPartitions)
		if err != nil || pe1 != pe2 {
			t.Fatalf("%s empty non-nil key must be deterministic", name)
		}
	}
}

func TestProducerContextAndConcurrencyAndClose(t *testing.T) {
	t.Run("cancelled context before Publish and PublishJSON skips SendMessage", func(t *testing.T) {
		f := &fakeSyncProducer{}
		p := newTestProducer(t, f, nil)
		defer p.Close()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if err := p.Publish(ctx, "topic-a", []byte("k"), []byte("v")); !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}

		// PublishJSON must check context before marshaling even an unmarshalable value.
		unmarshalable := make(chan int)
		if err := p.PublishJSON(ctx, "topic-a", "k", unmarshalable); !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled before JSON marshal, got %v", err)
		}

		deadlineCtx, deadlineCancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer deadlineCancel()
		if err := p.PublishWithHeaders(deadlineCtx, "topic-a", []byte("k"), []byte("v"), nil); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected context.DeadlineExceeded, got %v", err)
		}

		if f.sentCount() != 0 {
			t.Fatalf("expected 0 sends on cancelled contexts, got %d", f.sentCount())
		}
	})

	t.Run("preserves real send outcome after late context cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		f := &fakeSyncProducer{
			sendFn: func(msg *sarama.ProducerMessage) (int32, int64, error) {
				cancel()
				return 1, 42, nil
			},
		}
		p := newTestProducer(t, f, nil)
		defer p.Close()

		if err := p.Publish(ctx, "topic-a", []byte("k"), []byte("v")); err != nil {
			t.Fatalf("expected nil when started SendMessage succeeded, got %v", err)
		}
	})

	t.Run("invalid topic names rejected before marshal or SendMessage", func(t *testing.T) {
		f := &fakeSyncProducer{}
		p := newTestProducer(t, f, nil)
		defer p.Close()

		for _, badTopic := range []string{" ", ".", "..", "a/b", "bad topic", string(make([]byte, 250))} {
			if err := p.Publish(context.Background(), badTopic, []byte("k"), []byte("v")); err == nil {
				t.Fatalf("expected error for invalid topic %q", badTopic)
			}
			if err := p.PublishJSON(context.Background(), badTopic, "k", make(chan int)); err == nil {
				t.Fatalf("expected PublishJSON error for invalid topic %q", badTopic)
			}
		}
		if f.sentCount() != 0 {
			t.Fatalf("expected 0 sends for invalid topics, got %d", f.sentCount())
		}

		callerHeaders := []RecordHeader{{Key: "h1", Value: []byte("v1")}}
		if err := p.PublishWithHeaders(context.Background(), "topic-a", []byte("k"), []byte("v"), callerHeaders); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		callerHeaders[0].Value[0] = 'X'

		msgs := f.sentMessages()
		if string(msgs[0].Headers[0].Value) != "v1" {
			t.Fatalf("caller mutation affected sent header")
		}
	})

	t.Run("close coordinates with active sends and is idempotent", func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		f := &fakeSyncProducer{
			sendFn: func(msg *sarama.ProducerMessage) (int32, int64, error) {
				close(started)
				<-release
				return 0, 1, nil
			},
		}
		p := newTestProducer(t, f, nil)

		var wg sync.WaitGroup
		wg.Add(1)
		var sendErr error
		go func() {
			defer wg.Done()
			sendErr = p.Publish(context.Background(), "topic-a", []byte("k"), []byte("v"))
		}()

		<-started
		closeDone := make(chan error, 2)
		go func() { closeDone <- p.Close() }()
		go func() { closeDone <- p.Close() }()

		select {
		case err := <-closeDone:
			t.Fatalf("Close returned before active SendMessage completed: %v", err)
		case <-time.After(20 * time.Millisecond):
		}

		close(release)
		wg.Wait()
		if sendErr != nil {
			t.Fatalf("active send failed: %v", sendErr)
		}
		for i := 0; i < 2; i++ {
			if err := <-closeDone; err != nil {
				t.Fatalf("Close returned error: %v", err)
			}
		}
		if f.closeCnt.Load() != 1 {
			t.Fatalf("expected single underlying Close, got %d", f.closeCnt.Load())
		}

		if err := p.Publish(context.Background(), "topic-a", []byte("k"), []byte("v")); !errors.Is(err, ErrProducerClosed) {
			t.Fatalf("expected ErrProducerClosed, got %v", err)
		}
	})

	t.Run("multiple simultaneous publishers with concurrent Close", func(t *testing.T) {
		var inFlight atomic.Int32
		firstStarted := make(chan struct{})
		var firstOnce sync.Once
		release := make(chan struct{})

		f := &fakeSyncProducer{
			sendFn: func(msg *sarama.ProducerMessage) (int32, int64, error) {
				inFlight.Add(1)
				firstOnce.Do(func() { close(firstStarted) })
				<-release
				inFlight.Add(-1)
				return 0, 1, nil
			},
		}
		p := newTestProducer(t, f, nil)

		const workers = 16
		const perWorker = 5
		var pubWg sync.WaitGroup
		var succeeded, closedErrs atomic.Int32
		var unexpectedErr atomic.Value

		for w := 0; w < workers; w++ {
			pubWg.Add(1)
			go func(id int) {
				defer pubWg.Done()
				for i := 0; i < perWorker; i++ {
					err := p.Publish(context.Background(), "topic-a", []byte(fmt.Sprintf("k-%d", id)), []byte("v"))
					if err == nil {
						succeeded.Add(1)
					} else if errors.Is(err, ErrProducerClosed) {
						closedErrs.Add(1)
					} else {
						unexpectedErr.Store(err)
					}
				}
			}(w)
		}

		<-firstStarted
		var closeWg sync.WaitGroup
		for c := 0; c < 4; c++ {
			closeWg.Add(1)
			go func() {
				defer closeWg.Done()
				if err := p.Close(); err != nil {
					unexpectedErr.Store(err)
				}
			}()
		}

		close(release)
		closeWg.Wait()
		pubWg.Wait()

		if v := unexpectedErr.Load(); v != nil {
			t.Fatalf("unexpected error during concurrent publish/close: %v", v)
		}
		if f.closeCnt.Load() != 1 {
			t.Fatalf("expected single underlying Close, got %d", f.closeCnt.Load())
		}
		if succeeded.Load()+closedErrs.Load() != workers*perWorker {
			t.Fatalf("missing publish outcomes: ok=%d closed=%d", succeeded.Load(), closedErrs.Load())
		}
		if int(succeeded.Load()) != f.sentCount() {
			t.Fatalf("succeeded=%d != sentCount=%d", succeeded.Load(), f.sentCount())
		}
		if inFlight.Load() != 0 {
			t.Fatalf("expected 0 inFlight after Close, got %d", inFlight.Load())
		}
	})
}
