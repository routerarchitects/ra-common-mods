package kafka

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/IBM/sarama"
)

type saramaConfigAlias = sarama.Config

func buildBaseSaramaConfig(cfg BrokerConfig) (*sarama.Config, error) {
	sc := sarama.NewConfig()
	sc.ClientID = cfg.ClientID
	sc.Net.DialTimeout = cfg.DialTimeout
	sc.Net.ReadTimeout = cfg.ReadTimeout
	sc.Net.WriteTimeout = cfg.WriteTimeout
	sc.Metadata.AllowAutoTopicCreation = false

	if err := configureSASL(sc, cfg.Auth.SASL); err != nil {
		return nil, err
	}

	if cfg.Auth.TLS.Enabled {
		tlsCfg, err := buildTLSConfig(cfg.Auth.TLS)
		if err != nil {
			return nil, err
		}
		sc.Net.TLS.Enable = true
		sc.Net.TLS.Config = tlsCfg
	}

	return sc, nil
}

func configureSASL(sc *sarama.Config, cfg SASLConfig) error {
	if !cfg.Enabled {
		return nil
	}

	sc.Net.SASL.Enable = true
	sc.Net.SASL.User = cfg.Username
	sc.Net.SASL.Password = cfg.Password

	switch cfg.Mechanism {
	case "PLAIN":
		sc.Net.SASL.Mechanism = sarama.SASLTypePlaintext
	case "SCRAM-SHA-256":
		sc.Net.SASL.Mechanism = sarama.SASLTypeSCRAMSHA256
		sc.Net.SASL.SCRAMClientGeneratorFunc = func() sarama.SCRAMClient {
			return &XDGSCRAMClient{HashGeneratorFcn: SHA256}
		}
	case "SCRAM-SHA-512":
		sc.Net.SASL.Mechanism = sarama.SASLTypeSCRAMSHA512
		sc.Net.SASL.SCRAMClientGeneratorFunc = func() sarama.SCRAMClient {
			return &XDGSCRAMClient{HashGeneratorFcn: SHA512}
		}
	default:
		return fmt.Errorf("unsupported SASL mechanism: %s", cfg.Mechanism)
	}

	return nil
}

func buildTLSConfig(cfg TLSConfig) (*tls.Config, error) {
	tlsConfig := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: cfg.InsecureSkipVerify,
	}

	hasCert := strings.TrimSpace(cfg.CertFile) != ""
	hasKey := strings.TrimSpace(cfg.KeyFile) != ""
	if hasCert != hasKey {
		return nil, fmt.Errorf("both TLS cert_file and key_file must be specified together")
	}
	if hasCert && hasKey {
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load client cert: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}

	if strings.TrimSpace(cfg.CAFile) != "" {
		caCert, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read CA cert: %w", err)
		}
		caCertPool := x509.NewCertPool()
		if !caCertPool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("failed to parse CA cert PEM from %s: no valid certificates found", cfg.CAFile)
		}
		tlsConfig.RootCAs = caCertPool
	}

	return tlsConfig, nil
}

func buildProducerSaramaConfig(broker BrokerConfig, producer ProducerConfig) (*sarama.Config, error) {
	sc, err := buildBaseSaramaConfig(broker)
	if err != nil {
		return nil, err
	}

	sc.Producer.Return.Successes = true
	sc.Producer.Return.Errors = true
	sc.Producer.RequiredAcks = sarama.RequiredAcks(producer.RequiredAcks)
	sc.Producer.MaxMessageBytes = producer.MaxMessageBytes
	sc.Producer.Timeout = producer.Timeout
	sc.Producer.Retry.Max = producer.MaxRetries
	sc.Producer.Retry.Backoff = producer.RetryBackoff
	sc.Producer.Idempotent = producer.Idempotent

	switch producer.Compression {
	case "none":
		sc.Producer.Compression = sarama.CompressionNone
	case "gzip":
		sc.Producer.Compression = sarama.CompressionGZIP
	case "snappy":
		sc.Producer.Compression = sarama.CompressionSnappy
	case "lz4":
		sc.Producer.Compression = sarama.CompressionLZ4
	case "zstd":
		sc.Producer.Compression = sarama.CompressionZSTD
	default:
		return nil, fmt.Errorf("unsupported producer compression: %s", producer.Compression)
	}

	if producer.Idempotent {
		sc.Net.MaxOpenRequests = 1
	}

	switch producer.Partitioner {
	case "", "hash":
		sc.Producer.Partitioner = sarama.NewHashPartitioner
	case "crc32":
		sc.Producer.Partitioner = sarama.NewConsistentCRCHashPartitioner
	case "murmur2":
		sc.Producer.Partitioner = sarama.NewMurmur2Partitioner
	case "roundrobin":
		sc.Producer.Partitioner = sarama.NewRoundRobinPartitioner
	case "random":
		sc.Producer.Partitioner = sarama.NewRandomPartitioner
	default:
		return nil, fmt.Errorf("unsupported producer partitioner: %s", producer.Partitioner)
	}

	if err := sc.Validate(); err != nil {
		return nil, fmt.Errorf("invalid sarama producer config: %w", err)
	}

	return sc, nil
}

func buildConsumerSaramaConfig(broker BrokerConfig, consumer ConsumerConfig) (*sarama.Config, error) {
	sc, err := buildBaseSaramaConfig(broker)
	if err != nil {
		return nil, err
	}

	var strategy sarama.BalanceStrategy
	switch consumer.BalanceStrategy {
	case "roundrobin":
		strategy = sarama.NewBalanceStrategyRoundRobin()
	case "sticky":
		strategy = sarama.NewBalanceStrategySticky()
	case "range":
		strategy = sarama.NewBalanceStrategyRange()
	default:
		return nil, fmt.Errorf("unsupported balance strategy: %s", consumer.BalanceStrategy)
	}
	sc.Consumer.Group.Rebalance.GroupStrategies = []sarama.BalanceStrategy{strategy}

	switch consumer.InitialOffset {
	case "oldest":
		sc.Consumer.Offsets.Initial = sarama.OffsetOldest
	case "newest":
		sc.Consumer.Offsets.Initial = sarama.OffsetNewest
	default:
		return nil, fmt.Errorf("invalid initial offset: %s", consumer.InitialOffset)
	}

	sc.Consumer.Group.Session.Timeout = consumer.SessionTimeout
	sc.Consumer.Group.Heartbeat.Interval = consumer.HeartbeatInterval
	sc.Consumer.Group.Rebalance.Timeout = consumer.RebalanceTimeout
	sc.Consumer.Group.ResetInvalidOffsets = consumer.ResetInvalidOffsets
	switch consumer.IsolationLevel {
	case "", IsolationReadUncommitted:
		sc.Consumer.IsolationLevel = sarama.ReadUncommitted
	case IsolationReadCommitted:
		sc.Consumer.IsolationLevel = sarama.ReadCommitted
	default:
		return nil, fmt.Errorf("unsupported isolation level: %s", consumer.IsolationLevel)
	}
	sc.Consumer.MaxProcessingTime = consumer.MaxProcessingTime
	sc.Consumer.Return.Errors = true

	// Bound metadata and shutdown-time offset commit retries so unresponsive brokers
	// cannot multiply socket timeouts across excessive retry loops.
	metadataTimeout := broker.ReadTimeout * 2
	if metadataTimeout < 2*time.Second {
		metadataTimeout = 2 * time.Second
	}
	if metadataTimeout > 15*time.Second {
		metadataTimeout = 15 * time.Second
	}
	sc.Metadata.Timeout = metadataTimeout
	sc.Metadata.Retry.Max = 2
	sc.Consumer.Offsets.Retry.Max = 1

	switch consumer.CommitMode {
	case CommitPeriodic:
		sc.Consumer.Offsets.AutoCommit.Enable = true
		sc.Consumer.Offsets.AutoCommit.Interval = consumer.CommitInterval
	case CommitSync:
		sc.Consumer.Offsets.AutoCommit.Enable = false
		if consumer.CommitInterval > 0 {
			sc.Consumer.Offsets.AutoCommit.Interval = consumer.CommitInterval
		} else {
			sc.Consumer.Offsets.AutoCommit.Interval = defaultCommitInterval
		}
	default:
		return nil, fmt.Errorf("unsupported commit mode: %s", consumer.CommitMode)
	}

	if err := sc.Validate(); err != nil {
		return nil, fmt.Errorf("invalid sarama consumer config: %w", err)
	}

	return sc, nil
}
