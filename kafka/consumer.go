package kafka

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/IBM/sarama"
)

type topicPartitionKey struct {
	topic     string
	partition int32
}

type compiledSubscription struct {
	topic          string
	handler        Handler
	handlerTimeout time.Duration
	policy         RetryPolicy
}

type consumer struct {
	ownerID      uint64
	brokerCfg    BrokerConfig
	consumerCfg  ConsumerConfig
	saramaConfig *sarama.Config
	logger       *slog.Logger
	options      consumerOptions

	mu                sync.Mutex
	readyMetricMu     sync.Mutex
	state             ConsumerState
	ready             bool
	assignedClaims    int
	activeSessionSeq  uint64
	readySessionSeq   uint64
	expectedClaims    map[topicPartitionKey]struct{}
	initializedClaims map[topicPartitionKey]struct{}
	lastError         string
	lastErrorAt       time.Time
	lastSessionAt     time.Time
	lastResolvedAt    time.Time
	lastSuccessfulAt  time.Time

	subscriptions map[string]compiledSubscription
	topicOrder    []string

	client          consumerGroupClient
	stopRequested   bool
	gracefulStarted atomic.Bool
	stopChOnce      sync.Once
	shutdownOnce    sync.Once
	teardownArmOnce sync.Once
	teardownArmed   chan struct{}
	shutdownDone    chan struct{}
	consumeExited   chan struct{}
	stopCh          chan struct{}
	gate            *admissionGate
	cancelRun       context.CancelCauseFunc
	terminalErr     error
	closeErr        error
	runDone         chan struct{}

	sessionCounter atomic.Int64
}

var nextConsumerOwnerID atomic.Uint64

// NewConsumer creates a new consumer group member with independent BrokerConfig and ConsumerConfig.
// Call Subscribe to register topic handlers, then Run to start consuming.
//
// Do not call Close() from within a message handler on the same consumer instance;
// cancel the application context instead to avoid waiting on itself.
func NewConsumer(brokerCfg BrokerConfig, consumerCfg ConsumerConfig, logger *slog.Logger, opts ...ConsumerOption) (Consumer, error) {
	var options consumerOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&options)
		}
	}
	if logger == nil {
		logger = slog.Default()
	}

	normalizedBroker, err := normalizeAndValidateBrokerConfig(brokerCfg)
	if err != nil {
		return nil, fmt.Errorf("invalid broker config: %w", err)
	}
	normalizedConsumer, err := normalizeAndValidateConsumerConfig(consumerCfg)
	if err != nil {
		return nil, fmt.Errorf("invalid consumer config: %w", err)
	}

	sc, err := buildConsumerSaramaConfig(normalizedBroker, normalizedConsumer)
	if err != nil {
		return nil, err
	}

	return &consumer{
		ownerID:       nextConsumerOwnerID.Add(1),
		brokerCfg:     normalizedBroker,
		consumerCfg:   normalizedConsumer,
		saramaConfig:  sc,
		logger:        logger,
		options:       options,
		state:         ConsumerStateRegistered,
		subscriptions: make(map[string]compiledSubscription),
	}, nil
}

// Subscribe registers a topic handler and policy prior to Run.
// Subscribe performs validation only; no broker I/O or goroutines are started.
func (c *consumer) Subscribe(sub Subscription) error {
	if err := validateTopicName(sub.Topic); err != nil {
		return fmt.Errorf("invalid subscription topic: %w", err)
	}
	if sub.Handler == nil {
		return fmt.Errorf("subscription handler is required for topic %q", sub.Topic)
	}
	if sub.HandlerTimeout < 0 {
		return fmt.Errorf("subscription handler_timeout cannot be negative for topic %q", sub.Topic)
	}
	for i, ic := range sub.Interceptors {
		if ic == nil {
			return fmt.Errorf("subscription interceptor at index %d cannot be nil for topic %q", i, sub.Topic)
		}
	}

	policy, err := normalizeAndValidateRetryPolicy(sub.Topic, sub.RetryPolicy)
	if err != nil {
		return fmt.Errorf("invalid retry policy for topic %q: %w", sub.Topic, err)
	}

	effectiveTimeout := sub.HandlerTimeout
	if effectiveTimeout == 0 {
		effectiveTimeout = c.consumerCfg.HandlerTimeout
	}

	interceptors := append([]Interceptor(nil), sub.Interceptors...)
	finalHandler := sub.Handler
	for i := len(interceptors) - 1; i >= 0; i-- {
		finalHandler = interceptors[i](finalHandler)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.state == ConsumerStateClosed {
		return ErrConsumerClosed
	}
	if c.state != ConsumerStateRegistered {
		return ErrConsumerAlreadyRunning
	}
	if _, exists := c.subscriptions[sub.Topic]; exists {
		return fmt.Errorf("duplicate subscription for topic %q", sub.Topic)
	}

	c.subscriptions[sub.Topic] = compiledSubscription{
		topic:          sub.Topic,
		handler:        finalHandler,
		handlerTimeout: effectiveTimeout,
		policy:         policy,
	}
	c.topicOrder = append(c.topicOrder, sub.Topic)
	return nil
}

// Run starts the consumer group loop for all registered subscriptions and blocks until
// graceful shutdown completes or a terminal failure occurs.
func (c *consumer) Run(ctx context.Context) (runRetErr error) {
	if ctx == nil {
		ctx = context.Background()
	}

	c.mu.Lock()
	if c.state == ConsumerStateClosed {
		c.mu.Unlock()
		return ErrConsumerClosed
	}
	if c.state != ConsumerStateRegistered {
		c.mu.Unlock()
		return ErrConsumerAlreadyRunning
	}
	if len(c.subscriptions) == 0 {
		c.mu.Unlock()
		return ErrNoSubscriptions
	}

	subs := maps.Clone(c.subscriptions)
	topics := append([]string(nil), c.topicOrder...)
	brokers := append([]string(nil), c.brokerCfg.Brokers...)

	if ctx.Err() != nil {
		c.state = ConsumerStateStopped
		c.ready = false
		c.mu.Unlock()
		return nil
	}

	c.state = ConsumerStateStarting
	c.ready = false
	runDone := make(chan struct{})
	shutdownDone := make(chan struct{})
	consumeExited := make(chan struct{})
	stopCh := make(chan struct{})
	teardownArmed := make(chan struct{})
	gate := newAdmissionGate()

	internalBase := context.WithoutCancel(ctx)
	internalCtx, cancelInternal := context.WithCancelCause(internalBase)
	abortNetCtx, abortNet := context.WithCancel(context.Background())

	c.runDone = runDone
	c.shutdownDone = shutdownDone
	c.consumeExited = consumeExited
	c.stopCh = stopCh
	c.teardownArmOnce = sync.Once{}
	c.teardownArmed = teardownArmed
	c.gate = gate
	c.cancelRun = cancelInternal
	c.mu.Unlock()

	runExiting := make(chan struct{})
	watchdogDone := make(chan struct{})
	go func() {
		defer close(watchdogDone)
		select {
		case <-teardownArmed:
		case <-runExiting:
			return
		}

		// Shutdown arms teardown after the drain completes or expires. A claim
		// can still hold admission while waiting for broker I/O at that point.
		timer := time.NewTimer(c.consumerCfg.CloseTimeout)
		defer timer.Stop()
		select {
		case <-timer.C:
			abortNet()
			c.logger.Warn("consumer close teardown timeout exceeded; aborting pending broker I/O",
				"group_id", c.consumerCfg.GroupID,
				"client_id", c.brokerCfg.ClientID,
				"close_timeout", c.consumerCfg.CloseTimeout,
			)
		case <-runExiting:
		}
	}()

	var (
		drainerDone chan struct{}
		watcherStop = make(chan struct{})
		watcherDone = make(chan struct{})
		consumeDone bool
	)

	defer func() {
		if !consumeDone {
			close(consumeExited)
		}
		close(watcherStop)
		<-watcherDone

		cancelInternal(errGracefulShutdown)
		c.armTeardown()

		c.mu.Lock()
		client := c.client
		c.mu.Unlock()

		var closeErr error
		if client != nil {
			closeErr = client.Close()
		}

		close(runExiting)
		<-watchdogDone
		abortNet()

		if drainerDone != nil {
			<-drainerDone
		}

		c.mu.Lock()
		if closeErr != nil && c.closeErr == nil {
			c.closeErr = closeErr
		}
		termErr := c.terminalErr
		c.ready = false
		c.assignedClaims = 0
		c.expectedClaims = nil
		c.initializedClaims = nil
		if c.state != ConsumerStateClosed {
			if termErr != nil || closeErr != nil {
				c.state = ConsumerStateFailed
			} else {
				c.state = ConsumerStateStopped
			}
		}
		c.mu.Unlock()

		c.publishReadiness()

		if termErr != nil && closeErr != nil {
			runRetErr = errors.Join(termErr, fmt.Errorf("consumer group close: %w", closeErr))
		} else if termErr != nil {
			runRetErr = termErr
		} else if closeErr != nil {
			runRetErr = fmt.Errorf("consumer group close: %w", closeErr)
		}

		close(runDone)
	}()

	go func() {
		defer close(watcherDone)
		select {
		case <-ctx.Done():
			c.initiateGracefulShutdown()
		case <-watcherStop:
		}
	}()

	if c.isStopRequested() || ctx.Err() != nil {
		return nil
	}

	factory := c.options.consumerGroupFactory
	if factory == nil {
		factory = func(addrs []string, groupID string, cfg *sarama.Config) (consumerGroupClient, error) {
			return sarama.NewConsumerGroup(addrs, groupID, cfg)
		}
	}

	scCopy := *c.saramaConfig
	if !scCopy.Net.Proxy.Enable {
		baseDialer := &net.Dialer{
			Timeout:   scCopy.Net.DialTimeout,
			KeepAlive: scCopy.Net.KeepAlive,
			LocalAddr: scCopy.Net.LocalAddr,
		}
		scCopy.Net.Proxy.Enable = true
		scCopy.Net.Proxy.Dialer = &cancelableDialer{
			ctx:    abortNetCtx,
			dialer: baseDialer,
		}
	}
	metadataBackoff := scCopy.Metadata.Retry.Backoff
	scCopy.Metadata.Retry.BackoffFunc = func(_, _ int) time.Duration {
		if abortNetCtx.Err() != nil {
			// Once the watchdog closes the sockets, retries cannot reconnect.
			return 0
		}
		return metadataBackoff
	}
	client, err := factory(brokers, c.consumerCfg.GroupID, &scCopy)
	if err != nil {
		if ctx.Err() == nil && !c.isStopRequested() {
			c.signalTerminalFailure(formatConsumerError("failed to create consumer group", err))
		}
		return nil
	}

	c.mu.Lock()
	stopNow := c.stopRequested || ctx.Err() != nil
	c.client = client
	c.mu.Unlock()

	drainerDone = make(chan struct{})
	go c.drainErrors(client.Errors(), drainerDone)

	if stopNow {
		return nil
	}

	groupHandler := &consumerGroupHandler{
		consumer:   c,
		subs:       subs,
		gate:       gate,
		stopCh:     stopCh,
		commitMode: c.consumerCfg.CommitMode,
	}

	c.consumeLoop(internalCtx, client, topics, groupHandler)
	close(consumeExited)
	consumeDone = true

	if c.gracefulStarted.Load() {
		<-shutdownDone
	}

	return nil
}

func (c *consumer) consumeLoop(
	internalCtx context.Context,
	client consumerGroupClient,
	topics []string,
	handler *consumerGroupHandler,
) {
	reconnectDelay := defaultReconnectInitialDelay
	rt := c.newRetryRuntime()

	for {
		if internalCtx.Err() != nil || c.isStopRequested() {
			return
		}

		handler.currentSessionSeq.Store(0)
		err := client.Consume(internalCtx, topics, handler)

		if internalCtx.Err() != nil || c.isStopRequested() {
			return
		}

		initialized := handler.sessionReachedReadiness()
		if initialized {
			reconnectDelay = defaultReconnectInitialDelay
		}
		if err != nil {
			c.recordError(err, false)
			resetInvalid := c.consumerCfg.ResetInvalidOffsets
			kind := classifyConsumerError(err)
			if isTerminalErrorForConsumer(err, resetInvalid) {
				kind = "terminal"
			}
			c.notifyConsumerError(kind)
			c.logger.Error("consumer group consume error",
				"group_id", c.consumerCfg.GroupID,
				"client_id", c.brokerCfg.ClientID,
				"error_class", kind,
				"failure_category", classifyFailureCategory(err),
			)

			if isTerminalErrorForConsumer(err, resetInvalid) {
				c.signalTerminalFailure(formatConsumerError("terminal consumer error", err))
				return
			}
		} else if initialized {
			continue
		}

		// Sarama can return nil after Setup when a claim failed to initialize.
		wait := rt.jitter(reconnectDelay)
		if sleepErr := rt.sleep(internalCtx, wait); sleepErr != nil {
			return
		}
		reconnectDelay = nextBackoffDelay(reconnectDelay, defaultReconnectMaxDelay, 2.0)
	}
}

func (c *consumer) drainErrors(errCh <-chan error, done chan<- struct{}) {
	defer close(done)
	resetInvalid := c.consumerCfg.ResetInvalidOffsets
	for err := range errCh {
		if err == nil {
			continue
		}
		if isIdentifiableCommitError(err) {
			c.notifyCommitError()
		}
		kind := classifyConsumerError(err)
		if isTerminalErrorForConsumer(err, resetInvalid) {
			kind = "terminal"
		}
		c.notifyConsumerError(kind)

		// Do not treat expected closed-group errors during shutdown as an unexpected failure.
		if (errors.Is(err, sarama.ErrClosedConsumerGroup) || errors.Is(err, sarama.ErrClosedClient)) && c.isStopRequested() {
			continue
		}

		c.recordError(err, true)
		logArgs := []any{
			"group_id", c.consumerCfg.GroupID,
			"client_id", c.brokerCfg.ClientID,
			"error_class", kind,
			"failure_category", classifyFailureCategory(err),
		}
		var ce *sarama.ConsumerError
		if errors.As(err, &ce) {
			logArgs = append(logArgs, "topic", ce.Topic, "partition", ce.Partition)
		}
		c.logger.Error("consumer group reported error", logArgs...)

		if isTerminalErrorForConsumer(err, resetInvalid) {
			c.signalTerminalFailure(formatConsumerError("terminal consumer error", err))
		}
	}
}

func (c *consumer) armTeardown() {
	c.teardownArmOnce.Do(func() {
		if c.teardownArmed != nil {
			close(c.teardownArmed)
		}
	})
}

func (c *consumer) closeStopCh() {
	c.stopChOnce.Do(func() {
		if c.stopCh != nil {
			close(c.stopCh)
		}
	})
}

func (c *consumer) initiateGracefulShutdown() {
	c.shutdownOnce.Do(func() {
		c.gracefulStarted.Store(true)
		c.mu.Lock()
		gate := c.gate
		cancelRun := c.cancelRun
		consumeExited := c.consumeExited
		shutdownDone := c.shutdownDone
		termErr := c.terminalErr
		c.stopRequested = true
		c.ready = false
		if c.state == ConsumerStateStarting || c.state == ConsumerStateRunning || c.state == ConsumerStateRebalancing {
			c.state = ConsumerStateDraining
		}
		c.mu.Unlock()

		// Close admission gate first as the atomic cutoff before stop notifications,
		// readiness metric updates, or logging. Never hold c.mu while draining.
		var drained <-chan struct{}
		if gate != nil {
			drained = gate.CloseAndDrainedCh()
		}

		c.closeStopCh()
		c.publishReadiness()

		c.logger.Info("consumer group shutting down",
			"group_id", c.consumerCfg.GroupID,
			"client_id", c.brokerCfg.ClientID,
			"shutdown_timeout", c.consumerCfg.ShutdownTimeout,
		)

		if drained != nil && termErr == nil {
			timer := time.NewTimer(c.consumerCfg.ShutdownTimeout)
			select {
			case <-drained:
			case <-consumeExited:
			case <-timer.C:
				c.logger.Warn("consumer shutdown drain timeout exceeded; cancelling active claims",
					"group_id", c.consumerCfg.GroupID,
					"client_id", c.brokerCfg.ClientID,
					"shutdown_timeout", c.consumerCfg.ShutdownTimeout,
				)
			}
			timer.Stop()
		}

		if cancelRun != nil {
			cancelRun(errGracefulShutdown)
		}
		c.armTeardown()
		if shutdownDone != nil {
			close(shutdownDone)
		}
	})
}

func (c *consumer) signalTerminalFailure(err error) {
	c.mu.Lock()
	gate := c.gate
	cancelRun := c.cancelRun
	c.stopRequested = true
	c.ready = false
	if c.terminalErr == nil && err != nil {
		c.terminalErr = err
		c.lastError = formatConsumerDiagnostic(err, "terminal")
		c.lastErrorAt = time.Now()
	}
	c.mu.Unlock()

	// Close admission gate first as the atomic cutoff before stop notifications or readiness updates.
	if gate != nil {
		gate.CloseAndDrainedCh()
	}

	c.closeStopCh()
	c.publishReadiness()
	if cancelRun != nil {
		cancelRun(err)
	}
	c.armTeardown()
}

func (c *consumer) isStopRequested() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stopRequested
}

func (c *consumer) recordError(err error, preserveReadiness bool) {
	if err == nil {
		return
	}
	c.mu.Lock()
	if c.terminalErr != nil {
		c.mu.Unlock()
		return
	}
	c.lastError = safeConsumerDiagnosticForConsumer(err, c.consumerCfg.ResetInvalidOffsets)
	c.lastErrorAt = time.Now()
	if !preserveReadiness {
		c.ready = false
	}
	c.mu.Unlock()
	if !preserveReadiness {
		c.publishReadiness()
	}
}

func (c *consumer) Health() ConsumerHealth {
	c.mu.Lock()
	defer c.mu.Unlock()
	return ConsumerHealth{
		State:            c.state,
		Ready:            c.ready,
		AssignedClaims:   c.assignedClaims,
		LastError:        c.lastError,
		LastErrorAt:      c.lastErrorAt,
		LastSessionAt:    c.lastSessionAt,
		LastResolvedAt:   c.lastResolvedAt,
		LastSuccessfulAt: c.lastSuccessfulAt,
	}
}

// Close initiates graceful shutdown (if running) and waits for cleanup to finish.
// Calling Close before Run marks the consumer closed without allocating broker resources.
// Repeated and concurrent Close calls are safe.
func (c *consumer) Close() error {
	c.mu.Lock()
	if c.state == ConsumerStateRegistered {
		c.state = ConsumerStateClosed
		c.ready = false
		c.stopRequested = true
		err := c.closeErr
		c.mu.Unlock()
		return err
	}
	if c.state == ConsumerStateClosed {
		err := c.closeErr
		c.mu.Unlock()
		return err
	}
	if c.state == ConsumerStateStopped || c.state == ConsumerStateFailed {
		c.state = ConsumerStateClosed
		c.ready = false
		err := c.closeErr
		c.mu.Unlock()
		return err
	}
	runDone := c.runDone
	c.mu.Unlock()

	c.initiateGracefulShutdown()
	if runDone != nil {
		<-runDone
	}

	c.mu.Lock()
	c.state = ConsumerStateClosed
	c.ready = false
	err := c.closeErr
	c.mu.Unlock()
	return err
}

func (c *consumer) newRetryRuntime() *retryRuntime {
	rt := &retryRuntime{
		groupID:  c.consumerCfg.GroupID,
		clientID: c.brokerCfg.ClientID,
		logger:   c.logger,
		sleepFn:  c.options.sleepFn,
		jitterFn: c.options.jitterFn,
	}
	if m := c.options.metrics; m != nil {
		groupID := c.consumerCfg.GroupID
		rt.onAttemptFail = func(topic string) {
			m.recordAttemptFail(groupID, topic)
		}
		rt.onRetry = func(topic string) {
			m.recordRetry(groupID, topic)
		}
		rt.onAttemptDone = func(topic string, duration time.Duration, outcome string) {
			m.recordAttemptDuration(groupID, topic, duration, outcome)
		}
		rt.onDLQAttempt = func(sourceTopic string, success bool) {
			m.recordDLQAttempt(groupID, sourceTopic, success)
		}
	}
	if kt := c.options.tracing; kt != nil {
		groupID := c.consumerCfg.GroupID
		rt.startAttempt = func(ctx context.Context, msg *Message, attempt int) (context.Context, func(error)) {
			return kt.startConsumeSpan(ctx, groupID, msg, attempt)
		}
	}
	return rt
}

func (c *consumer) publishReadiness() {
	if c.options.metrics == nil {
		return
	}
	c.readyMetricMu.Lock()
	defer c.readyMetricMu.Unlock()
	c.mu.Lock()
	ready := c.ready
	c.mu.Unlock()
	c.options.metrics.recordReady(c.consumerCfg.GroupID, ready)
}

func (c *consumer) notifyRebalance() {
	c.options.metrics.recordRebalance(c.consumerCfg.GroupID)
}

func (c *consumer) notifyCommitAttempt() {
	c.options.metrics.recordCommitAttempt(c.consumerCfg.GroupID)
}

func (c *consumer) notifyCommitError() {
	c.options.metrics.recordCommitError(c.consumerCfg.GroupID)
}

func (c *consumer) notifyConsumerError(kind string) {
	c.options.metrics.recordConsumerError(c.consumerCfg.GroupID, kind)
}

func (c *consumer) notifyOutcome(topic string, res processingResult) {
	c.options.metrics.recordOutcome(c.consumerCfg.GroupID, topic, res)
}

func (c *consumer) notifyLag(topic string, partition int32, lag int64) {
	c.options.metrics.recordLag(c.ownerID, c.consumerCfg.GroupID, topic, partition, lag)
}

func (c *consumer) notifyClaimCleanup(topic string, partition int32) {
	c.options.metrics.cleanupLag(c.ownerID, c.consumerCfg.GroupID, topic, partition)
}

type consumerGroupHandler struct {
	consumer   *consumer
	subs       map[string]compiledSubscription
	gate       *admissionGate
	stopCh     <-chan struct{}
	commitMode CommitMode

	currentSessionSeq  atomic.Uint64
	stopSessionWatcher func()
	commitMu           sync.Mutex
	hasMarked          atomic.Bool
}

func (h *consumerGroupHandler) sessionReachedReadiness() bool {
	seq := h.currentSessionSeq.Load()
	h.consumer.mu.Lock()
	defer h.consumer.mu.Unlock()
	return seq != 0 && seq == h.consumer.readySessionSeq
}

func (h *consumerGroupHandler) Setup(session sarama.ConsumerGroupSession) error {
	h.hasMarked.Store(false)

	expected := make(map[topicPartitionKey]struct{})
	for topic, parts := range session.Claims() {
		for _, p := range parts {
			expected[topicPartitionKey{topic: topic, partition: p}] = struct{}{}
		}
	}
	totalClaims := len(expected)

	sessionNum := h.consumer.sessionCounter.Add(1)
	if sessionNum > 1 {
		h.consumer.notifyRebalance()
	}

	sessionCtx := session.Context()
	seq := h.consumer.setupSessionClaims(sessionCtx, expected)
	h.currentSessionSeq.Store(seq)
	watcherDone := make(chan struct{})
	stopWatcher := context.AfterFunc(sessionCtx, func() {
		defer close(watcherDone)
		h.consumer.invalidateSessionReadiness(seq)
	})
	h.stopSessionWatcher = func() {
		if !stopWatcher() {
			<-watcherDone
		}
	}

	h.consumer.publishReadiness()
	h.consumer.logger.Info("consumer group session started",
		"group_id", h.consumer.consumerCfg.GroupID,
		"client_id", h.consumer.brokerCfg.ClientID,
		"member_id", session.MemberID(),
		"generation_id", session.GenerationID(),
		"assigned_claims", totalClaims,
	)
	return nil
}

func (c *consumer) setupSessionClaims(ctx context.Context, expected map[topicPartitionKey]struct{}) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.activeSessionSeq++
	seq := c.activeSessionSeq
	c.expectedClaims = expected
	c.initializedClaims = make(map[topicPartitionKey]struct{}, len(expected))
	c.assignedClaims = len(expected)
	c.lastSessionAt = time.Now()

	canBeReady := !c.stopRequested && c.terminalErr == nil &&
		c.state != ConsumerStateClosed && c.state != ConsumerStateDraining &&
		c.state != ConsumerStateFailed && c.state != ConsumerStateStopped

	if len(expected) == 0 && canBeReady && ctx.Err() == nil {
		c.ready = true
		c.readySessionSeq = seq
		c.state = ConsumerStateRunning
	} else {
		c.ready = false
		if c.state == ConsumerStateRunning {
			c.state = ConsumerStateRebalancing
		}
	}
	return seq
}

func (c *consumer) markClaimInitialized(ctx context.Context, seq uint64, topic string, partition int32) {
	c.mu.Lock()
	if seq == 0 || seq != c.activeSessionSeq || ctx.Err() != nil {
		c.mu.Unlock()
		return
	}
	key := topicPartitionKey{topic: topic, partition: partition}
	if _, expected := c.expectedClaims[key]; !expected {
		c.mu.Unlock()
		return
	}
	if c.initializedClaims == nil {
		c.initializedClaims = make(map[topicPartitionKey]struct{}, len(c.expectedClaims))
	}
	c.initializedClaims[key] = struct{}{}

	canBeReady := !c.stopRequested && c.terminalErr == nil &&
		c.state != ConsumerStateClosed && c.state != ConsumerStateDraining &&
		c.state != ConsumerStateFailed && c.state != ConsumerStateStopped

	becameReady := false
	if canBeReady && len(c.initializedClaims) >= len(c.expectedClaims) {
		if !c.ready || c.state != ConsumerStateRunning {
			c.ready = true
			c.readySessionSeq = seq
			c.state = ConsumerStateRunning
			becameReady = true
		}
	}
	c.mu.Unlock()

	if becameReady {
		c.publishReadiness()
	}
}

func (c *consumer) invalidateSessionReadiness(seq uint64) {
	c.mu.Lock()
	if seq != c.activeSessionSeq {
		c.mu.Unlock()
		return
	}
	c.ready = false
	if c.state == ConsumerStateRunning {
		c.state = ConsumerStateRebalancing
	}
	c.mu.Unlock()
	c.publishReadiness()
}

func (h *consumerGroupHandler) Cleanup(session sarama.ConsumerGroupSession) error {
	h.consumer.invalidateSessionReadiness(h.currentSessionSeq.Load())
	if h.stopSessionWatcher != nil {
		h.stopSessionWatcher()
		h.stopSessionWatcher = nil
	}

	if h.commitMode == CommitSync && h.hasMarked.Load() {
		h.commitMu.Lock()
		h.consumer.notifyCommitAttempt()
		session.Commit()
		h.commitMu.Unlock()
	}

	for topic, parts := range session.Claims() {
		for _, p := range parts {
			h.consumer.notifyClaimCleanup(topic, p)
		}
	}

	h.consumer.mu.Lock()
	h.consumer.activeSessionSeq++
	h.consumer.expectedClaims = nil
	h.consumer.initializedClaims = nil
	h.consumer.assignedClaims = 0
	h.consumer.mu.Unlock()

	h.consumer.logger.Info("consumer group session ended",
		"group_id", h.consumer.consumerCfg.GroupID,
		"client_id", h.consumer.brokerCfg.ClientID,
		"member_id", session.MemberID(),
		"generation_id", session.GenerationID(),
	)
	return nil
}

func (h *consumerGroupHandler) ConsumeClaim(session sarama.ConsumerGroupSession, claim sarama.ConsumerGroupClaim) error {
	sessionCtx := session.Context()
	topic := claim.Topic()
	partition := claim.Partition()

	sub, ok := h.subs[topic]
	if !ok {
		err := fmt.Errorf("unexpected topic %q received with no registered handler", topic)
		h.consumer.signalTerminalFailure(err)
		return err
	}

	h.consumer.markClaimInitialized(sessionCtx, h.currentSessionSeq.Load(), topic, partition)
	initialOffset := claim.InitialOffset()
	// Sarama falls back to the configured sentinel without resetting its offset
	// manager. Reset on the first resolved record so a higher commit cannot mask it.
	resetOnResolve := h.consumer.consumerCfg.ResetInvalidOffsets &&
		(initialOffset == sarama.OffsetOldest || initialOffset == sarama.OffsetNewest)

	rt := h.consumer.newRetryRuntime()
	rt.onAttemptStart = func(m *Message) {
		h.updateClaimLag(topic, partition, claim.HighWaterMarkOffset(), m.Offset)
	}
	h.updateClaimLag(topic, partition, claim.HighWaterMarkOffset(), initialOffset)

	for {
		if sessionCtx.Err() != nil {
			return nil
		}

		// During graceful shutdown, idle claims must wait for session cancellation
		// instead of returning early and interrupting active claims in other partitions.
		if h.gate.IsStopping() {
			<-sessionCtx.Done()
			return nil
		}

		var msg *sarama.ConsumerMessage
		var chanOpen bool
		select {
		case <-sessionCtx.Done():
			return nil
		case <-h.stopCh:
			<-sessionCtx.Done()
			return nil
		case msg, chanOpen = <-claim.Messages():
			if !chanOpen || msg == nil {
				return nil
			}
		}

		if !h.gate.TryAdmit(sessionCtx) {
			if sessionCtx.Err() != nil {
				return nil
			}
			<-sessionCtx.Done()
			return nil
		}

		kafkaMsg := convertSaramaMessage(msg)
		res := rt.processMessage(sessionCtx, kafkaMsg, sub.handler, sub.policy, sub.handlerTimeout)

		if sessionCtx.Err() != nil || res.Outcome == outcomeInterrupted {
			res.Outcome = outcomeInterrupted
			h.consumer.notifyOutcome(topic, res)
			h.gate.Release()
			return nil
		}

		switch res.Outcome {
		case outcomeProcessed, outcomeDeadLettered, outcomeDiscarded:
			if resetOnResolve {
				// ResetOffset is a no-op for fresh groups and for commits below
				// this offset. MarkMessage handles both cases as usual.
				session.ResetOffset(topic, partition, msg.Offset+1, "")
				resetOnResolve = false
			}
			session.MarkMessage(msg, "")
			h.hasMarked.Store(true)
			if h.commitMode == CommitSync {
				h.commitMu.Lock()
				h.consumer.notifyCommitAttempt()
				session.Commit()
				h.commitMu.Unlock()
			}
			now := time.Now()
			h.consumer.mu.Lock()
			h.consumer.lastResolvedAt = now
			if res.Outcome == outcomeProcessed {
				h.consumer.lastSuccessfulAt = now
			}
			h.consumer.mu.Unlock()
			h.consumer.notifyOutcome(topic, res)
			h.updateClaimLag(topic, partition, claim.HighWaterMarkOffset(), msg.Offset+1)
			h.gate.Release()

		case outcomeTerminalFailure:
			h.consumer.notifyOutcome(topic, res)
			h.consumer.signalTerminalFailure(res.Err)
			h.gate.Release()
			return res.Err

		default:
			invErr := fmt.Errorf("invalid message processing disposition %d", res.Outcome)
			h.consumer.signalTerminalFailure(invErr)
			h.gate.Release()
			return invErr
		}
	}
}

func (h *consumerGroupHandler) updateClaimLag(topic string, partition int32, hwm, nextOffset int64) {
	if nextOffset < 0 {
		return
	}
	lag := hwm - nextOffset
	if lag < 0 {
		lag = 0
	}
	h.consumer.notifyLag(topic, partition, lag)
}

func convertSaramaMessage(msg *sarama.ConsumerMessage) *Message {
	headers := make([]RecordHeader, len(msg.Headers))
	for i, hdr := range msg.Headers {
		headers[i] = RecordHeader{
			Key:   string(hdr.Key),
			Value: cloneBytesPreserveNil(hdr.Value),
		}
	}
	return &Message{
		Key:       msg.Key,
		Value:     msg.Value,
		Topic:     msg.Topic,
		Partition: msg.Partition,
		Offset:    msg.Offset,
		Headers:   headers,
		Timestamp: msg.Timestamp,
	}
}
