package kafka

import (
	"time"
)

// Config is the top-level configuration for Kafka clients.
type Config struct {
	// Brokers is the list of Kafka broker addresses
	Brokers []string `json:"brokers" yaml:"brokers"`

	// ClientID identifies the client in Kafka logs
	ClientID string `json:"client_id" yaml:"client_id"`

	// Auth contains authentication configuration
	Auth AuthConfig `json:"auth" yaml:"auth"`

	// Consumer specific configuration
	Consumer ConsumerConfig `json:"consumer" yaml:"consumer"`

	// Producer specific configuration
	Producer ProducerConfig `json:"producer" yaml:"producer"`
}

// AuthConfig contains authentication and security settings.
type AuthConfig struct {
	// SASL configuration
	SASL SASLConfig `json:"sasl" yaml:"sasl"`

	// TLS configuration
	TLS TLSConfig `json:"tls" yaml:"tls"`
}

// SASLConfig contains SASL authentication settings.
type SASLConfig struct {
	// Enabled determines if SASL is enabled
	Enabled bool `json:"enabled" yaml:"enabled"`

	// Mechanism is the SASL mechanism (PLAIN, SCRAM-SHA-256, SCRAM-SHA-512)
	Mechanism string `json:"mechanism" yaml:"mechanism"`

	// Username for SASL authentication
	Username string `json:"username" yaml:"username"`

	// Password for SASL authentication
	Password string `json:"password" yaml:"password"`
}

// TLSConfig contains TLS settings.
type TLSConfig struct {
	// Enabled determines if TLS is enabled
	Enabled bool `json:"enabled" yaml:"enabled"`

	// InsecureSkipVerify skips certificate verification (for testing only)
	InsecureSkipVerify bool `json:"insecure_skip_verify" yaml:"insecure_skip_verify"`

	// CertFile is the path to client certificate
	CertFile string `json:"cert_file" yaml:"cert_file"`

	// KeyFile is the path to client key
	KeyFile string `json:"key_file" yaml:"key_file"`

	// CAFile is the path to CA certificate
	CAFile string `json:"ca_file" yaml:"ca_file"`
}

// ConsumerConfig contains consumer-specific settings.
type ConsumerConfig struct {
	// GroupID is the consumer group ID
	GroupID string `json:"group_id" yaml:"group_id"`

	// Topics is the list of topics to subscribe to
	Topics []string `json:"topics" yaml:"topics"`

	// InitialOffset determines where to start consuming (oldest/newest)
	// Valid values: "oldest", "newest"
	InitialOffset string `json:"initial_offset" yaml:"initial_offset"`

	// SessionTimeout is the timeout for consumer group sessions
	SessionTimeout time.Duration `json:"session_timeout" yaml:"session_timeout"`

	// HeartbeatInterval is the interval between heartbeats
	HeartbeatInterval time.Duration `json:"heartbeat_interval" yaml:"heartbeat_interval"`

	// MaxProcessingTime is the maximum time for processing a batch
	MaxProcessingTime time.Duration `json:"max_processing_time" yaml:"max_processing_time"`

	// CommitInterval is how often to auto-commit offsets (0 = manual only)
	CommitInterval time.Duration `json:"commit_interval" yaml:"commit_interval"`
}

// ProducerConfig contains producer-specific settings.
type ProducerConfig struct {
	// MaxMessageBytes is the maximum message size
	MaxMessageBytes int `json:"max_message_bytes" yaml:"max_message_bytes"`

	// RequiredAcks determines the number of required acknowledgements
	// 0 = no response, 1 = leader only, -1 = all in-sync replicas
	RequiredAcks int16 `json:"required_acks" yaml:"required_acks"`

	// Timeout is the timeout for produce requests
	Timeout time.Duration `json:"timeout" yaml:"timeout"`

	// Compression is the compression codec (none, gzip, snappy, lz4, zstd)
	Compression string `json:"compression" yaml:"compression"`

	// Idempotent enables idempotent producer
	Idempotent bool `json:"idempotent" yaml:"idempotent"`

	// MaxRetries is the maximum number of retries for failed produce requests
	MaxRetries int `json:"max_retries" yaml:"max_retries"`

	// RetryBackoff is the backoff duration between retries
	RetryBackoff time.Duration `json:"retry_backoff" yaml:"retry_backoff"`
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		ClientID: "ra-kafka-client",
		Consumer: ConsumerConfig{
			InitialOffset:     "newest",
			SessionTimeout:    10 * time.Second,
			HeartbeatInterval: 3 * time.Second,
			MaxProcessingTime: 30 * time.Second,
			CommitInterval:    5 * time.Second,
		},
		Producer: ProducerConfig{
			MaxMessageBytes: 1000000, // 1MB
			RequiredAcks:    -1,      // Wait for all in-sync replicas
			Timeout:         10 * time.Second,
			Compression:     "snappy",
			Idempotent:      true,
			MaxRetries:      3,
			RetryBackoff:    100 * time.Millisecond,
		},
	}
}
