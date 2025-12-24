package kafka

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"os"
	"sync"
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
	}

	// Apply interceptors to handler
	finalHandler := handler
	for i := len(opts.Interceptors) - 1; i >= 0; i-- {
		finalHandler = opts.Interceptors[i](finalHandler)
	}

	// Create consumer group
	client, err := sarama.NewConsumerGroup(c.brokers, c.groupID, c.saramaConfig)
	if err != nil {
		return fmt.Errorf("failed to create consumer group: %w", err)
	}
	c.client = client

	// Create consumer group handler
	consumerHandler := &consumerGroupHandler{
		handler:     finalHandler,
		opts:        opts,
		logger:      c.logger,
		workerPool:  newWorkerPool(opts.Workers, opts.BufferSize),
		retryPolicy: opts.RetryPolicy,
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
	handler     Handler
	opts        *SubscribeOptions
	logger      *slog.Logger
	workerPool  *workerPool
	retryPolicy *RetryPolicy
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
func (h *consumerGroupHandler) ConsumeClaim(session sarama.ConsumerGroupSession, claim sarama.ConsumerGroupClaim) error {
	for {
		select {
		case msg := <-claim.Messages():
			if msg == nil {
				return nil
			}

			// Convert sarama message to our Message type
			kafkaMsg := h.convertMessage(msg, session.Context())

			// Submit to worker pool
			h.workerPool.Submit(session.Context(), kafkaMsg, func(ctx context.Context, m *Message) error {
				// Process with retries
				return h.processWithRetry(ctx, m, h.handler)
			}, func(processedMsg *Message, err error) {
				if err != nil {
					h.logger.Error("message processing failed",
						"topic", processedMsg.Topic,
						"partition", processedMsg.Partition,
						"offset", processedMsg.Offset,
						"error", err,
					)
				}
				// Mark offset regardless of success/failure (using original sarama message)
				session.MarkMessage(msg, "")
			})

		case <-session.Context().Done():
			return nil
		}
	}
}

// convertMessage converts a Sarama message to our Message type
func (h *consumerGroupHandler) convertMessage(msg *sarama.ConsumerMessage, ctx context.Context) *Message {
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
		Context:   ctx,
	}
}

// processWithRetry processes a message with retry logic
func (h *consumerGroupHandler) processWithRetry(ctx context.Context, msg *Message, handler Handler) error {
	if h.retryPolicy == nil {
		return handler(ctx, msg)
	}

	var lastErr error
	delay := h.retryPolicy.InitialDelay

	for attempt := 0; attempt <= h.retryPolicy.MaxRetries; attempt++ {
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

		err := handler(ctx, msg)
		if err == nil {
			return nil
		}

		lastErr = err
	}

	// All retries exhausted, send to DLQ if configured
	if h.retryPolicy.DLQTopic != "" && h.retryPolicy.DLQProducer != nil {
		h.logger.Error("sending message to DLQ after retry exhaustion",
			"topic", msg.Topic,
			"partition", msg.Partition,
			"offset", msg.Offset,
			"error", lastErr,
		)

		dlqErr := h.retryPolicy.DLQProducer.PublishWithHeaders(
			ctx,
			h.retryPolicy.DLQTopic,
			msg.Key,
			msg.Value,
			append(msg.Headers, RecordHeader{
				Key:   "x-original-topic",
				Value: []byte(msg.Topic),
			}),
		)
		if dlqErr != nil {
			h.logger.Error("failed to send to DLQ", "error", dlqErr)
		}
	}

	return lastErr
}

// workerPool manages a pool of workers for processing messages
type workerPool struct {
	workers int
	jobs    chan job
	wg      sync.WaitGroup
}

type job struct {
	ctx      context.Context
	msg      *Message
	handler  func(context.Context, *Message) error
	callback func(*Message, error)
}

func newWorkerPool(workers, bufferSize int) *workerPool {
	if workers <= 0 {
		workers = 1
	}
	if bufferSize <= 0 {
		bufferSize = 100
	}

	wp := &workerPool{
		workers: workers,
		jobs:    make(chan job, bufferSize),
	}

	// Start workers
	for i := 0; i < workers; i++ {
		wp.wg.Add(1)
		go wp.worker()
	}

	return wp
}

func (wp *workerPool) worker() {
	defer wp.wg.Done()

	for job := range wp.jobs {
		err := job.handler(job.ctx, job.msg)
		if job.callback != nil {
			job.callback(job.msg, err)
		}
	}
}

func (wp *workerPool) Submit(ctx context.Context, msg *Message, handler func(context.Context, *Message) error, callback func(*Message, error)) {
	select {
	case wp.jobs <- job{ctx: ctx, msg: msg, handler: handler, callback: callback}:
	case <-ctx.Done():
		// Context cancelled, don't submit
	}
}

func (wp *workerPool) Close() {
	close(wp.jobs)
	wp.wg.Wait()
}
