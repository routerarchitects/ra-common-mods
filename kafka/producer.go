package kafka

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/IBM/sarama"
)

// ErrProducerClosed is returned when publishing to a closed producer.
var ErrProducerClosed = errors.New("kafka: producer is closed")

type syncProducerClient interface {
	SendMessage(msg *sarama.ProducerMessage) (partition int32, offset int64, err error)
	Close() error
}

type consumerGroupClient interface {
	Consume(ctx context.Context, topics []string, handler sarama.ConsumerGroupHandler) error
	Errors() <-chan error
	Close() error
}

// producer implements the Producer interface using Sarama.
type producer struct {
	mu           sync.RWMutex
	closed       bool
	closeErr     error
	syncProducer syncProducerClient
	requiredAcks int16
	metrics      *Metrics
	tracing      *kafkaTracer
}

// NewProducer creates a new Kafka producer from independent BrokerConfig and ProducerConfig.
// The returned producer is safe for concurrent use across goroutines and topics.
// Close coordinates with active Publish calls and waits for already-admitted synchronous sends to complete.
func NewProducer(brokerCfg BrokerConfig, producerCfg ProducerConfig, opts ...ProducerOption) (Producer, error) {
	var options producerOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&options)
		}
	}

	normalizedBroker, err := normalizeAndValidateBrokerConfig(brokerCfg)
	if err != nil {
		return nil, fmt.Errorf("invalid broker config: %w", err)
	}
	normalizedProducer, err := normalizeAndValidateProducerConfig(producerCfg)
	if err != nil {
		return nil, fmt.Errorf("invalid producer config: %w", err)
	}

	saramaCfg, err := buildProducerSaramaConfig(normalizedBroker, normalizedProducer)
	if err != nil {
		return nil, err
	}

	factory := options.syncProducerFactory
	if factory == nil {
		factory = func(addrs []string, cfg *sarama.Config) (syncProducerClient, error) {
			return sarama.NewSyncProducer(addrs, cfg)
		}
	}

	sp, err := factory(normalizedBroker.Brokers, saramaCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create producer: %w", err)
	}

	return &producer{
		syncProducer: sp,
		requiredAcks: normalizedProducer.RequiredAcks,
		metrics:      options.metrics,
		tracing:      options.tracing,
	}, nil
}

// Publish sends a message to the specified topic.
func (p *producer) Publish(ctx context.Context, topic string, key, value []byte) error {
	return p.PublishWithHeaders(ctx, topic, key, value, nil)
}

// PublishWithHeaders sends a message with custom headers. Context cancellation
// can prevent admission. Once Sarama starts the synchronous send, its network
// timeouts and retry budget govern completion.
func (p *producer) PublishWithHeaders(ctx context.Context, topic string, key, value []byte, headers []RecordHeader) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateTopicName(topic); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		p.metrics.recordPublish(topic, 0, err)
		return err
	}

	p.mu.RLock()
	if p.closed {
		p.mu.RUnlock()
		p.metrics.recordPublish(topic, 0, ErrProducerClosed)
		return ErrProducerClosed
	}
	// Re-check context after admission and before calling Sarama synchronous send.
	if err := ctx.Err(); err != nil {
		p.mu.RUnlock()
		p.metrics.recordPublish(topic, 0, err)
		return err
	}

	sendHeaders := cloneHeaders(headers)
	var endSpan func(error)
	if p.tracing != nil {
		_, sendHeaders, endSpan = p.tracing.startPublishSpan(ctx, topic, sendHeaders)
	}

	msg := &sarama.ProducerMessage{
		Topic: topic,
		Value: sarama.ByteEncoder(value),
	}
	if key != nil {
		msg.Key = sarama.ByteEncoder(key)
	}
	if len(sendHeaders) > 0 {
		msg.Headers = make([]sarama.RecordHeader, len(sendHeaders))
		for i, h := range sendHeaders {
			msg.Headers[i] = sarama.RecordHeader{
				Key:   []byte(h.Key),
				Value: h.Value,
			}
		}
	}

	start := time.Now()
	_, _, err := p.syncProducer.SendMessage(msg)
	duration := time.Since(start)
	p.mu.RUnlock()

	if endSpan != nil {
		endSpan(err)
	}
	p.metrics.recordPublish(topic, duration, err)

	if err != nil {
		return fmt.Errorf("failed to publish message on topic %s: %w", topic, err)
	}
	return nil
}

// PublishJSON is a convenience method to publish JSON-encoded data.
func (p *producer) PublishJSON(ctx context.Context, topic string, key string, data interface{}) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateTopicName(topic); err != nil {
		return err
	}

	value, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("failed to marshal JSON: %w", err)
	}

	return p.Publish(ctx, topic, []byte(key), value)
}

// Close closes the producer after waiting for any active synchronous Publish calls to finish.
// Repeated and concurrent Close calls are safe and return the recorded close result.
func (p *producer) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return p.closeErr
	}
	p.closed = true
	if p.syncProducer != nil {
		p.closeErr = p.syncProducer.Close()
	}
	return p.closeErr
}
