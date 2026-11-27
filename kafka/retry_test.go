package kafka

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type mockDLQProducer struct {
	mu       sync.Mutex
	calls    []mockPublishCall
	publishF func(ctx context.Context, topic string, key, value []byte, headers []RecordHeader) error
}

type mockPublishCall struct {
	Topic   string
	Key     []byte
	Value   []byte
	Headers []RecordHeader
}

func (m *mockDLQProducer) Publish(ctx context.Context, topic string, key, value []byte) error {
	return m.PublishWithHeaders(ctx, topic, key, value, nil)
}

func (m *mockDLQProducer) PublishWithHeaders(ctx context.Context, topic string, key, value []byte, headers []RecordHeader) error {
	m.mu.Lock()
	m.calls = append(m.calls, mockPublishCall{
		Topic:   topic,
		Key:     cloneBytesPreserveNil(key),
		Value:   cloneBytesPreserveNil(value),
		Headers: cloneHeaders(headers),
	})
	fn := m.publishF
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, topic, key, value, headers)
	}
	return nil
}

func (m *mockDLQProducer) PublishJSON(ctx context.Context, topic string, key string, data interface{}) error {
	return nil
}

func (m *mockDLQProducer) Close() error { return nil }

func newNoopRetryRuntime(sleeps *[]time.Duration) *retryRuntime {
	return &retryRuntime{
		groupID:  "test-group",
		clientID: "test-client",
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		sleepFn: func(ctx context.Context, d time.Duration) error {
			if sleeps != nil {
				*sleeps = append(*sleeps, d)
			}
			return ctx.Err()
		},
		jitterFn: func(delay time.Duration) time.Duration {
			return delay
		},
	}
}

func TestRetryPolicyValidation(t *testing.T) {
	validDLQ := &mockDLQProducer{}
	zeroAcksProd := newTestProducer(t, &fakeSyncProducer{}, func(p *ProducerConfig) {
		p.Idempotent = false
		p.RequiredAcks = 0
	})
	defer zeroAcksProd.Close()

	// Nil policy is explicitly rejected (F03); DefaultRetryPolicy() remains available as explicit opt-in.
	if _, err := normalizeAndValidateRetryPolicy("src", nil); err == nil {
		t.Fatalf("expected nil retry policy to be rejected")
	}
	if _, err := normalizeAndValidateRetryPolicy("src", &RetryPolicy{MaxRetries: 2}); err == nil {
		t.Fatalf("expected empty retry strategy to be rejected")
	}
	p1, err := normalizeAndValidateRetryPolicy("src", DefaultRetryPolicy())
	if err != nil {
		t.Fatalf("DefaultRetryPolicy failed: err=%v", err)
	}
	if p1.Strategy != RetryStrategyLogAndIgnore || p1.MaxRetries != 3 ||
		p1.InitialDelay != 100*time.Millisecond || p1.MaxDelay != 30*time.Second || p1.Multiplier != 2.0 {
		t.Fatalf("unexpected default retry policy: %+v", p1)
	}

	// RetryStrategyStop is valid without DLQ fields (F07).
	pStop, err := normalizeAndValidateRetryPolicy("src", &RetryPolicy{Strategy: RetryStrategyStop, MaxRetries: 2})
	if err != nil {
		t.Fatalf("RetryStrategyStop failed validation: %v", err)
	}
	if pStop.Strategy != RetryStrategyStop || pStop.MaxRetries != 2 {
		t.Fatalf("unexpected stop policy: %+v", pStop)
	}

	// Explicit zero MaxRetries must NOT be overwritten to 3.
	pZero, err := normalizeAndValidateRetryPolicy("src", &RetryPolicy{Strategy: RetryStrategyLogAndIgnore, MaxRetries: 0})
	if err != nil {
		t.Fatalf("explicit zero MaxRetries failed: %v", err)
	}
	if pZero.MaxRetries != 0 {
		t.Fatalf("expected MaxRetries=0 preserved, got %d", pZero.MaxRetries)
	}

	tests := []struct {
		name    string
		policy  *RetryPolicy
		wantErr string
	}{
		{
			name:    "negative max retries",
			policy:  &RetryPolicy{Strategy: RetryStrategyInfinite, MaxRetries: -1},
			wantErr: "cannot be negative",
		},
		{
			name:    "invalid strategy",
			policy:  &RetryPolicy{Strategy: RetryStrategy("drop")},
			wantErr: "invalid retry strategy",
		},
		{
			name:    "max delay below initial delay",
			policy:  &RetryPolicy{Strategy: RetryStrategyLogAndIgnore, InitialDelay: time.Second, MaxDelay: 100 * time.Millisecond},
			wantErr: "cannot be less than initial_delay",
		},
		{
			name:    "multiplier below 1",
			policy:  &RetryPolicy{Strategy: RetryStrategyLogAndIgnore, Multiplier: 0.5},
			wantErr: "multiplier",
		},
		{
			name:    "multiplier NaN",
			policy:  &RetryPolicy{Strategy: RetryStrategyLogAndIgnore, Multiplier: math.NaN()},
			wantErr: "multiplier",
		},
		{
			name:    "multiplier Inf",
			policy:  &RetryPolicy{Strategy: RetryStrategyLogAndIgnore, Multiplier: math.Inf(1)},
			wantErr: "multiplier",
		},
		{
			name:    "dlq missing topic",
			policy:  &RetryPolicy{Strategy: RetryStrategyDLQ, DLQProducer: validDLQ, DLQFailurePolicy: DLQFailureDiscard},
			wantErr: "DLQ topic",
		},
		{
			name:    "dlq invalid topic characters",
			policy:  &RetryPolicy{Strategy: RetryStrategyDLQ, DLQTopic: "orders/dlq", DLQProducer: validDLQ, DLQFailurePolicy: DLQFailureDiscard},
			wantErr: "invalid DLQ topic",
		},
		{
			name:    "dlq self topic rejected",
			policy:  &RetryPolicy{Strategy: RetryStrategyDLQ, DLQTopic: "src", DLQProducer: validDLQ, DLQFailurePolicy: DLQFailureDiscard},
			wantErr: "cannot be identical to source topic",
		},
		{
			name:    "dlq nil producer",
			policy:  &RetryPolicy{Strategy: RetryStrategyDLQ, DLQTopic: "dlq", DLQFailurePolicy: DLQFailureDiscard},
			wantErr: "DLQ producer",
		},
		{
			name:    "dlq typed nil producer",
			policy:  &RetryPolicy{Strategy: RetryStrategyDLQ, DLQTopic: "dlq", DLQProducer: (*mockDLQProducer)(nil), DLQFailurePolicy: DLQFailureDiscard},
			wantErr: "DLQ producer",
		},
		{
			name:    "dlq producer with required_acks=0 rejected",
			policy:  &RetryPolicy{Strategy: RetryStrategyDLQ, DLQTopic: "dlq", DLQProducer: zeroAcksProd, DLQFailurePolicy: DLQFailureDiscard},
			wantErr: "required_acks=0",
		},
		{
			name:    "dlq missing fallback rejected",
			policy:  &RetryPolicy{Strategy: RetryStrategyDLQ, DLQTopic: "dlq", DLQProducer: validDLQ},
			wantErr: "DLQ failure policy",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := normalizeAndValidateRetryPolicy("src", tc.policy)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestRetryProcessingOutcomes(t *testing.T) {
	baseMsg := func() *Message {
		return &Message{
			Topic:     "orders",
			Partition: 2,
			Offset:    99,
			Key:       []byte("k1"),
			Value:     []byte("v1"),
			Headers: []RecordHeader{
				{Key: "traceparent", Value: []byte("00-abc-def-01")},
				{Key: "x-custom", Value: []byte("one")},
				{Key: "x-custom", Value: []byte("two")},
				{Key: HeaderOriginalTopic, Value: []byte("stale-topic")},
			},
			Timestamp: time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC),
		}
	}

	t.Run("UT02 fail twice then succeed with MaxRetries=3", func(t *testing.T) {
		var sleeps []time.Duration
		rt := newNoopRetryRuntime(&sleeps)
		policy, err := normalizeAndValidateRetryPolicy("orders", &RetryPolicy{
			Strategy:     RetryStrategyLogAndIgnore,
			MaxRetries:   3,
			InitialDelay: 10 * time.Millisecond,
			MaxDelay:     time.Second,
			Multiplier:   2,
		})
		if err != nil {
			t.Fatal(err)
		}

		var calls int
		res := rt.processMessage(context.Background(), baseMsg(), func(ctx context.Context, msg *Message) error {
			calls++
			if calls <= 2 {
				return errors.New("transient")
			}
			return nil
		}, policy, time.Second)

		if res.Outcome != outcomeProcessed || res.Attempts != 3 || calls != 3 || len(sleeps) != 2 {
			t.Fatalf("unexpected result: %+v calls=%d sleeps=%v", res, calls, sleeps)
		}
	})

	t.Run("UT03 bounded ignore exhausts 1+MaxRetries attempts and discards", func(t *testing.T) {
		var sleeps []time.Duration
		rt := newNoopRetryRuntime(&sleeps)
		policy, err := normalizeAndValidateRetryPolicy("orders", &RetryPolicy{
			Strategy:     RetryStrategyLogAndIgnore,
			MaxRetries:   3,
			InitialDelay: 10 * time.Millisecond,
			MaxDelay:     time.Second,
			Multiplier:   2,
		})
		if err != nil {
			t.Fatal(err)
		}

		var calls int
		res := rt.processMessage(context.Background(), baseMsg(), func(ctx context.Context, msg *Message) error {
			calls++
			return errors.New("always fails")
		}, policy, time.Second)

		if res.Outcome != outcomeDiscarded || res.DiscardReason != "handler_exhausted" ||
			res.Attempts != 4 || calls != 4 || len(sleeps) != 3 {
			t.Fatalf("unexpected result: %+v calls=%d sleeps=%v", res, calls, sleeps)
		}
	})

	t.Run("UT06 explicit zero retries executes exactly once", func(t *testing.T) {
		var sleeps []time.Duration
		rt := newNoopRetryRuntime(&sleeps)
		policy, err := normalizeAndValidateRetryPolicy("orders", &RetryPolicy{
			Strategy:   RetryStrategyLogAndIgnore,
			MaxRetries: 0,
		})
		if err != nil {
			t.Fatal(err)
		}

		var calls int
		res := rt.processMessage(context.Background(), baseMsg(), func(ctx context.Context, msg *Message) error {
			calls++
			return errors.New("fail")
		}, policy, time.Second)

		if res.Outcome != outcomeDiscarded || calls != 1 || len(sleeps) != 0 {
			t.Fatalf("expected 1 call and 0 sleeps, got calls=%d sleeps=%d res=%+v", calls, len(sleeps), res)
		}
	})

	t.Run("UT04_UT12 DLQ success preserves payload and sets unique reserved metadata without mutating original", func(t *testing.T) {
		var sleeps []time.Duration
		rt := newNoopRetryRuntime(&sleeps)
		dlq := &mockDLQProducer{}
		policy, err := normalizeAndValidateRetryPolicy("orders", &RetryPolicy{
			Strategy:         RetryStrategyDLQ,
			MaxRetries:       1,
			InitialDelay:     5 * time.Millisecond,
			MaxDelay:         time.Second,
			Multiplier:       2,
			DLQTopic:         "orders-dlq",
			DLQProducer:      dlq,
			DLQFailurePolicy: DLQFailureStop,
		})
		if err != nil {
			t.Fatal(err)
		}

		orig := baseMsg()
		res := rt.processMessage(context.Background(), orig, func(ctx context.Context, msg *Message) error {
			return errors.New("bad\npayload")
		}, policy, time.Second)

		if res.Outcome != outcomeDeadLettered || res.Attempts != 2 || len(dlq.calls) != 1 {
			t.Fatalf("unexpected result: %+v dlqCalls=%d", res, len(dlq.calls))
		}
		// Original headers must remain untouched.
		v, ok := orig.GetHeader(HeaderOriginalTopic)
		if !ok || string(v) != "stale-topic" || len(orig.Headers) != 4 {
			t.Fatalf("original message headers were mutated: %+v", orig.Headers)
		}

		sent := dlq.calls[0]
		if sent.Topic != "orders-dlq" || string(sent.Key) != "k1" || string(sent.Value) != "v1" {
			t.Fatalf("unexpected DLQ record payload/key/topic: %+v", sent)
		}

		counts := map[string][]string{}
		for _, h := range sent.Headers {
			counts[h.Key] = append(counts[h.Key], string(h.Value))
		}
		if len(counts["x-custom"]) != 2 || counts["x-custom"][0] != "one" || counts["x-custom"][1] != "two" {
			t.Fatalf("duplicate unrelated headers not preserved: %v", counts["x-custom"])
		}
		if len(counts[HeaderOriginalTopic]) != 1 || counts[HeaderOriginalTopic][0] != "orders" {
			t.Fatalf("expected unique x-original-topic=orders, got %v", counts[HeaderOriginalTopic])
		}
		if counts[HeaderOriginalPartition][0] != "2" ||
			counts[HeaderOriginalOffset][0] != "99" ||
			counts[HeaderOriginalConsumerGroup][0] != "test-group" ||
			counts[HeaderOriginalAttempts][0] != "2" ||
			counts[HeaderOriginalTimestamp][0] != "2026-11-25T10:00:00Z" ||
			counts[HeaderOriginalError][0] != "handler_error" {
			t.Fatalf("unexpected DLQ headers: %v", counts)
		}
	})

	t.Run("R03 DLQ captures immutable snapshot before handler/interceptor mutations and preserves nil vs empty", func(t *testing.T) {
		for _, tc := range []struct {
			name      string
			key       []byte
			value     []byte
			hdrVal    []byte
			wantNilKV bool
		}{
			{name: "non-empty slices", key: []byte("orig-key"), value: []byte("orig-val"), hdrVal: []byte("orig-hdr")},
			{name: "nil slices preserved", key: nil, value: nil, hdrVal: nil, wantNilKV: true},
			{name: "empty non-nil slices preserved", key: []byte{}, value: []byte{}, hdrVal: []byte{}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				var sleeps []time.Duration
				rt := newNoopRetryRuntime(&sleeps)
				dlq := &mockDLQProducer{}
				policy, err := normalizeAndValidateRetryPolicy("orders", &RetryPolicy{
					Strategy:         RetryStrategyDLQ,
					MaxRetries:       0,
					DLQTopic:         "orders-dlq",
					DLQProducer:      dlq,
					DLQFailurePolicy: DLQFailureStop,
				})
				if err != nil {
					t.Fatal(err)
				}

				ts := time.Date(2026, 11, 26, 8, 30, 0, 0, time.UTC)
				wantKey := cloneBytesPreserveNil(tc.key)
				wantVal := cloneBytesPreserveNil(tc.value)
				msg := &Message{
					Topic:     "orders",
					Partition: 4,
					Offset:    123,
					Timestamp: ts,
					Key:       tc.key,
					Value:     tc.value,
					Headers:   []RecordHeader{{Key: "x-app", Value: tc.hdrVal}},
				}

				res := rt.processMessage(context.Background(), msg, func(ctx context.Context, m *Message) error {
					// Handler mutates every field of m in place before failing.
					if len(m.Key) > 0 {
						m.Key[0] = 'X'
					}
					if len(m.Value) > 0 {
						m.Value[0] = 'Y'
					}
					if len(m.Headers) > 0 && len(m.Headers[0].Value) > 0 {
						m.Headers[0].Value[0] = 'Z'
					}
					m.Key = []byte("tampered-key")
					m.Value = []byte("tampered-val")
					m.Topic = "wrong-topic"
					m.Partition = 99
					m.Offset = 999
					m.Timestamp = time.Time{}
					m.Headers = []RecordHeader{{Key: "injected", Value: []byte("bad")}}
					return errors.New("boom")
				}, policy, time.Second)

				if res.Outcome != outcomeDeadLettered || len(dlq.calls) != 1 {
					t.Fatalf("expected outcomeDeadLettered, got %+v calls=%d", res, len(dlq.calls))
				}
				sent := dlq.calls[0]
				if tc.wantNilKV {
					if sent.Key != nil || sent.Value != nil {
						t.Fatalf("expected nil key/value preserved, got key=%v value=%v", sent.Key, sent.Value)
					}
				} else {
					if sent.Key == nil || sent.Value == nil {
						t.Fatalf("expected non-nil key/value preserved")
					}
					if !bytes.Equal(sent.Key, wantKey) || !bytes.Equal(sent.Value, wantVal) {
						t.Fatalf("mutated key/value leaked to DLQ: got key=%q val=%q, want key=%q val=%q", sent.Key, sent.Value, wantKey, wantVal)
					}
				}

				hdrs := map[string]RecordHeader{}
				for _, h := range sent.Headers {
					hdrs[h.Key] = h
				}
				if _, injected := hdrs["injected"]; injected {
					t.Fatalf("handler-injected header leaked into DLQ")
				}
				if string(hdrs[HeaderOriginalTopic].Value) != "orders" ||
					string(hdrs[HeaderOriginalPartition].Value) != "4" ||
					string(hdrs[HeaderOriginalOffset].Value) != "123" ||
					string(hdrs[HeaderOriginalTimestamp].Value) != ts.Format(time.RFC3339Nano) {
					t.Fatalf("mutated metadata leaked into DLQ headers: %+v", hdrs)
				}
			})
		}
	})

	t.Run("R05 safe diagnostics redact secrets from logs and DLQ while preserving errors.Is", func(t *testing.T) {
		var logBuf bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		sentinel := errors.New("db_auth_token=SUPER_SECRET_TOKEN_XYZ")
		dlq := &mockDLQProducer{}

		rt := &retryRuntime{
			groupID:  "grp-safe",
			clientID: "cli-safe",
			logger:   logger,
			sleepFn:  func(ctx context.Context, d time.Duration) error { return ctx.Err() },
			jitterFn: func(d time.Duration) time.Duration { return d },
		}

		policy, err := normalizeAndValidateRetryPolicy("orders", &RetryPolicy{
			Strategy:         RetryStrategyDLQ,
			MaxRetries:       1,
			InitialDelay:     time.Millisecond,
			MaxDelay:         time.Millisecond,
			Multiplier:       1,
			DLQTopic:         "orders-dlq",
			DLQProducer:      dlq,
			DLQFailurePolicy: DLQFailureStop,
		})
		if err != nil {
			t.Fatal(err)
		}

		var attempt int
		res := rt.processMessage(context.Background(), baseMsg(), func(ctx context.Context, m *Message) error {
			attempt++
			if attempt == 1 {
				panic(fmt.Errorf("panic wrapping secret: %w", sentinel))
			}
			return fmt.Errorf("failed query: %w", sentinel)
		}, policy, time.Second)

		if res.Outcome != outcomeDeadLettered {
			t.Fatalf("expected outcomeDeadLettered, got %+v", res)
		}
		if !errors.Is(res.Err, sentinel) {
			t.Fatalf("expected errors.Is(res.Err, sentinel) to remain true, got %v", res.Err)
		}
		if strings.Contains(logBuf.String(), "SUPER_SECRET_TOKEN_XYZ") {
			t.Fatalf("secret leaked into library logs:\n%s", logBuf.String())
		}
		var xOrigErr string
		for _, h := range dlq.calls[0].Headers {
			if h.Key == HeaderOriginalError {
				xOrigErr = string(h.Value)
			}
		}
		if strings.Contains(xOrigErr, "SUPER_SECRET_TOKEN_XYZ") || xOrigErr != "handler_error" {
			t.Fatalf("expected safe category in x-original-error, got %q", xOrigErr)
		}
	})

	t.Run("UT04_UT05 DLQ failure with discard vs stop", func(t *testing.T) {
		transient := errors.New("broker down")
		permanent := Permanent(errors.New("invalid DLQ record"))
		for _, tc := range []struct {
			fallback   DLQFailurePolicy
			publishErr error
			wantOut    disposition
			wantReason string
		}{
			{DLQFailureDiscard, transient, outcomeDiscarded, "dlq_failed"},
			{DLQFailureDiscard, permanent, outcomeDiscarded, "dlq_failed"},
			{DLQFailureStop, transient, outcomeTerminalFailure, ""},
			{DLQFailureStop, permanent, outcomeTerminalFailure, ""},
			{DLQFailureRetry, permanent, outcomeTerminalFailure, ""},
		} {
			rt := newNoopRetryRuntime(nil)
			var attempts []bool
			rt.onDLQAttempt = func(_ string, success bool) { attempts = append(attempts, success) }
			dlq := &mockDLQProducer{publishF: func(context.Context, string, []byte, []byte, []RecordHeader) error {
				// Bound this regression if the implementation retries a permanent error.
				if len(attempts) == 1 {
					return nil
				}
				return tc.publishErr
			}}
			policy, err := normalizeAndValidateRetryPolicy("orders", &RetryPolicy{
				Strategy:         RetryStrategyDLQ,
				MaxRetries:       0,
				DLQTopic:         "orders-dlq",
				DLQProducer:      dlq,
				DLQFailurePolicy: tc.fallback,
			})
			if err != nil {
				t.Fatal(err)
			}
			res := rt.processMessage(context.Background(), baseMsg(), func(context.Context, *Message) error {
				return errors.New("boom")
			}, policy, time.Second)
			if res.Outcome != tc.wantOut || res.DiscardReason != tc.wantReason {
				t.Fatalf("fallback=%s: got %+v, want outcome=%v reason=%s", tc.fallback, res, tc.wantOut, tc.wantReason)
			}
			if len(attempts) != 1 || attempts[0] || !errors.Is(res.Err, tc.publishErr) {
				t.Fatalf("fallback=%s: want one failed DLQ metric and original error, got %v and %v", tc.fallback, attempts, res.Err)
			}
		}
	})

	t.Run("UT10_UT11 DLQ retry retries DLQ only without rerunning handler and stops on cancel", func(t *testing.T) {
		var handlerCalls atomic.Int32
		var dlqAttempts atomic.Int32
		var sleeps []time.Duration
		rt := newNoopRetryRuntime(&sleeps)

		dlq := &mockDLQProducer{
			publishF: func(context.Context, string, []byte, []byte, []RecordHeader) error {
				if dlqAttempts.Add(1) < 3 {
					return errors.New("dlq unavailable")
				}
				return nil
			},
		}
		policy, err := normalizeAndValidateRetryPolicy("orders", &RetryPolicy{
			Strategy:         RetryStrategyDLQ,
			MaxRetries:       1,
			InitialDelay:     10 * time.Millisecond,
			MaxDelay:         time.Second,
			Multiplier:       2,
			DLQTopic:         "orders-dlq",
			DLQProducer:      dlq,
			DLQFailurePolicy: DLQFailureRetry,
		})
		if err != nil {
			t.Fatal(err)
		}

		res := rt.processMessage(context.Background(), baseMsg(), func(context.Context, *Message) error {
			handlerCalls.Add(1)
			return errors.New("handler fail")
		}, policy, time.Second)

		if res.Outcome != outcomeDeadLettered || handlerCalls.Load() != 2 || dlqAttempts.Load() != 3 {
			t.Fatalf("got res=%+v handlerCalls=%d dlqAttempts=%d", res, handlerCalls.Load(), dlqAttempts.Load())
		}

		// Cancellation during DLQ retry yields outcomeInterrupted.
		ctx, cancel := context.WithCancel(context.Background())
		var cancelledAttempts int
		dlqFail := &mockDLQProducer{
			publishF: func(context.Context, string, []byte, []byte, []RecordHeader) error {
				cancelledAttempts++
				if cancelledAttempts == 2 {
					cancel()
				}
				return errors.New("dlq still failing")
			},
		}
		policy.DLQProducer = dlqFail
		res2 := rt.processMessage(ctx, baseMsg(), func(context.Context, *Message) error {
			return errors.New("handler fail")
		}, policy, time.Second)
		if res2.Outcome != outcomeInterrupted {
			t.Fatalf("expected outcomeInterrupted on cancelled DLQ retry, got %+v", res2)
		}

		ctx, cancel = context.WithCancel(context.Background())
		defer cancel()
		policy.DLQProducer = &mockDLQProducer{publishF: func(context.Context, string, []byte, []byte, []RecordHeader) error {
			cancel()
			return Permanent(errors.New("invalid DLQ record"))
		}}
		res3 := rt.processMessage(ctx, baseMsg(), func(context.Context, *Message) error {
			return errors.New("handler fail")
		}, policy, time.Second)
		if res3.Outcome != outcomeInterrupted || !errors.Is(res3.Err, context.Canceled) {
			t.Fatalf("session cancellation should take precedence over a permanent DLQ result, got %+v", res3)
		}
	})

	t.Run("UT09 infinite retry retries beyond limit until success or cancel", func(t *testing.T) {
		var sleeps []time.Duration
		rt := newNoopRetryRuntime(&sleeps)
		policy, err := normalizeAndValidateRetryPolicy("orders", &RetryPolicy{
			Strategy:     RetryStrategyInfinite,
			MaxRetries:   1, // ignored by infinite
			InitialDelay: time.Millisecond,
			MaxDelay:     10 * time.Millisecond,
			Multiplier:   2,
		})
		if err != nil {
			t.Fatal(err)
		}

		var calls int
		res := rt.processMessage(context.Background(), baseMsg(), func(context.Context, *Message) error {
			calls++
			if calls < 7 {
				return errors.New("not ready")
			}
			return nil
		}, policy, time.Second)
		if res.Outcome != outcomeProcessed || calls != 7 {
			t.Fatalf("expected 7 calls and outcomeProcessed, got calls=%d res=%+v", calls, res)
		}

		ctx, cancel := context.WithCancel(context.Background())
		calls = 0
		resCancel := rt.processMessage(ctx, baseMsg(), func(context.Context, *Message) error {
			calls++
			if calls == 3 {
				cancel()
			}
			return errors.New("still failing")
		}, policy, time.Second)
		if resCancel.Outcome != outcomeInterrupted {
			t.Fatalf("expected outcomeInterrupted, got %+v", resCancel)
		}
	})

	t.Run("UT07 handler attempt timeout triggers retry while session active; session cancel interrupts", func(t *testing.T) {
		var sleeps []time.Duration
		rt := newNoopRetryRuntime(&sleeps)
		policy, err := normalizeAndValidateRetryPolicy("orders", &RetryPolicy{
			Strategy:     RetryStrategyLogAndIgnore,
			MaxRetries:   1,
			InitialDelay: time.Millisecond,
			MaxDelay:     time.Second,
			Multiplier:   2,
		})
		if err != nil {
			t.Fatal(err)
		}

		var attempt int
		res := rt.processMessage(context.Background(), baseMsg(), func(ctx context.Context, msg *Message) error {
			attempt++
			if attempt == 1 {
				// Simulate handler hitting per-attempt deadline and returning nil anyway (conservative failure).
				<-ctx.Done()
				return nil
			}
			return nil
		}, policy, 15*time.Millisecond)

		if res.Outcome != outcomeProcessed || attempt != 2 {
			t.Fatalf("expected retry after attempt timeout and success on attempt 2, got attempt=%d res=%+v", attempt, res)
		}

		// Session cancellation must interrupt immediately without discarding.
		sessionCtx, sessionCancel := context.WithCancel(context.Background())
		res2 := rt.processMessage(sessionCtx, baseMsg(), func(ctx context.Context, msg *Message) error {
			sessionCancel()
			return errors.New("handler saw cancel")
		}, policy, time.Second)
		if res2.Outcome != outcomeInterrupted {
			t.Fatalf("expected outcomeInterrupted on session cancel, got %+v", res2)
		}
	})

	t.Run("UT27 panic containment feeds retry policy", func(t *testing.T) {
		rt := newNoopRetryRuntime(nil)
		policy, err := normalizeAndValidateRetryPolicy("orders", &RetryPolicy{
			Strategy:   RetryStrategyLogAndIgnore,
			MaxRetries: 1,
		})
		if err != nil {
			t.Fatal(err)
		}

		var calls int
		res := rt.processMessage(context.Background(), baseMsg(), func(context.Context, *Message) error {
			calls++
			if calls == 1 {
				panic("unexpected nil pointer")
			}
			return nil
		}, policy, time.Second)
		if res.Outcome != outcomeProcessed || calls != 2 {
			t.Fatalf("expected panic to be recovered and retried to success, got calls=%d res=%+v", calls, res)
		}
	})

	t.Run("F07 bounded RetryStrategyStop exhausts attempts and returns terminal failure", func(t *testing.T) {
		var sleeps []time.Duration
		rt := newNoopRetryRuntime(&sleeps)
		policy, err := normalizeAndValidateRetryPolicy("orders", &RetryPolicy{
			Strategy:     RetryStrategyStop,
			MaxRetries:   2,
			InitialDelay: 5 * time.Millisecond,
			MaxDelay:     time.Second,
			Multiplier:   2,
		})
		if err != nil {
			t.Fatal(err)
		}

		sentinel := errors.New("db unavailable")
		var calls int
		res := rt.processMessage(context.Background(), baseMsg(), func(context.Context, *Message) error {
			calls++
			return sentinel
		}, policy, time.Second)

		if res.Outcome != outcomeTerminalFailure || calls != 3 || len(sleeps) != 2 || !errors.Is(res.Err, sentinel) {
			t.Fatalf("unexpected stop result: %+v calls=%d sleeps=%v", res, calls, sleeps)
		}
	})

	t.Run("F08 Permanent error skips retries and applies strategy disposition", func(t *testing.T) {
		if Permanent(nil) != nil {
			t.Fatalf("Permanent(nil) must return nil")
		}
		rootErr := errors.New("malformed json")
		permErr := Permanent(rootErr)
		if !errors.Is(permErr, rootErr) {
			t.Fatalf("Permanent must preserve wrapped error for errors.Is")
		}

		// Ignore -> immediate discard
		{
			var sleeps []time.Duration
			rt := newNoopRetryRuntime(&sleeps)
			policy, _ := normalizeAndValidateRetryPolicy("orders", &RetryPolicy{
				Strategy:   RetryStrategyLogAndIgnore,
				MaxRetries: 5,
			})
			var calls int
			res := rt.processMessage(context.Background(), baseMsg(), func(context.Context, *Message) error {
				calls++
				return permErr
			}, policy, time.Second)
			if res.Outcome != outcomeDiscarded || res.DiscardReason != "permanent_error" || calls != 1 || len(sleeps) != 0 {
				t.Fatalf("ignore+Permanent: got %+v calls=%d sleeps=%v", res, calls, sleeps)
			}
		}

		// DLQ -> immediate DLQ publish
		{
			var sleeps []time.Duration
			rt := newNoopRetryRuntime(&sleeps)
			dlq := &mockDLQProducer{}
			policy, _ := normalizeAndValidateRetryPolicy("orders", &RetryPolicy{
				Strategy:         RetryStrategyDLQ,
				MaxRetries:       5,
				DLQTopic:         "orders-dlq",
				DLQProducer:      dlq,
				DLQFailurePolicy: DLQFailureStop,
			})
			var calls int
			res := rt.processMessage(context.Background(), baseMsg(), func(context.Context, *Message) error {
				calls++
				return permErr
			}, policy, time.Second)
			if res.Outcome != outcomeDeadLettered || calls != 1 || len(sleeps) != 0 || len(dlq.calls) != 1 {
				t.Fatalf("dlq+Permanent: got %+v calls=%d sleeps=%v dlq=%d", res, calls, sleeps, len(dlq.calls))
			}
		}

		// Stop -> immediate terminal failure
		{
			var sleeps []time.Duration
			rt := newNoopRetryRuntime(&sleeps)
			policy, _ := normalizeAndValidateRetryPolicy("orders", &RetryPolicy{
				Strategy:   RetryStrategyStop,
				MaxRetries: 5,
			})
			var calls int
			res := rt.processMessage(context.Background(), baseMsg(), func(context.Context, *Message) error {
				calls++
				return permErr
			}, policy, time.Second)
			if res.Outcome != outcomeTerminalFailure || calls != 1 || len(sleeps) != 0 || !errors.Is(res.Err, rootErr) {
				t.Fatalf("stop+Permanent: got %+v calls=%d sleeps=%v", res, calls, sleeps)
			}
		}

		// Infinite -> stop without acknowledging rather than looping forever
		{
			var sleeps []time.Duration
			rt := newNoopRetryRuntime(&sleeps)
			policy, _ := normalizeAndValidateRetryPolicy("orders", &RetryPolicy{
				Strategy: RetryStrategyInfinite,
			})
			var calls int
			res := rt.processMessage(context.Background(), baseMsg(), func(context.Context, *Message) error {
				calls++
				return permErr
			}, policy, time.Second)
			if res.Outcome != outcomeTerminalFailure || calls != 1 || len(sleeps) != 0 || !errors.Is(res.Err, rootErr) {
				t.Fatalf("infinite+Permanent: got %+v calls=%d sleeps=%v", res, calls, sleeps)
			}
		}
	})
}

func TestBackoffBoundsAndOverflowGuard(t *testing.T) {
	// Huge multiplier and huge current delay must never overflow into negative duration.
	for _, tc := range []struct {
		current time.Duration
		max     time.Duration
		mult    float64
		want    time.Duration
	}{
		{100 * time.Millisecond, 30 * time.Second, 2.0, 200 * time.Millisecond},
		{20 * time.Second, 30 * time.Second, 2.0, 30 * time.Second},
		{time.Duration(math.MaxInt64 / 2), time.Duration(math.MaxInt64), 100.0, time.Duration(math.MaxInt64)},
		{time.Second, 5 * time.Second, 1e300, 5 * time.Second},
	} {
		got := nextBackoffDelay(tc.current, tc.max, tc.mult)
		if got != tc.want || got < 0 {
			t.Fatalf("nextBackoffDelay(%v, %v, %v) = %v, want %v", tc.current, tc.max, tc.mult, got, tc.want)
		}
	}

	// Default jitter stays in [delay/2, delay] for positive delays.
	d := 100 * time.Millisecond
	for i := 0; i < 100; i++ {
		j := defaultJitter(d)
		if j < d/2 || j > d {
			t.Fatalf("jitter %v out of [%v, %v]", j, d/2, d)
		}
	}

	// Cancelable sleep returns immediately when context is cancelled.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := defaultSleep(ctx, 10*time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Fatalf("defaultSleep did not abort promptly on cancel")
	}
	// Verify strconv isn't unused if needed
	_ = strconv.Itoa(1)
}
