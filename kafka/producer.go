package kafka

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"os"

	"github.com/IBM/sarama"
)

// producer implements the Producer interface using Sarama.
type producer struct {
	syncProducer sarama.SyncProducer
	config       Config
}

// NewProducer creates a new Kafka producer.
func NewProducer(cfg Config) (Producer, error) {
	if len(cfg.Brokers) == 0 {
		return nil, fmt.Errorf("no brokers specified")
	}

	saramaConfig := sarama.NewConfig()
	saramaConfig.Producer.Partitioner = sarama.NewHashPartitioner
	saramaConfig.ClientID = cfg.ClientID
	saramaConfig.Producer.Return.Successes = true
	saramaConfig.Producer.Return.Errors = true

	// Set required acks
	saramaConfig.Producer.RequiredAcks = sarama.RequiredAcks(cfg.Producer.RequiredAcks)

	// Set compression
	switch cfg.Producer.Compression {
	case "gzip":
		saramaConfig.Producer.Compression = sarama.CompressionGZIP
	case "snappy":
		saramaConfig.Producer.Compression = sarama.CompressionSnappy
	case "lz4":
		saramaConfig.Producer.Compression = sarama.CompressionLZ4
	case "zstd":
		saramaConfig.Producer.Compression = sarama.CompressionZSTD
	default:
		saramaConfig.Producer.Compression = sarama.CompressionNone
	}

	// Set idempotent
	saramaConfig.Producer.Idempotent = cfg.Producer.Idempotent
	if cfg.Producer.Idempotent {
		// Idempotent producer requires these settings
		saramaConfig.Producer.RequiredAcks = sarama.WaitForAll
		saramaConfig.Producer.Retry.Max = cfg.Producer.MaxRetries
		saramaConfig.Net.MaxOpenRequests = 1
	}

	// Set max message bytes
	if cfg.Producer.MaxMessageBytes > 0 {
		saramaConfig.Producer.MaxMessageBytes = cfg.Producer.MaxMessageBytes
	}

	// Set timeout
	if cfg.Producer.Timeout > 0 {
		saramaConfig.Producer.Timeout = cfg.Producer.Timeout
	}

	// Set retry backoff
	if cfg.Producer.RetryBackoff > 0 {
		saramaConfig.Producer.Retry.Backoff = cfg.Producer.RetryBackoff
	}

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

	// Create sync producer
	syncProducer, err := sarama.NewSyncProducer(cfg.Brokers, saramaConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create producer: %w", err)
	}

	return &producer{
		syncProducer: syncProducer,
		config:       cfg,
	}, nil
}

// Publish sends a message to the specified topic.
func (p *producer) Publish(ctx context.Context, topic string, key, value []byte) error {
	return p.PublishWithHeaders(ctx, topic, key, value, nil)
}

// PublishWithHeaders sends a message with custom headers.
func (p *producer) PublishWithHeaders(ctx context.Context, topic string, key, value []byte, headers []RecordHeader) error {
	msg := &sarama.ProducerMessage{
		Topic: topic,
		Key:   sarama.ByteEncoder(key),
		Value: sarama.ByteEncoder(value),
	}

	// Add headers
	if len(headers) > 0 {
		msg.Headers = make([]sarama.RecordHeader, len(headers))
		for i, h := range headers {
			msg.Headers[i] = sarama.RecordHeader{
				Key:   []byte(h.Key),
				Value: h.Value,
			}
		}
	}

	// Send message
	_, _, err := p.syncProducer.SendMessage(msg)
	if err != nil {
		return fmt.Errorf("failed to publish message: %w", err)
	}

	return nil
}

// PublishJSON is a convenience method to publish JSON-encoded data.
func (p *producer) PublishJSON(ctx context.Context, topic string, key string, data interface{}) error {
	value, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("failed to marshal JSON: %w", err)
	}

	return p.Publish(ctx, topic, []byte(key), value)
}

// Close closes the producer.
func (p *producer) Close() error {
	return p.syncProducer.Close()
}
