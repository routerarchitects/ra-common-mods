package kafka

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

const (
	defaultClientID            = "ra-kafka-client"
	defaultNetworkTimeout      = 30 * time.Second
	defaultMaxMessageBytes     = 1000000
	defaultProducerTimeout     = 10 * time.Second
	defaultProducerCompression = "snappy"
	defaultProducerPartitioner = "hash"
	defaultProducerRetries     = 3
	defaultProducerBackoff     = 100 * time.Millisecond

	defaultInitialOffset       = "oldest"
	defaultBalanceStrategy     = "roundrobin"
	defaultIsolationLevel      = IsolationReadUncommitted
	defaultSessionTimeout      = 10 * time.Second
	defaultHeartbeatInterval   = 3 * time.Second
	defaultRebalanceTimeout    = 60 * time.Second
	defaultMaxProcessingTime   = 100 * time.Millisecond
	defaultCommitMode          = CommitPeriodic
	defaultCommitInterval      = 5 * time.Second
	defaultHandlerTimeout      = 30 * time.Second
	defaultShutdownTimeout     = 30 * time.Second
	defaultCloseTimeout        = 5 * time.Second
	defaultResetInvalidOffsets = true
)

// CommitMode defines how consumer offsets are committed to Kafka.
type CommitMode string

const (
	// CommitPeriodic enables Sarama background periodic offset commits.
	CommitPeriodic CommitMode = "periodic"

	// CommitSync commits marked offsets synchronously after each resolved message
	// and performs a best-effort commit on session cleanup.
	CommitSync CommitMode = "sync"
)

// IsolationLevel defines how a consumer reads records written by transactional producers.
type IsolationLevel string

const (
	// IsolationReadUncommitted returns all messages, including uncommitted and aborted transactional records.
	IsolationReadUncommitted IsolationLevel = "read_uncommitted"

	// IsolationReadCommitted hides records belonging to aborted transactions.
	IsolationReadCommitted IsolationLevel = "read_committed"
)

// BrokerConfig contains reusable Kafka cluster connectivity, authentication, and network settings.
type BrokerConfig struct {
	// Brokers is the list of Kafka broker addresses (host:port).
	Brokers []string `json:"brokers" yaml:"brokers" env:"KAFKA_BROKERS" envSeparator:","`

	// ClientID identifies the client in Kafka logs.
	ClientID string `json:"client_id" yaml:"client_id" env:"KAFKA_CLIENT_ID" envDefault:"ra-kafka-client"`

	// Auth contains SASL and TLS authentication configuration.
	Auth AuthConfig `json:"auth" yaml:"auth" envPrefix:"KAFKA_AUTH_"`

	// DialTimeout is the socket dial timeout for broker connections.
	DialTimeout time.Duration `json:"dial_timeout" yaml:"dial_timeout" env:"KAFKA_DIAL_TIMEOUT" envDefault:"30s"`

	// ReadTimeout is the socket read timeout for broker connections.
	ReadTimeout time.Duration `json:"read_timeout" yaml:"read_timeout" env:"KAFKA_READ_TIMEOUT" envDefault:"30s"`

	// WriteTimeout is the socket write timeout for broker connections.
	WriteTimeout time.Duration `json:"write_timeout" yaml:"write_timeout" env:"KAFKA_WRITE_TIMEOUT" envDefault:"30s"`
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

	// AllowInsecurePlaintext explicitly opts in to SASL PLAIN over non-TLS connections (development only).
	AllowInsecurePlaintext bool `json:"allow_insecure_plaintext" yaml:"allow_insecure_plaintext" env:"ALLOW_INSECURE_PLAINTEXT" envDefault:"false"`
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

// ConsumerConfig contains consumer-group settings.
type ConsumerConfig struct {
	// GroupID is the consumer group ID
	GroupID string `json:"group_id" yaml:"group_id" env:"GROUP_ID"`

	// InitialOffset determines where to start consuming when no valid committed offset exists.
	// Valid values: "oldest", "newest"
	InitialOffset string `json:"initial_offset" yaml:"initial_offset" env:"INITIAL_OFFSET" envDefault:"oldest"`

	// ResetInvalidOffsets automatically resets out-of-range offsets to InitialOffset when true.
	// When false, out-of-range offsets stop the consumer with a terminal error instead of resetting.
	ResetInvalidOffsets bool `json:"reset_invalid_offsets" yaml:"reset_invalid_offsets" env:"RESET_INVALID_OFFSETS" envDefault:"true"`

	// BalanceStrategy selects the partition assignment strategy ("roundrobin", "sticky", "range").
	BalanceStrategy string `json:"balance_strategy" yaml:"balance_strategy" env:"BALANCE_STRATEGY" envDefault:"roundrobin"`

	// IsolationLevel controls transactional read isolation ("read_uncommitted" or "read_committed").
	IsolationLevel IsolationLevel `json:"isolation_level" yaml:"isolation_level" env:"ISOLATION_LEVEL" envDefault:"read_uncommitted"`

	// SessionTimeout is the timeout for consumer group sessions
	SessionTimeout time.Duration `json:"session_timeout" yaml:"session_timeout" env:"SESSION_TIMEOUT" envDefault:"10s"`

	// HeartbeatInterval is the interval between heartbeats
	HeartbeatInterval time.Duration `json:"heartbeat_interval" yaml:"heartbeat_interval" env:"HEARTBEAT_INTERVAL" envDefault:"3s"`

	// RebalanceTimeout is the maximum time allowed for group members to rejoin during a rebalance.
	RebalanceTimeout time.Duration `json:"rebalance_timeout" yaml:"rebalance_timeout" env:"REBALANCE_TIMEOUT" envDefault:"60s"`

	// MaxProcessingTime is Sarama's message-channel delivery backpressure timeout, NOT a per-message handler deadline.
	MaxProcessingTime time.Duration `json:"max_processing_time" yaml:"max_processing_time" env:"MAX_PROCESSING_TIME" envDefault:"100ms"`

	// CommitMode selects offset commit behavior ("periodic" or "sync").
	CommitMode CommitMode `json:"commit_mode" yaml:"commit_mode" env:"COMMIT_MODE" envDefault:"periodic"`

	// CommitInterval controls periodic auto-commit frequency when CommitMode is "periodic".
	CommitInterval time.Duration `json:"commit_interval" yaml:"commit_interval" env:"COMMIT_INTERVAL" envDefault:"5s"`

	// HandlerTimeout is the default cooperative per-attempt timeout applied to message handlers.
	HandlerTimeout time.Duration `json:"handler_timeout" yaml:"handler_timeout" env:"HANDLER_TIMEOUT" envDefault:"30s"`

	// ShutdownTimeout is the graceful handler drain budget allowed for admitted in-flight messages on shutdown.
	ShutdownTimeout time.Duration `json:"shutdown_timeout" yaml:"shutdown_timeout" env:"SHUTDOWN_TIMEOUT" envDefault:"30s"`

	// CloseTimeout limits how long to wait before aborting pending consumer broker I/O
	// once the drain completes or expires. Total shutdown can take longer when a
	// handler or a synchronous DLQ producer is still blocked.
	CloseTimeout time.Duration `json:"close_timeout" yaml:"close_timeout" env:"CLOSE_TIMEOUT" envDefault:"5s"`
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

	// Partitioner selects key-to-partition routing: "hash" (default Sarama FNV-1a),
	// "crc32" (librdkafka consistent_random compatible), "murmur2" (Kafka Java client compatible),
	// "roundrobin", or "random".
	Partitioner string `json:"partitioner" yaml:"partitioner" env:"PARTITIONER" envDefault:"hash"`

	// Idempotent enables idempotent producer (forces Net.MaxOpenRequests=1 for in-flight ordering).
	// When false, retries with multiple open requests can reorder messages within a partition.
	Idempotent bool `json:"idempotent" yaml:"idempotent" env:"IDEMPOTENT" envDefault:"true"`

	// MaxRetries is the maximum number of retries for failed produce requests
	MaxRetries int `json:"max_retries" yaml:"max_retries" env:"MAX_RETRIES" envDefault:"3"`

	// RetryBackoff is the backoff duration between retries
	RetryBackoff time.Duration `json:"retry_backoff" yaml:"retry_backoff" env:"RETRY_BACKOFF" envDefault:"100ms"`
}

// DefaultBrokerConfig returns a BrokerConfig initialized with documented defaults.
// Brokers is left empty and must be populated by the caller.
func DefaultBrokerConfig() BrokerConfig {
	return BrokerConfig{
		ClientID: defaultClientID,
		Auth: AuthConfig{
			SASL: SASLConfig{
				Mechanism: "PLAIN",
			},
		},
		DialTimeout:  defaultNetworkTimeout,
		ReadTimeout:  defaultNetworkTimeout,
		WriteTimeout: defaultNetworkTimeout,
	}
}

// DefaultProducerConfig returns a ProducerConfig initialized with documented defaults.
func DefaultProducerConfig() ProducerConfig {
	return ProducerConfig{
		MaxMessageBytes: defaultMaxMessageBytes,
		RequiredAcks:    -1,
		Timeout:         defaultProducerTimeout,
		Compression:     defaultProducerCompression,
		Partitioner:     defaultProducerPartitioner,
		Idempotent:      true,
		MaxRetries:      defaultProducerRetries,
		RetryBackoff:    defaultProducerBackoff,
	}
}

// DefaultConsumerConfig returns a ConsumerConfig initialized with documented defaults.
// GroupID is left empty and must be populated by the caller.
func DefaultConsumerConfig() ConsumerConfig {
	return ConsumerConfig{
		InitialOffset:       defaultInitialOffset,
		ResetInvalidOffsets: defaultResetInvalidOffsets,
		BalanceStrategy:     defaultBalanceStrategy,
		IsolationLevel:      defaultIsolationLevel,
		SessionTimeout:      defaultSessionTimeout,
		HeartbeatInterval:   defaultHeartbeatInterval,
		RebalanceTimeout:    defaultRebalanceTimeout,
		MaxProcessingTime:   defaultMaxProcessingTime,
		CommitMode:          defaultCommitMode,
		CommitInterval:      defaultCommitInterval,
		HandlerTimeout:      defaultHandlerTimeout,
		ShutdownTimeout:     defaultShutdownTimeout,
		CloseTimeout:        defaultCloseTimeout,
	}
}

func normalizeAndValidateBrokerConfig(cfg BrokerConfig) (BrokerConfig, error) {
	if len(cfg.Brokers) == 0 {
		return BrokerConfig{}, fmt.Errorf("no brokers specified")
	}

	brokers := make([]string, len(cfg.Brokers))
	for i, raw := range cfg.Brokers {
		b := strings.TrimSpace(raw)
		if b == "" {
			return BrokerConfig{}, fmt.Errorf("broker address at index %d cannot be empty", i)
		}
		if err := validateBrokerAddr(b); err != nil {
			return BrokerConfig{}, fmt.Errorf("invalid broker address %q: %w", raw, err)
		}
		brokers[i] = b
	}
	cfg.Brokers = brokers

	if strings.TrimSpace(cfg.ClientID) == "" {
		return BrokerConfig{}, fmt.Errorf("client ID is required")
	}

	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = defaultNetworkTimeout
	}
	if cfg.ReadTimeout == 0 {
		cfg.ReadTimeout = defaultNetworkTimeout
	}
	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = defaultNetworkTimeout
	}
	if cfg.DialTimeout < 0 {
		return BrokerConfig{}, fmt.Errorf("dial timeout must be greater than zero")
	}
	if cfg.ReadTimeout < 0 {
		return BrokerConfig{}, fmt.Errorf("read timeout must be greater than zero")
	}
	if cfg.WriteTimeout < 0 {
		return BrokerConfig{}, fmt.Errorf("write timeout must be greater than zero")
	}

	if cfg.Auth.SASL.Enabled {
		if strings.TrimSpace(cfg.Auth.SASL.Username) == "" {
			return BrokerConfig{}, fmt.Errorf("SASL username is required when SASL is enabled")
		}
		if cfg.Auth.SASL.Password == "" {
			return BrokerConfig{}, fmt.Errorf("SASL password is required when SASL is enabled")
		}
		switch cfg.Auth.SASL.Mechanism {
		case "PLAIN", "SCRAM-SHA-256", "SCRAM-SHA-512":
		default:
			return BrokerConfig{}, fmt.Errorf("unsupported SASL mechanism: %s", cfg.Auth.SASL.Mechanism)
		}
		if cfg.Auth.SASL.Mechanism == "PLAIN" && !cfg.Auth.TLS.Enabled && !cfg.Auth.SASL.AllowInsecurePlaintext {
			return BrokerConfig{}, fmt.Errorf("SASL PLAIN requires TLS to be enabled unless allow_insecure_plaintext is explicitly set")
		}
	} else {
		if strings.TrimSpace(cfg.Auth.SASL.Username) != "" || cfg.Auth.SASL.Password != "" ||
			cfg.Auth.SASL.AllowInsecurePlaintext ||
			(cfg.Auth.SASL.Mechanism != "" && cfg.Auth.SASL.Mechanism != "PLAIN") {
			return BrokerConfig{}, fmt.Errorf("SASL credentials or options cannot be configured when SASL is disabled")
		}
	}

	if cfg.Auth.TLS.Enabled {
		hasCert := strings.TrimSpace(cfg.Auth.TLS.CertFile) != ""
		hasKey := strings.TrimSpace(cfg.Auth.TLS.KeyFile) != ""
		if hasCert != hasKey {
			return BrokerConfig{}, fmt.Errorf("both TLS cert_file and key_file must be specified together")
		}
	} else {
		if strings.TrimSpace(cfg.Auth.TLS.CertFile) != "" ||
			strings.TrimSpace(cfg.Auth.TLS.KeyFile) != "" ||
			strings.TrimSpace(cfg.Auth.TLS.CAFile) != "" ||
			cfg.Auth.TLS.InsecureSkipVerify {
			return BrokerConfig{}, fmt.Errorf("TLS certificates or security options cannot be configured when TLS is disabled")
		}
	}

	return cfg, nil
}

const maxTopicNameLen = 249

func validateTopicName(topic string) error {
	if topic == "" {
		return fmt.Errorf("topic name is required")
	}
	if len(topic) > maxTopicNameLen {
		return fmt.Errorf("topic name %q exceeds maximum length of %d characters", topic, maxTopicNameLen)
	}
	if topic == "." || topic == ".." {
		return fmt.Errorf("topic name %q is reserved and invalid", topic)
	}
	for i := 0; i < len(topic); i++ {
		c := topic[i]
		valid := (c >= 'a' && c <= 'z') ||
			(c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') ||
			c == '.' || c == '_' || c == '-'
		if !valid {
			return fmt.Errorf("topic name %q contains invalid character %q (permitted: ASCII letters, digits, '.', '_', '-')", topic, c)
		}
	}
	return nil
}

func validateBrokerAddr(addr string) error {
	if strings.Contains(addr, "://") {
		return fmt.Errorf("URL scheme is not allowed in broker address")
	}
	if strings.ContainsAny(addr, " \t\r\n") {
		return fmt.Errorf("whitespace is not allowed in broker address")
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if strings.TrimSpace(host) == "" {
		return fmt.Errorf("host is empty")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("invalid port %q", portStr)
	}
	return nil
}

func normalizeAndValidateProducerConfig(cfg ProducerConfig) (ProducerConfig, error) {
	if cfg.Compression == "" {
		cfg.Compression = defaultProducerCompression
	}
	if cfg.Partitioner == "" {
		cfg.Partitioner = defaultProducerPartitioner
	}

	if cfg.MaxMessageBytes <= 0 {
		return ProducerConfig{}, fmt.Errorf("producer max_message_bytes must be greater than zero")
	}
	if cfg.Timeout <= 0 {
		return ProducerConfig{}, fmt.Errorf("producer timeout must be greater than zero")
	}
	switch cfg.RequiredAcks {
	case -1, 0, 1:
	default:
		return ProducerConfig{}, fmt.Errorf("producer required_acks must be -1, 0, or 1 (got %d)", cfg.RequiredAcks)
	}
	switch cfg.Compression {
	case "none", "gzip", "snappy", "lz4", "zstd":
	default:
		return ProducerConfig{}, fmt.Errorf("unsupported producer compression %q", cfg.Compression)
	}
	switch cfg.Partitioner {
	case "hash", "crc32", "murmur2", "roundrobin", "random":
	default:
		return ProducerConfig{}, fmt.Errorf("unsupported producer partitioner %q: must be \"hash\", \"crc32\", \"murmur2\", \"roundrobin\", or \"random\"", cfg.Partitioner)
	}
	if cfg.MaxRetries < 0 {
		return ProducerConfig{}, fmt.Errorf("producer max_retries cannot be negative")
	}
	if cfg.RetryBackoff < 0 {
		return ProducerConfig{}, fmt.Errorf("producer retry_backoff cannot be negative")
	}
	if cfg.Idempotent {
		if cfg.RequiredAcks != -1 {
			return ProducerConfig{}, fmt.Errorf("idempotent producer requires required_acks=-1 (got %d)", cfg.RequiredAcks)
		}
		if cfg.MaxRetries < 1 {
			return ProducerConfig{}, fmt.Errorf("idempotent producer requires max_retries >= 1")
		}
	}
	return cfg, nil
}

func normalizeAndValidateConsumerConfig(cfg ConsumerConfig) (ConsumerConfig, error) {
	if strings.TrimSpace(cfg.GroupID) == "" {
		return ConsumerConfig{}, fmt.Errorf("consumer group ID is required")
	}
	switch cfg.InitialOffset {
	case "oldest", "newest":
	default:
		return ConsumerConfig{}, fmt.Errorf("invalid initial_offset %q: must be \"oldest\" or \"newest\"", cfg.InitialOffset)
	}
	switch cfg.BalanceStrategy {
	case "roundrobin", "sticky", "range":
	default:
		return ConsumerConfig{}, fmt.Errorf("unsupported balance_strategy %q: must be \"roundrobin\", \"sticky\", or \"range\"", cfg.BalanceStrategy)
	}
	if cfg.IsolationLevel == "" {
		cfg.IsolationLevel = defaultIsolationLevel
	}
	switch cfg.IsolationLevel {
	case IsolationReadUncommitted, IsolationReadCommitted:
	default:
		return ConsumerConfig{}, fmt.Errorf("unsupported isolation_level %q: must be \"read_uncommitted\" or \"read_committed\"", cfg.IsolationLevel)
	}
	if cfg.SessionTimeout <= 0 {
		return ConsumerConfig{}, fmt.Errorf("consumer session_timeout must be greater than zero")
	}
	if cfg.HeartbeatInterval <= 0 {
		return ConsumerConfig{}, fmt.Errorf("consumer heartbeat_interval must be greater than zero")
	}
	if cfg.HeartbeatInterval >= cfg.SessionTimeout {
		return ConsumerConfig{}, fmt.Errorf("consumer heartbeat_interval (%s) must be less than session_timeout (%s)", cfg.HeartbeatInterval, cfg.SessionTimeout)
	}
	if cfg.RebalanceTimeout <= 0 {
		return ConsumerConfig{}, fmt.Errorf("consumer rebalance_timeout must be greater than zero")
	}
	if cfg.MaxProcessingTime <= 0 {
		return ConsumerConfig{}, fmt.Errorf("consumer max_processing_time must be greater than zero")
	}
	switch cfg.CommitMode {
	case CommitPeriodic:
		if cfg.CommitInterval <= 0 {
			return ConsumerConfig{}, fmt.Errorf("consumer commit_interval must be greater than zero in periodic commit mode")
		}
	case CommitSync:
		if cfg.CommitInterval < 0 {
			return ConsumerConfig{}, fmt.Errorf("consumer commit_interval cannot be negative")
		}
	default:
		return ConsumerConfig{}, fmt.Errorf("unsupported commit_mode %q: must be \"periodic\" or \"sync\"", cfg.CommitMode)
	}
	if cfg.HandlerTimeout <= 0 {
		return ConsumerConfig{}, fmt.Errorf("consumer handler_timeout must be greater than zero")
	}
	if cfg.ShutdownTimeout <= 0 {
		return ConsumerConfig{}, fmt.Errorf("consumer shutdown_timeout must be greater than zero")
	}
	if cfg.CloseTimeout == 0 {
		cfg.CloseTimeout = defaultCloseTimeout
	}
	if cfg.CloseTimeout < 0 {
		return ConsumerConfig{}, fmt.Errorf("consumer close_timeout must be greater than zero")
	}
	return cfg, nil
}
