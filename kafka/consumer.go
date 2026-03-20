package kafka

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/IBM/sarama"
)

// consumer implements the Consumer interface using Sarama.
type consumer struct {
	brokers      []string
	groupID      string
	config       Config
	saramaConfig *sarama.Config
	client       sarama.ConsumerGroup
	logger       *slog.Logger
}

// NewConsumer creates a new Kafka consumer.
func NewConsumer(cfg Config, logger *slog.Logger) (Consumer, error) {
	if len(cfg.Brokers) == 0 {
		return nil, fmt.Errorf("no brokers specified")
	}

	if cfg.Consumer.GroupID == "" {
		return nil, fmt.Errorf("consumer group ID is required")
	}

	if logger == nil {
		logger = slog.Default()
	}

	saramaConfig := sarama.NewConfig()
	saramaConfig.ClientID = cfg.ClientID
	saramaConfig.Consumer.Group.Rebalance.GroupStrategies = []sarama.BalanceStrategy{
		sarama.NewBalanceStrategyRoundRobin(),
	}

	// Set initial offset
	if cfg.Consumer.InitialOffset == "oldest" {
		saramaConfig.Consumer.Offsets.Initial = sarama.OffsetOldest
	} else {
		saramaConfig.Consumer.Offsets.Initial = sarama.OffsetNewest
	}

	// Set session timeout
	if cfg.Consumer.SessionTimeout > 0 {
		saramaConfig.Consumer.Group.Session.Timeout = cfg.Consumer.SessionTimeout
	}

	// Set heartbeat interval
	if cfg.Consumer.HeartbeatInterval > 0 {
		saramaConfig.Consumer.Group.Heartbeat.Interval = cfg.Consumer.HeartbeatInterval
	}

	// Set max processing time
	if cfg.Consumer.MaxProcessingTime > 0 {
		saramaConfig.Consumer.MaxProcessingTime = cfg.Consumer.MaxProcessingTime
	}

	// Set auto-commit
	if cfg.Consumer.CommitInterval > 0 {
		saramaConfig.Consumer.Offsets.AutoCommit.Enable = true
		saramaConfig.Consumer.Offsets.AutoCommit.Interval = cfg.Consumer.CommitInterval
	} else {
		saramaConfig.Consumer.Offsets.AutoCommit.Enable = false
	}

	// Return errors
	saramaConfig.Consumer.Return.Errors = true

	// Configure SASL if enabled
	if cfg.Auth.SASL.Enabled {
		saramaConfig.Net.SASL.Enable = true
		saramaConfig.Net.SASL.User = cfg.Auth.SASL.Username
		saramaConfig.Net.SASL.Password = cfg.Auth.SASL.Password

		switch cfg.Auth.SASL.Mechanism {
		case "PLAIN":
			saramaConfig.Net.SASL.Mechanism = sarama.SASLTypePlaintext
		case "SCRAM-SHA-256":
			saramaConfig.Net.SASL.Mechanism = sarama.SASLTypeSCRAMSHA256
		case "SCRAM-SHA-512":
			saramaConfig.Net.SASL.Mechanism = sarama.SASLTypeSCRAMSHA512
		default:
			return nil, fmt.Errorf("unsupported SASL mechanism: %s", cfg.Auth.SASL.Mechanism)
		}

		// Set up SCRAM client if needed
		if cfg.Auth.SASL.Mechanism == "SCRAM-SHA-256" || cfg.Auth.SASL.Mechanism == "SCRAM-SHA-512" {
			saramaConfig.Net.SASL.SCRAMClientGeneratorFunc = func() sarama.SCRAMClient {
				if cfg.Auth.SASL.Mechanism == "SCRAM-SHA-512" {
					return &XDGSCRAMClient{HashGeneratorFcn: SHA512}
				}
				return &XDGSCRAMClient{HashGeneratorFcn: SHA256}
			}
		}
	}

	// Configure TLS if enabled
	if cfg.Auth.TLS.Enabled {
		tlsConfig := &tls.Config{
			InsecureSkipVerify: cfg.Auth.TLS.InsecureSkipVerify,
		}

		// Load client cert if specified
		if cfg.Auth.TLS.CertFile != "" && cfg.Auth.TLS.KeyFile != "" {
			cert, err := tls.LoadX509KeyPair(cfg.Auth.TLS.CertFile, cfg.Auth.TLS.KeyFile)
			if err != nil {
				return nil, fmt.Errorf("failed to load client cert: %w", err)
			}
			tlsConfig.Certificates = []tls.Certificate{cert}
		}

		// Load CA cert if specified
		if cfg.Auth.TLS.CAFile != "" {
			caCert, err := os.ReadFile(cfg.Auth.TLS.CAFile)
			if err != nil {
				return nil, fmt.Errorf("failed to read CA cert: %w", err)
			}
			caCertPool := x509.NewCertPool()
			caCertPool.AppendCertsFromPEM(caCert)
			tlsConfig.RootCAs = caCertPool
		}

		saramaConfig.Net.TLS.Enable = true
		saramaConfig.Net.TLS.Config = tlsConfig
	}

	// Validate config
	if err := saramaConfig.Validate(); err != nil {
		return nil, fmt.Errorf("invalid sarama config: %w", err)
	}

	return &consumer{
		brokers:      cfg.Brokers,
		groupID:      cfg.Consumer.GroupID,
		config:       cfg,
		saramaConfig: saramaConfig,
		logger:       logger,
	}, nil
}

// Subscribe subscribes to a single topic with a handler.
func (c *consumer) Subscribe(ctx context.Context, topic string, handler Handler, opts *SubscribeOptions) error {
	return c.SubscribeMultiple(ctx, []string{topic}, handler, opts)
}

func validateOpts(opts *SubscribeOptions) error {
	if opts.AutoCommit < 0 {
		return fmt.Errorf("auto-commit interval cannot be negative")
	}
	if opts.RetryPolicy == nil {
		return nil
	}

	strategy := opts.RetryPolicy.Strategy
	if strategy == "" {
		strategy = RetryStrategyLogAndIgnore
	}

	if opts.RetryPolicy.MaxRetries < 0 {
		return fmt.Errorf("retry policy max retries cannot be negative.")
	}

	switch strategy {
	case RetryStrategyLogAndIgnore:
		// No retry timing constraints are required; message is logged and skipped.
		return nil

	case RetryStrategyInfinite:
		if opts.RetryPolicy.InitialDelay <= 0 {
			return fmt.Errorf("retry policy initial delay must be greater than zero for infinite strategy")
		}
		if opts.RetryPolicy.MaxDelay <= 0 {
			return fmt.Errorf("retry policy max delay must be greater than zero for infinite strategy")
		}
		if opts.RetryPolicy.Multiplier < 1 {
			return fmt.Errorf("retry policy multiplier must be at least 1 for infinite strategy")
		}
		return nil

	case RetryStrategyDLQ:
		if opts.RetryPolicy.InitialDelay <= 0 {
			return fmt.Errorf("retry policy initial delay must be greater than zero for dlq strategy")
		}
		if opts.RetryPolicy.MaxDelay <= 0 {
			return fmt.Errorf("retry policy max delay must be greater than zero for dlq strategy")
		}
		if opts.RetryPolicy.Multiplier < 1 {
			return fmt.Errorf("retry policy multiplier must be at least 1 for dlq strategy")
		}
		if opts.RetryPolicy.DLQTopic == "" {
			return fmt.Errorf("DLQ topic must be specified for DLQ retry strategy")
		}
		if opts.RetryPolicy.DLQProducer == nil {
			return fmt.Errorf("DLQ producer must be specified for DLQ retry strategy")
		}
		return nil

	default:
		return fmt.Errorf("invalid retry strategy: %s", opts.RetryPolicy.Strategy)
	}
}

// SubscribeMultiple subscribes to multiple topics with the same handler.
func (c *consumer) SubscribeMultiple(ctx context.Context, topics []string, handler Handler, opts *SubscribeOptions) error {
	if len(topics) == 0 {
		return fmt.Errorf("no topics specified")
	}

	if handler == nil {
		return fmt.Errorf("handler is required")
	}

	if opts == nil {
		opts = DefaultSubscribeOptions()
		opts.AutoCommit = c.config.Consumer.CommitInterval
	}

	if err := validateOpts(opts); err != nil {
		return fmt.Errorf("invalid subscribe options: %w", err)
	}

	// Apply interceptors to handler
	finalHandler := handler
	for i := len(opts.Interceptors) - 1; i >= 0; i-- {
		finalHandler = opts.Interceptors[i](finalHandler)
	}

	// Configure commit behavior.
	// AutoCommit > 0: Sarama interval auto-commit
	// AutoCommit == 0: disable auto-commit and sync-commit after each processed message
	saramaConfig := *c.saramaConfig
	if opts.AutoCommit > 0 {
		saramaConfig.Consumer.Offsets.AutoCommit.Enable = true
		saramaConfig.Consumer.Offsets.AutoCommit.Interval = opts.AutoCommit
	} else {
		saramaConfig.Consumer.Offsets.AutoCommit.Enable = false
	}

	// Create consumer group
	client, err := sarama.NewConsumerGroup(c.brokers, c.groupID, &saramaConfig)
	if err != nil {
		return fmt.Errorf("failed to create consumer group: %w", err)
	}
	c.client = client

	// Sarama requires draining the Errors() channel when Consumer.Return.Errors is enabled.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case err, ok := <-client.Errors():
				if !ok {
					return
				}
				c.logger.Error("consumer group reported error", "error", err)
			}
		}
	}()

	// Create consumer group handler
	consumerHandler := &consumerGroupHandler{
		handler:         finalHandler,
		logger:          c.logger,
		retryPolicy:     opts.RetryPolicy,
		commitAfterEach: opts.AutoCommit == 0,
	}

	// Start consuming in a loop (handles rebalancing)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
				if err := client.Consume(ctx, topics, consumerHandler); err != nil {
					c.logger.Error("consumer error", "error", err)
				}
				// Check if context was cancelled
				if ctx.Err() != nil {
					return
				}
			}
		}
	}()

	// Wait for context cancellation
	<-ctx.Done()
	return nil
}

// Close closes the consumer.
func (c *consumer) Close() error {
	if c.client != nil {
		return c.client.Close()
	}
	return nil
}

// consumerGroupHandler implements sarama.ConsumerGroupHandler
type consumerGroupHandler struct {
	handler         Handler
	logger          *slog.Logger
	retryPolicy     *RetryPolicy
	commitAfterEach bool
}

// Setup is run at the beginning of a new session, before ConsumeClaim
func (h *consumerGroupHandler) Setup(sarama.ConsumerGroupSession) error {
	h.logger.Info("consumer group session started")
	return nil
}

// Cleanup is run at the end of a session, once all ConsumeClaim goroutines have exited
func (h *consumerGroupHandler) Cleanup(sarama.ConsumerGroupSession) error {
	h.logger.Info("consumer group session ended")
	return nil
}

// ConsumeClaim processes messages from a partition
// ConsumeClaim processes messages from a partition
func (h *consumerGroupHandler) ConsumeClaim(session sarama.ConsumerGroupSession, claim sarama.ConsumerGroupClaim) error {
	for {
		select {
		case msg := <-claim.Messages():
			if msg == nil {
				return nil
			}

			// Convert sarama message to our Message type
			kafkaMsg := h.convertMessage(msg)

			// Process synchronously
			err := h.processWithRetry(session.Context(), kafkaMsg, h.handler)
			if err != nil {
				h.logger.Error("message processing failed",
					"topic", kafkaMsg.Topic,
					"partition", kafkaMsg.Partition,
					"offset", kafkaMsg.Offset,
					"error", err,
				)
				// We still advance offsets after logging the failure.
				// Final behavior on failures is controlled by retry strategy.
			}

			// Mark offset
			session.MarkMessage(msg, "")
			// If auto-commit is disabled via AutoCommit == 0, commit immediately.
			if h.commitAfterEach {
				session.Commit()
			}

		case <-session.Context().Done():
			return nil
		}
	}
}

// convertMessage converts a Sarama message to our Message type
func (h *consumerGroupHandler) convertMessage(msg *sarama.ConsumerMessage) *Message {
	headers := make([]RecordHeader, len(msg.Headers))
	for i, hdr := range msg.Headers {
		headers[i] = RecordHeader{
			Key:   string(hdr.Key),
			Value: hdr.Value,
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

// processWithRetry processes a message with retry logic
func (h *consumerGroupHandler) processWithRetry(ctx context.Context, msg *Message, handler Handler) error {
	var err error
	if h.retryPolicy == nil {
		return handler(ctx, msg)
	}

	delay := h.retryPolicy.InitialDelay
	maxRetries := h.retryPolicy.MaxRetries
	strategy := h.retryPolicy.Strategy

	// Default to LogAndIgnore if not specified
	if strategy == "" {
		strategy = RetryStrategyLogAndIgnore
	}

	// For infinite strategy, we loop forever (or until context cancelled)
	// For others, we loop until maxRetries
	attempt := 0
	for {
		// correct logic: check attempt limit only if NOT infinite
		if strategy != RetryStrategyInfinite && attempt > maxRetries {
			break
		}

		if attempt > 0 {
			// Wait before retry
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return ctx.Err()
			}

			h.logger.Warn("retrying message",
				"topic", msg.Topic,
				"partition", msg.Partition,
				"offset", msg.Offset,
				"attempt", attempt,
				"delay", delay,
			)

			// Exponential backoff
			delay = time.Duration(float64(delay) * h.retryPolicy.Multiplier)
			if delay > h.retryPolicy.MaxDelay {
				delay = h.retryPolicy.MaxDelay
			}
		}

		err = handler(ctx, msg)
		if err == nil {
			return nil
		}

		if strategy == RetryStrategyLogAndIgnore {
			// We break from here as we are not retrying
			break
		}

		attempt++
	}

	// If we are here, we exhausted retries (for non-infinite strategies)
	// Use explicit strategy handling
	switch strategy {
	case RetryStrategyDLQ:
		if h.retryPolicy.DLQTopic != "" && h.retryPolicy.DLQProducer != nil {
			h.logger.Error("sending message to DLQ after retry exhaustion",
				"topic", msg.Topic,
				"partition", msg.Partition,
				"offset", msg.Offset,
				"error", "retries exhausted", // We don't have the last specific error easily here without tracking, but context is clear
			)

			dlqErr := h.retryPolicy.DLQProducer.PublishWithHeaders(
				ctx,
				h.retryPolicy.DLQTopic,
				msg.Key,
				msg.Value,
				append(msg.Headers, RecordHeader{
					Key:   "x-original-topic",
					Value: []byte(msg.Topic),
				}, RecordHeader{
					Key:   "x-original-error",
					Value: []byte(err.Error()),
				}),
			)
			if dlqErr != nil {
				h.logger.Error("failed to send to DLQ", "error", dlqErr)
				// Fallback to error return (effectively ignore with error log if caller logs it)
				return fmt.Errorf("retries exhausted and DLQ failed: %w", dlqErr)
			}
			// DLQ success, considered handled
			return nil
		}
		// DLQ not configured, fallthrough to error
		return fmt.Errorf("retries exhausted and DLQ not configured")

	case RetryStrategyLogAndIgnore:
		h.logger.Error("message processing failed, skipping message",
			"topic", msg.Topic,
			"partition", msg.Partition,
			"offset", msg.Offset,
			"error", err,
		)
		return nil // Return nil so ConsumeClaim marks it and moves on

	default:
		h.logger.Error("Invalid retry strategy, treating as LogAndIgnore",
			"topic", msg.Topic,
			"partition", msg.Partition,
			"offset", msg.Offset,
			"error", err,
		)
	}
	return nil
}
