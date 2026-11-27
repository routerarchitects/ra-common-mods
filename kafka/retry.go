package kafka

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"reflect"
	"time"
)

type permanentError struct {
	err error
}

func (e *permanentError) Error() string {
	return e.err.Error()
}

func (e *permanentError) Unwrap() error {
	return e.err
}

// Permanent marks err as non-retryable so the consumer skips further handler retries
// and immediately applies the subscription's failure disposition. A permanent
// publication error under DLQFailureRetry stops the consumer.
// Calling Permanent(nil) returns nil.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &permanentError{err: err}
}

func isPermanentError(err error) bool {
	if err == nil {
		return false
	}
	var pe *permanentError
	return errors.As(err, &pe)
}

const (
	defaultReconnectInitialDelay = 250 * time.Millisecond
	defaultReconnectMaxDelay     = 5 * time.Second
)

type disposition uint8

const (
	outcomeProcessed disposition = iota + 1
	outcomeDeadLettered
	outcomeDiscarded
	outcomeInterrupted
	outcomeTerminalFailure
)

func (d disposition) String() string {
	switch d {
	case outcomeProcessed:
		return "processed"
	case outcomeDeadLettered:
		return "dead_lettered"
	case outcomeDiscarded:
		return "discarded"
	case outcomeInterrupted:
		return "interrupted"
	case outcomeTerminalFailure:
		return "terminal_failure"
	default:
		return "invalid"
	}
}

type processingResult struct {
	Outcome       disposition
	Err           error
	DiscardReason string
	Attempts      int
}

func isNilInterface(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Ptr, reflect.Interface, reflect.Slice, reflect.Map, reflect.Func, reflect.Chan:
		return rv.IsNil()
	default:
		return false
	}
}

func normalizeAndValidateRetryPolicy(sourceTopic string, policy *RetryPolicy) (RetryPolicy, error) {
	if policy == nil {
		return RetryPolicy{}, fmt.Errorf("retry policy must be explicitly configured (use DefaultRetryPolicy() for bounded retries with discard)")
	}

	p := *policy
	if p.Strategy == "" {
		return RetryPolicy{}, fmt.Errorf("retry policy strategy must be specified")
	}
	if p.MaxRetries < 0 {
		return RetryPolicy{}, fmt.Errorf("retry policy max_retries cannot be negative")
	}

	if p.InitialDelay == 0 {
		p.InitialDelay = 100 * time.Millisecond
	}
	if p.MaxDelay == 0 {
		p.MaxDelay = 30 * time.Second
	}
	if p.Multiplier == 0 {
		p.Multiplier = 2.0
	}

	if p.InitialDelay <= 0 {
		return RetryPolicy{}, fmt.Errorf("retry policy initial_delay must be greater than zero")
	}
	if p.MaxDelay <= 0 {
		return RetryPolicy{}, fmt.Errorf("retry policy max_delay must be greater than zero")
	}
	if p.MaxDelay < p.InitialDelay {
		return RetryPolicy{}, fmt.Errorf("retry policy max_delay (%s) cannot be less than initial_delay (%s)", p.MaxDelay, p.InitialDelay)
	}
	if math.IsNaN(p.Multiplier) || math.IsInf(p.Multiplier, 0) || p.Multiplier < 1.0 {
		return RetryPolicy{}, fmt.Errorf("retry policy multiplier must be a finite number >= 1.0")
	}

	if p.DLQFailurePolicy != "" {
		switch p.DLQFailurePolicy {
		case DLQFailureDiscard, DLQFailureRetry, DLQFailureStop:
		default:
			return RetryPolicy{}, fmt.Errorf("unsupported DLQ failure policy %q", p.DLQFailurePolicy)
		}
	}

	switch p.Strategy {
	case RetryStrategyLogAndIgnore, RetryStrategyInfinite, RetryStrategyStop:
		return p, nil

	case RetryStrategyDLQ:
		if err := validateTopicName(p.DLQTopic); err != nil {
			return RetryPolicy{}, fmt.Errorf("invalid DLQ topic: %w", err)
		}
		if p.DLQTopic == sourceTopic {
			return RetryPolicy{}, fmt.Errorf("DLQ topic %q cannot be identical to source topic", p.DLQTopic)
		}
		if isNilInterface(p.DLQProducer) {
			return RetryPolicy{}, fmt.Errorf("DLQ producer must be specified for DLQ retry strategy")
		}
		if kp, ok := p.DLQProducer.(*producer); ok && kp.requiredAcks == 0 {
			return RetryPolicy{}, fmt.Errorf("DLQ producer must acknowledge writes (required_acks=0 is not allowed for DLQ)")
		}
		if p.DLQFailurePolicy == "" {
			return RetryPolicy{}, fmt.Errorf("DLQ failure policy (discard, retry, or stop) must be explicitly configured for DLQ retry strategy")
		}
		return p, nil

	default:
		return RetryPolicy{}, fmt.Errorf("invalid retry strategy %q", p.Strategy)
	}
}

func defaultSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// defaultJitter returns a uniform random duration in [delay/2, delay].
func defaultJitter(delay time.Duration) time.Duration {
	if delay <= 1 {
		return delay
	}
	half := delay / 2
	span := delay - half
	return half + time.Duration(rand.Int64N(int64(span)+1))
}

func nextBackoffDelay(current, maxDelay time.Duration, multiplier float64) time.Duration {
	if current <= 0 {
		return 0
	}
	if current >= maxDelay {
		return maxDelay
	}
	if multiplier <= 1.0 {
		return current
	}
	scaled := float64(current) * multiplier
	if math.IsNaN(scaled) || math.IsInf(scaled, 0) || scaled >= float64(maxDelay) || scaled >= float64(math.MaxInt64) {
		return maxDelay
	}
	next := time.Duration(scaled)
	if next <= 0 || next > maxDelay {
		return maxDelay
	}
	return next
}

type retryRuntime struct {
	groupID        string
	clientID       string
	logger         *slog.Logger
	sleepFn        func(ctx context.Context, d time.Duration) error
	jitterFn       func(delay time.Duration) time.Duration
	onAttemptStart func(msg *Message)
	onAttemptFail  func(topic string)
	onRetry        func(topic string)
	onAttemptDone  func(topic string, duration time.Duration, outcome string)
	onDLQAttempt   func(sourceTopic string, success bool)
	startAttempt   func(ctx context.Context, msg *Message, attempt int) (context.Context, func(error))
}

func (rt *retryRuntime) sleep(ctx context.Context, d time.Duration) error {
	if rt.sleepFn != nil {
		return rt.sleepFn(ctx, d)
	}
	return defaultSleep(ctx, d)
}

func (rt *retryRuntime) jitter(delay time.Duration) time.Duration {
	if rt.jitterFn != nil {
		j := rt.jitterFn(delay)
		if j < 0 {
			return 0
		}
		if j > delay {
			return delay
		}
		return j
	}
	return defaultJitter(delay)
}

func invokeHandlerSafe(ctx context.Context, msg *Message, handler Handler, logger *slog.Logger, groupID, clientID string, attempt int) (err error) {
	defer func() {
		if r := recover(); r != nil {
			pe := newHandlerPanicError(r).(*handlerPanicError)
			if logger != nil {
				logger.Error("message handler panicked",
					"group_id", groupID,
					"client_id", clientID,
					"topic", msg.Topic,
					"partition", msg.Partition,
					"offset", msg.Offset,
					"attempt", attempt,
					"failure_category", "handler_panic",
					"panic_type", pe.typeName,
					"panic_frames", pe.frames,
				)
			}
			err = pe
		}
	}()
	return handler(ctx, msg)
}

func (rt *retryRuntime) processMessage(
	sessionCtx context.Context,
	msg *Message,
	handler Handler,
	policy RetryPolicy,
	handlerTimeout time.Duration,
) processingResult {
	if err := sessionCtx.Err(); err != nil {
		return processingResult{Outcome: outcomeInterrupted, Err: err}
	}

	// Capture an immutable snapshot before any handler/interceptor runs only for DLQ-enabled subscriptions.
	var dlqSnapshot *Message
	if policy.Strategy == RetryStrategyDLQ {
		dlqSnapshot = cloneMessageSnapshot(msg)
	}

	delay := policy.InitialDelay
	retriesUsed := 0
	attempts := 0
	var lastErr error
	permanent := false

	for {
		if err := sessionCtx.Err(); err != nil {
			return processingResult{Outcome: outcomeInterrupted, Err: err, Attempts: attempts}
		}

		if attempts > 0 {
			wait := rt.jitter(delay)
			if rt.logger != nil {
				rt.logger.Warn("retrying message processing",
					"group_id", rt.groupID,
					"client_id", rt.clientID,
					"topic", msg.Topic,
					"partition", msg.Partition,
					"offset", msg.Offset,
					"attempt", attempts+1,
					"retry_delay", wait,
					"failure_category", classifyFailureCategory(lastErr),
				)
			}
			if rt.onRetry != nil {
				rt.onRetry(msg.Topic)
			}
			if err := rt.sleep(sessionCtx, wait); err != nil {
				return processingResult{Outcome: outcomeInterrupted, Err: err, Attempts: attempts}
			}
			delay = nextBackoffDelay(delay, policy.MaxDelay, policy.Multiplier)
		}

		if err := sessionCtx.Err(); err != nil {
			return processingResult{Outcome: outcomeInterrupted, Err: err, Attempts: attempts}
		}

		attempts++
		if rt.onAttemptStart != nil {
			rt.onAttemptStart(msg)
		}
		attemptCtx, attemptCancel := context.WithTimeout(sessionCtx, handlerTimeout)
		var endTrace func(error)
		if rt.startAttempt != nil {
			attemptCtx, endTrace = rt.startAttempt(attemptCtx, msg, attempts)
		}

		start := time.Now()
		err := invokeHandlerSafe(attemptCtx, msg, handler, rt.logger, rt.groupID, rt.clientID, attempts)
		attemptErr := attemptCtx.Err()
		attemptCancel()
		duration := time.Since(start)

		// Session cancellation always supersedes attempt outcome.
		if sessionCtx.Err() != nil {
			cancelErr := sessionCtx.Err()
			if endTrace != nil {
				endTrace(cancelErr)
			}
			if rt.onAttemptDone != nil {
				rt.onAttemptDone(msg.Topic, duration, "interrupted")
			}
			return processingResult{Outcome: outcomeInterrupted, Err: cancelErr, Attempts: attempts}
		}

		// If the attempt deadline expired even though the handler returned nil, treat conservatively as failure.
		if err == nil && attemptErr != nil {
			err = fmt.Errorf("handler attempt timed out: %w", attemptErr)
		}

		if endTrace != nil {
			endTrace(err)
		}

		if err == nil {
			if rt.onAttemptDone != nil {
				rt.onAttemptDone(msg.Topic, duration, "success")
			}
			if rt.logger != nil {
				rt.logger.Debug("message processed successfully",
					"group_id", rt.groupID,
					"client_id", rt.clientID,
					"topic", msg.Topic,
					"partition", msg.Partition,
					"offset", msg.Offset,
					"attempt", attempts,
					"duration", duration,
					"outcome", outcomeProcessed.String(),
				)
			}
			return processingResult{Outcome: outcomeProcessed, Attempts: attempts}
		}

		lastErr = err
		if rt.onAttemptFail != nil {
			rt.onAttemptFail(msg.Topic)
		}
		if rt.onAttemptDone != nil {
			rt.onAttemptDone(msg.Topic, duration, "failure")
		}

		if isPermanentError(err) {
			permanent = true
			break
		}

		if policy.Strategy != RetryStrategyInfinite && retriesUsed >= policy.MaxRetries {
			break
		}
		retriesUsed++
	}

	// Handler retries exhausted (or permanent error); execute post-exhaustion strategy.
	if err := sessionCtx.Err(); err != nil {
		return processingResult{Outcome: outcomeInterrupted, Err: err, Attempts: attempts}
	}

	switch policy.Strategy {
	case RetryStrategyLogAndIgnore:
		discardReason := "handler_exhausted"
		logMsg := "message processing failed after retries; discarding message"
		if permanent {
			discardReason = "permanent_error"
			logMsg = "message processing failed with permanent error; discarding message"
		}
		if rt.logger != nil {
			rt.logger.Error(logMsg,
				"group_id", rt.groupID,
				"client_id", rt.clientID,
				"topic", msg.Topic,
				"partition", msg.Partition,
				"offset", msg.Offset,
				"attempt", attempts,
				"outcome", outcomeDiscarded.String(),
				"discard_reason", discardReason,
				"failure_category", classifyFailureCategory(lastErr),
			)
		}
		return processingResult{
			Outcome:       outcomeDiscarded,
			DiscardReason: discardReason,
			Err:           lastErr,
			Attempts:      attempts,
		}

	case RetryStrategyDLQ:
		return rt.handleDLQ(sessionCtx, dlqSnapshot, policy, lastErr, attempts)

	case RetryStrategyStop:
		if rt.logger != nil {
			rt.logger.Error("message processing failed; stopping consumer per policy",
				"group_id", rt.groupID,
				"client_id", rt.clientID,
				"topic", msg.Topic,
				"partition", msg.Partition,
				"offset", msg.Offset,
				"attempt", attempts,
				"outcome", outcomeTerminalFailure.String(),
				"failure_category", classifyFailureCategory(lastErr),
			)
		}
		return processingResult{
			Outcome:  outcomeTerminalFailure,
			Err:      fmt.Errorf("message processing stopped after %d attempts on %s/%d@%d: %w", attempts, msg.Topic, msg.Partition, msg.Offset, lastErr),
			Attempts: attempts,
		}

	case RetryStrategyInfinite:
		// Reached only on permanent error so infinite retry does not wedge a partition forever.
		if rt.logger != nil {
			rt.logger.Error("permanent error under infinite retry policy; stopping consumer without acknowledging",
				"group_id", rt.groupID,
				"client_id", rt.clientID,
				"topic", msg.Topic,
				"partition", msg.Partition,
				"offset", msg.Offset,
				"attempt", attempts,
				"outcome", outcomeTerminalFailure.String(),
				"failure_category", classifyFailureCategory(lastErr),
			)
		}
		return processingResult{
			Outcome:  outcomeTerminalFailure,
			Err:      fmt.Errorf("permanent failure under infinite retry policy on %s/%d@%d: %w", msg.Topic, msg.Partition, msg.Offset, lastErr),
			Attempts: attempts,
		}

	default:
		return processingResult{
			Outcome:  outcomeTerminalFailure,
			Err:      fmt.Errorf("unexpected retry strategy %q", policy.Strategy),
			Attempts: attempts,
		}
	}
}

func (rt *retryRuntime) handleDLQ(
	sessionCtx context.Context,
	msg *Message,
	policy RetryPolicy,
	handlerErr error,
	attempts int,
) processingResult {
	handlerCategory := classifyFailureCategory(handlerErr)
	dlqHeaders := buildDLQHeaders(msg.Headers, dlqMetadata{
		Topic:         msg.Topic,
		Partition:     msg.Partition,
		Offset:        msg.Offset,
		Timestamp:     msg.Timestamp,
		ConsumerGroup: rt.groupID,
		Error:         handlerCategory,
		Attempts:      attempts,
	})

	dlqDelay := policy.InitialDelay
	dlqAttempt := 0

	for {
		if err := sessionCtx.Err(); err != nil {
			return processingResult{Outcome: outcomeInterrupted, Err: err, Attempts: attempts}
		}

		if dlqAttempt > 0 {
			wait := rt.jitter(dlqDelay)
			if rt.logger != nil {
				rt.logger.Warn("retrying DLQ publication",
					"group_id", rt.groupID,
					"client_id", rt.clientID,
					"topic", msg.Topic,
					"dlq_topic", policy.DLQTopic,
					"partition", msg.Partition,
					"offset", msg.Offset,
					"attempt", attempts,
					"dlq_attempt", dlqAttempt+1,
					"retry_delay", wait,
					"failure_category", handlerCategory,
				)
			}
			if err := rt.sleep(sessionCtx, wait); err != nil {
				return processingResult{Outcome: outcomeInterrupted, Err: err, Attempts: attempts}
			}
			dlqDelay = nextBackoffDelay(dlqDelay, policy.MaxDelay, policy.Multiplier)
		}

		if err := sessionCtx.Err(); err != nil {
			return processingResult{Outcome: outcomeInterrupted, Err: err, Attempts: attempts}
		}

		dlqAttempt++
		// Publication is synchronous and may outlive the session's cancellation.
		// Check the session again before applying any DLQ disposition.
		dlqErr := policy.DLQProducer.PublishWithHeaders(
			sessionCtx,
			policy.DLQTopic,
			msg.Key,
			msg.Value,
			cloneHeaders(dlqHeaders),
		)

		if err := sessionCtx.Err(); err != nil {
			return processingResult{Outcome: outcomeInterrupted, Err: err, Attempts: attempts}
		}

		if dlqErr == nil {
			if rt.onDLQAttempt != nil {
				rt.onDLQAttempt(msg.Topic, true)
			}
			if rt.logger != nil {
				rt.logger.Warn("message sent to DLQ after retry exhaustion",
					"group_id", rt.groupID,
					"client_id", rt.clientID,
					"topic", msg.Topic,
					"dlq_topic", policy.DLQTopic,
					"partition", msg.Partition,
					"offset", msg.Offset,
					"attempt", attempts,
					"outcome", outcomeDeadLettered.String(),
					"failure_category", handlerCategory,
				)
			}
			return processingResult{
				Outcome:  outcomeDeadLettered,
				Err:      handlerErr,
				Attempts: attempts,
			}
		}

		if rt.onDLQAttempt != nil {
			rt.onDLQAttempt(msg.Topic, false)
		}

		switch policy.DLQFailurePolicy {
		case DLQFailureDiscard:
			if rt.logger != nil {
				rt.logger.Error("DLQ publication failed; discarding message per policy",
					"group_id", rt.groupID,
					"client_id", rt.clientID,
					"topic", msg.Topic,
					"dlq_topic", policy.DLQTopic,
					"partition", msg.Partition,
					"offset", msg.Offset,
					"attempt", attempts,
					"outcome", outcomeDiscarded.String(),
					"discard_reason", "dlq_failed",
					"failure_category", "dlq_publish_failed",
				)
			}
			return processingResult{
				Outcome:       outcomeDiscarded,
				DiscardReason: "dlq_failed",
				Err:           dlqErr,
				Attempts:      attempts,
			}

		case DLQFailureStop, DLQFailureRetry:
			category := "dlq_publish_failed"
			logMessage := "DLQ publication failed; stopping consumer"
			outcome := outcomeTerminalFailure.String()
			retry := false
			if policy.DLQFailurePolicy == DLQFailureRetry {
				if permanentCategory := permanentDLQFailureCategory(dlqErr); permanentCategory != "" {
					category = permanentCategory
				} else {
					logMessage = "DLQ publication failed; will retry DLQ transfer"
					outcome = "retrying"
					retry = true
				}
			}
			if rt.logger != nil {
				rt.logger.Error(logMessage,
					"group_id", rt.groupID,
					"client_id", rt.clientID,
					"topic", msg.Topic,
					"dlq_topic", policy.DLQTopic,
					"partition", msg.Partition,
					"offset", msg.Offset,
					"attempt", attempts,
					"dlq_attempt", dlqAttempt,
					"outcome", outcome,
					"failure_category", category,
				)
			}
			if retry {
				continue
			}
			return processingResult{
				Outcome: outcomeTerminalFailure,
				Err: &dlqPublishError{
					err:      fmt.Errorf("message retries exhausted (%w) and DLQ publish to %s failed: %w", handlerErr, policy.DLQTopic, dlqErr),
					category: category,
				},
				Attempts: attempts,
			}

		default:
			return processingResult{
				Outcome:  outcomeTerminalFailure,
				Err:      fmt.Errorf("invalid DLQ failure policy %q", policy.DLQFailurePolicy),
				Attempts: attempts,
			}
		}
	}
}
