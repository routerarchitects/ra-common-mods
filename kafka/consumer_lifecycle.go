package kafka

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"runtime"
	"strings"
	"sync"

	"github.com/IBM/sarama"
)

type cancelableDialer struct {
	ctx    context.Context
	dialer *net.Dialer
}

func (d *cancelableDialer) Dial(network, addr string) (net.Conn, error) {
	if err := d.ctx.Err(); err != nil {
		return nil, err
	}
	conn, err := d.dialer.DialContext(d.ctx, network, addr)
	if err != nil {
		return nil, err
	}
	stopAfter := context.AfterFunc(d.ctx, func() {
		_ = conn.Close()
	})
	return &cancelOnCloseConn{Conn: conn, stopAfter: stopAfter}, nil
}

type cancelOnCloseConn struct {
	net.Conn
	stopAfter func() bool
}

func (c *cancelOnCloseConn) Close() error {
	c.stopAfter()
	return c.Conn.Close()
}

// ErrConsumerAlreadyRunning is returned when Subscribe or Run is called on a consumer that has already started.
var ErrConsumerAlreadyRunning = errors.New("kafka: consumer has already started")

// ErrConsumerClosed is returned when operating on a closed consumer.
var ErrConsumerClosed = errors.New("kafka: consumer is closed")

// ErrNoSubscriptions is returned when Run is called before any topic subscriptions are registered.
var ErrNoSubscriptions = errors.New("kafka: no topic subscriptions registered")

var errGracefulShutdown = errors.New("kafka: graceful shutdown")

type admissionGate struct {
	mu       sync.Mutex
	stopping bool
	inFlight int
	drained  chan struct{}
}

func newAdmissionGate() *admissionGate {
	return &admissionGate{}
}

func (g *admissionGate) TryAdmit(sessionCtx context.Context) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopping || sessionCtx.Err() != nil {
		return false
	}
	g.inFlight++
	return true
}

func (g *admissionGate) Release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.inFlight--
	if g.inFlight < 0 {
		g.inFlight = 0
	}
	if g.stopping && g.inFlight == 0 && g.drained != nil {
		close(g.drained)
		g.drained = nil
	}
}

func (g *admissionGate) CloseAndDrainedCh() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stopping = true
	if g.inFlight == 0 {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	if g.drained == nil {
		g.drained = make(chan struct{})
	}
	return g.drained
}

func (g *admissionGate) IsStopping() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stopping
}

func isIdentifiableCommitError(err error) bool {
	if err == nil {
		return false
	}
	var kerr sarama.KError
	if errors.As(err, &kerr) {
		switch kerr {
		case sarama.ErrOffsetMetadataTooLarge,
			sarama.ErrInvalidCommitOffsetSize,
			sarama.ErrUnstableOffsetCommit:
			return true
		}
	}
	var ces sarama.ConsumerErrors
	if errors.As(err, &ces) {
		for _, ce := range ces {
			if ce != nil && isIdentifiableCommitError(ce) {
				return true
			}
		}
	}
	return false
}

func isTerminalSaramaError(err error) bool {
	if err == nil {
		return false
	}

	var cfgErr sarama.ConfigurationError
	if errors.As(err, &cfgErr) {
		return true
	}

	if errors.Is(err, sarama.ErrClosedConsumerGroup) || errors.Is(err, sarama.ErrClosedClient) {
		return true
	}

	var kerr sarama.KError
	if errors.As(err, &kerr) {
		return isTerminalKError(kerr)
	}

	var ces sarama.ConsumerErrors
	if errors.As(err, &ces) {
		for _, ce := range ces {
			if ce != nil && isTerminalSaramaError(ce) {
				return true
			}
		}
	}

	return false
}

func isTerminalKError(kerr sarama.KError) bool {
	switch kerr {
	case sarama.ErrTopicAuthorizationFailed,
		sarama.ErrGroupAuthorizationFailed,
		sarama.ErrClusterAuthorizationFailed,
		sarama.ErrTransactionalIDAuthorizationFailed,
		sarama.ErrDelegationTokenAuthorizationFailed,
		sarama.ErrSASLAuthenticationFailed,
		sarama.ErrUnsupportedSASLMechanism,
		sarama.ErrIllegalSASLState,
		sarama.ErrUnsupportedVersion,
		sarama.ErrUnsupportedForMessageFormat,
		sarama.ErrInconsistentGroupProtocol,
		sarama.ErrInvalidGroupId,
		sarama.ErrInvalidSessionTimeout,
		sarama.ErrInvalidTopic,
		sarama.ErrPolicyViolation,
		sarama.ErrGroupMaxSizeReached,
		sarama.ErrFencedInstancedId,
		sarama.ErrInvalidConfig,
		sarama.ErrInvalidRequiredAcks,
		sarama.ErrUnsupportedCompressionType,
		sarama.ErrSecurityDisabled:
		return true
	default:
		return false
	}
}

func isTerminalErrorForConsumer(err error, resetInvalidOffsets bool) bool {
	if isTerminalSaramaError(err) {
		return true
	}
	if !resetInvalidOffsets && errors.Is(err, sarama.ErrOffsetOutOfRange) {
		return true
	}
	return false
}

func classifyConsumerError(err error) string {
	if err == nil {
		return "none"
	}
	if isIdentifiableCommitError(err) {
		return "commit"
	}
	if isTerminalSaramaError(err) {
		return "terminal"
	}
	return "transient"
}

// SafeErrorCodeProvider can be implemented by application errors to expose a bounded,
// non-sensitive error code for safe diagnostics, logs, DLQ metadata, and trace attributes.
type SafeErrorCodeProvider interface {
	SafeErrorCode() string
}

type codedError struct {
	err  error
	code string
}

func (e *codedError) Error() string         { return e.err.Error() }
func (e *codedError) Unwrap() error         { return e.err }
func (e *codedError) SafeErrorCode() string { return e.code }

// WithErrorCode wraps err with an application-defined, bounded, non-sensitive diagnostic code
// while preserving err through standard Go error unwrapping (errors.Is / errors.As).
// Calling WithErrorCode(nil, code) returns nil.
func WithErrorCode(err error, code string) error {
	if err == nil {
		return nil
	}
	return &codedError{
		err:  err,
		code: sanitizeErrorCode(code),
	}
}

func sanitizeErrorCode(raw string) string {
	const maxCodeLen = 64
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(min(len(raw), maxCodeLen))
	for i := 0; i < len(raw) && b.Len() < maxCodeLen; i++ {
		c := raw[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '.' || c == '_' || c == '-' || c == ':' {
			b.WriteByte(c)
		} else if c == ' ' || c == '/' {
			b.WriteByte('_')
		}
	}
	return strings.Trim(b.String(), "_")
}

type handlerPanicError struct {
	typeName string
	frames   []string
	wrapped  error
}

func capturePanicFrames(skip, maxFrames int) []string {
	pcs := make([]uintptr, maxFrames)
	n := runtime.Callers(skip, pcs)
	if n == 0 {
		return nil
	}
	frames := runtime.CallersFrames(pcs[:n])
	out := make([]string, 0, n)
	for {
		frame, more := frames.Next()
		fn := frame.Function
		if idx := strings.LastIndex(fn, "/"); idx >= 0 {
			fn = fn[idx+1:]
		}
		if fn != "" {
			out = append(out, fmt.Sprintf("%s:%d", fn, frame.Line))
		}
		if !more || len(out) >= maxFrames {
			break
		}
	}
	return out
}

func newHandlerPanicError(r any) error {
	pe := &handlerPanicError{
		typeName: fmt.Sprintf("%T", r),
		frames:   capturePanicFrames(4, 6),
	}
	if err, ok := r.(error); ok {
		pe.wrapped = err
	}
	return pe
}

func (e *handlerPanicError) Error() string {
	return "handler panic recovered"
}

func (e *handlerPanicError) Unwrap() error {
	return e.wrapped
}

type dlqPublishError struct {
	err      error
	category string
}

func (e *dlqPublishError) Error() string {
	return "dlq publication failed"
}

func (e *dlqPublishError) Unwrap() error {
	return e.err
}

func permanentDLQFailureCategory(err error) string {
	// A broker lookup can join errors from multiple brokers. Check each known
	// protocol code through the chain instead of taking the first Kafka error.
	switch {
	case errors.Is(err, ErrProducerClosed), errors.Is(err, sarama.ErrShuttingDown), errors.Is(err, sarama.ErrClosedClient):
		return "dlq_producer_closed"
	case errors.Is(err, sarama.ErrMessageSizeTooLarge), errors.Is(err, sarama.ErrMessageSetSizeTooLarge):
		return "dlq_message_too_large"
	case errors.Is(err, sarama.ErrTopicAuthorizationFailed), errors.Is(err, sarama.ErrGroupAuthorizationFailed),
		errors.Is(err, sarama.ErrClusterAuthorizationFailed), errors.Is(err, sarama.ErrTransactionalIDAuthorizationFailed),
		errors.Is(err, sarama.ErrDelegationTokenAuthorizationFailed):
		return "dlq_authorization_failed"
	case errors.Is(err, sarama.ErrSASLAuthenticationFailed), errors.Is(err, sarama.ErrUnsupportedSASLMechanism),
		errors.Is(err, sarama.ErrIllegalSASLState):
		return "dlq_authentication_failed"
	case errors.Is(err, sarama.ErrInvalidTopic):
		return "dlq_invalid_topic"
	}
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return "dlq_tls_verification_failed"
	}
	var cfgErr sarama.ConfigurationError
	if errors.As(err, &cfgErr) || errors.Is(err, sarama.ErrInvalidConfig) {
		return "dlq_configuration_error"
	}
	if isPermanentError(err) {
		return "dlq_permanent_error"
	}
	return ""
}

func classifyFailureCategory(err error) string {
	if err == nil {
		return "none"
	}
	var de *dlqPublishError
	if errors.As(err, &de) {
		return de.category
	}
	var pe *handlerPanicError
	if errors.As(err, &pe) {
		return "handler_panic"
	}
	var provider SafeErrorCodeProvider
	if errors.As(err, &provider) {
		if code := sanitizeErrorCode(provider.SafeErrorCode()); code != "" {
			return code
		}
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "handler_timeout"
	case errors.Is(err, context.Canceled):
		return "context_canceled"
	case errors.Is(err, ErrProducerClosed):
		return "producer_closed"
	case errors.Is(err, sarama.ErrClosedConsumerGroup), errors.Is(err, sarama.ErrClosedClient):
		return "consumer_closed"
	}
	var cfgErr sarama.ConfigurationError
	if errors.As(err, &cfgErr) {
		return "configuration_error"
	}
	var kerr sarama.KError
	if errors.As(err, &kerr) {
		return fmt.Sprintf("kafka_error_%d", int16(kerr))
	}
	return "handler_error"
}

func safeConsumerDiagnostic(err error) string {
	return safeConsumerDiagnosticForConsumer(err, true)
}

func safeConsumerDiagnosticForConsumer(err error, resetInvalidOffsets bool) string {
	if err == nil {
		return ""
	}
	class := classifyConsumerError(err)
	if isTerminalErrorForConsumer(err, resetInvalidOffsets) {
		class = "terminal"
	}
	return formatConsumerDiagnostic(err, class)
}

func formatConsumerDiagnostic(err error, class string) string {
	var de *dlqPublishError
	if errors.As(err, &de) {
		return class + ": " + de.category
	}
	var pe *handlerPanicError
	if errors.As(err, &pe) {
		return class + ": handler_panic"
	}
	var provider SafeErrorCodeProvider
	if errors.As(err, &provider) {
		if code := sanitizeErrorCode(provider.SafeErrorCode()); code != "" {
			return class + ": " + code
		}
	}
	var kerr sarama.KError
	if errors.As(err, &kerr) {
		return fmt.Sprintf("%s: kafka_error_%d", class, int16(kerr))
	}
	var cfgErr sarama.ConfigurationError
	if errors.As(err, &cfgErr) {
		return class + ": configuration_error"
	}
	switch {
	case errors.Is(err, sarama.ErrClosedConsumerGroup), errors.Is(err, sarama.ErrClosedClient):
		return class + ": consumer_closed"
	case errors.Is(err, context.DeadlineExceeded):
		return class + ": deadline_exceeded"
	case errors.Is(err, context.Canceled):
		return class + ": context_canceled"
	default:
		return class + ": consumer_error"
	}
}

func formatConsumerError(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", op, err)
}
