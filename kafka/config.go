package kafka

import (
	"time"

	"github.com/caarlos0/env/v11"
)

// Config is the top-level configuration for Kafka clients.
// It is intended to be populated via github.com/caarlos0/env parsing.
type Config struct {
	// Brokers is the list of Kafka broker addresses
	Brokers []string `json:"brokers" yaml:"brokers" env:"KAFKA_BROKERS" envSeparator:","`

	// ClientID identifies the client in Kafka logs
	ClientID string `json:"client_id" yaml:"client_id" env:"KAFKA_CLIENT_ID" envDefault:"ra-kafka-client"`

	// Auth contains authentication configuration
	Auth AuthConfig `json:"auth" yaml:"auth" envPrefix:"KAFKA_AUTH_"`

	// Consumer specific configuration
	Consumer ConsumerConfig `json:"consumer" yaml:"consumer" envPrefix:"KAFKA_CONSUMER_"`

	// Producer specific configuration
	Producer ProducerConfig `json:"producer" yaml:"producer" envPrefix:"KAFKA_PRODUCER_"`
}

// AuthConfig contains authentication and security settings.
type AuthConfig struct {
	// SASL configuration
	SASL SASLConfig `json:"sasl" yaml:"sasl" envPrefix:"SASL_"`

	// TLS configuration
	TLS TLSConfig `json:"tls" yaml:"tls" envPrefix:"TLS_"`
}

// SASLConfig contains SASL authentication settings.
type SASLConfig struct {
	// Enabled determines if SASL is enabled
	Enabled bool `json:"enabled" yaml:"enabled" env:"ENABLED" envDefault:"false"`

	// Mechanism is the SASL mechanism (PLAIN, SCRAM-SHA-256, SCRAM-SHA-512)
	Mechanism string `json:"mechanism" yaml:"mechanism" env:"MECHANISM" envDefault:"PLAIN"`

	// Username for SASL authentication
	Username string `json:"username" yaml:"username" env:"USERNAME"`

	// Password for SASL authentication
	Password string `json:"password" yaml:"password" env:"PASSWORD"`
}

// TLSConfig contains TLS settings.
type TLSConfig struct {
	// Enabled determines if TLS is enabled
	Enabled bool `json:"enabled" yaml:"enabled" env:"ENABLED" envDefault:"false"`

	// InsecureSkipVerify skips certificate verification (for testing only)
	InsecureSkipVerify bool `json:"insecure_skip_verify" yaml:"insecure_skip_verify" env:"INSECURE_SKIP_VERIFY" envDefault:"false"`

	// CertFile is the path to client certificate
	CertFile string `json:"cert_file" yaml:"cert_file" env:"CERT_FILE"`

	// KeyFile is the path to client key
	KeyFile string `json:"key_file" yaml:"key_file" env:"KEY_FILE"`

	// CAFile is the path to CA certificate
	CAFile string `json:"ca_file" yaml:"ca_file" env:"CA_FILE"`
}

// ConsumerConfig contains consumer-specific settings.
type ConsumerConfig struct {
	// GroupID is the consumer group ID
	GroupID string `json:"group_id" yaml:"group_id" env:"GROUP_ID"`

	// Topics is the list of topics to subscribe to
	Topics []string `json:"topics" yaml:"topics" env:"TOPICS" envSeparator:","`

	// InitialOffset determines where to start consuming (oldest/newest)
	// Valid values: "oldest", "newest"
	InitialOffset string `json:"initial_offset" yaml:"initial_offset" env:"INITIAL_OFFSET" envDefault:"newest"`

	// SessionTimeout is the timeout for consumer group sessions
	SessionTimeout time.Duration `json:"session_timeout" yaml:"session_timeout" env:"SESSION_TIMEOUT" envDefault:"10s"`

	// HeartbeatInterval is the interval between heartbeats
	HeartbeatInterval time.Duration `json:"heartbeat_interval" yaml:"heartbeat_interval" env:"HEARTBEAT_INTERVAL" envDefault:"3s"`

	// MaxProcessingTime is the maximum time for processing a batch
	MaxProcessingTime time.Duration `json:"max_processing_time" yaml:"max_processing_time" env:"MAX_PROCESSING_TIME" envDefault:"30s"`

	// CommitInterval is how often to auto-commit offsets (0 = manual only)
	CommitInterval time.Duration `json:"commit_interval" yaml:"commit_interval" env:"COMMIT_INTERVAL" envDefault:"5s"`
}

// ProducerConfig contains producer-specific settings.
type ProducerConfig struct {
	// MaxMessageBytes is the maximum message size
	MaxMessageBytes int `json:"max_message_bytes" yaml:"max_message_bytes" env:"MAX_MESSAGE_BYTES" envDefault:"1000000"`

	// RequiredAcks determines the number of required acknowledgements
	// 0 = no response, 1 = leader only, -1 = all in-sync replicas
	RequiredAcks int16 `json:"required_acks" yaml:"required_acks" env:"REQUIRED_ACKS" envDefault:"-1"`

	// Timeout is the timeout for produce requests
	Timeout time.Duration `json:"timeout" yaml:"timeout" env:"TIMEOUT" envDefault:"10s"`

	// Compression is the compression codec (none, gzip, snappy, lz4, zstd)
	Compression string `json:"compression" yaml:"compression" env:"COMPRESSION" envDefault:"snappy"`

	// Idempotent enables idempotent producer
	Idempotent bool `json:"idempotent" yaml:"idempotent" env:"IDEMPOTENT" envDefault:"true"`

	// MaxRetries is the maximum number of retries for failed produce requests
	MaxRetries int `json:"max_retries" yaml:"max_retries" env:"MAX_RETRIES" envDefault:"3"`

	// RetryBackoff is the backoff duration between retries
	RetryBackoff time.Duration `json:"retry_backoff" yaml:"retry_backoff" env:"RETRY_BACKOFF" envDefault:"100ms"`
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

// LoadFromEnv loads the configuration from environment variables.
// It starts with DefaultConfig and overrides with values from the environment.
// Returns an error if required environment variables are missing or if parsing fails.
//
// Example usage:
//
//	cfg, err := kafka.LoadFromEnv()
//	if err != nil {
//	    log.Fatal(err)
//	}
func LoadFromEnv() (Config, error) {
	cfg := DefaultConfig()
	if err := env.Parse(&cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}
