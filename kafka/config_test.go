package kafka

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/IBM/sarama"
)

func TestDefaultConstructorsIndependenceAndDefaults(t *testing.T) {
	b1 := DefaultBrokerConfig()
	b2 := DefaultBrokerConfig()
	if len(b1.Brokers) != 0 {
		t.Fatalf("expected empty default brokers, got %v", b1.Brokers)
	}
	if b1.ClientID != "ra-kafka-client" {
		t.Fatalf("unexpected default client ID: %s", b1.ClientID)
	}
	if b1.DialTimeout != 30*time.Second || b1.ReadTimeout != 30*time.Second || b1.WriteTimeout != 30*time.Second {
		t.Fatalf("unexpected default network timeouts: %+v", b1)
	}
	b1.Brokers = append(b1.Brokers, "localhost:9092")
	if len(b2.Brokers) != 0 {
		t.Fatalf("mutating b1.Brokers affected b2.Brokers")
	}

	p := DefaultProducerConfig()
	if p.MaxMessageBytes != 1000000 || p.RequiredAcks != -1 || p.Timeout != 10*time.Second ||
		p.Compression != "snappy" || p.Partitioner != "hash" || !p.Idempotent ||
		p.MaxRetries != 3 || p.RetryBackoff != 100*time.Millisecond {
		t.Fatalf("unexpected DefaultProducerConfig: %+v", p)
	}

	c := DefaultConsumerConfig()
	if c.GroupID != "" || c.InitialOffset != "oldest" || !c.ResetInvalidOffsets || c.BalanceStrategy != "roundrobin" ||
		c.SessionTimeout != 10*time.Second || c.HeartbeatInterval != 3*time.Second ||
		c.RebalanceTimeout != 60*time.Second || c.MaxProcessingTime != 100*time.Millisecond ||
		c.CommitMode != CommitPeriodic || c.CommitInterval != 5*time.Second ||
		c.HandlerTimeout != 30*time.Second || c.ShutdownTimeout != 30*time.Second ||
		c.CloseTimeout != 5*time.Second {
		t.Fatalf("unexpected DefaultConsumerConfig: %+v", c)
	}
}

func TestBrokerConfigValidationAndTimeoutMapping(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*BrokerConfig)
		wantErr string
	}{
		{
			name:    "valid ipv4 and ipv6",
			mutate:  func(b *BrokerConfig) { b.Brokers = []string{"127.0.0.1:9092", "[::1]:9093", "kafka.local:9094"} },
			wantErr: "",
		},
		{
			name:    "empty brokers",
			mutate:  func(b *BrokerConfig) { b.Brokers = nil },
			wantErr: "no brokers specified",
		},
		{
			name:    "blank broker entry",
			mutate:  func(b *BrokerConfig) { b.Brokers = []string{"localhost:9092", "  "} },
			wantErr: "cannot be empty",
		},
		{
			name:    "broker with URL scheme",
			mutate:  func(b *BrokerConfig) { b.Brokers = []string{"kafka://localhost:9092"} },
			wantErr: "URL scheme",
		},
		{
			name:    "broker missing port",
			mutate:  func(b *BrokerConfig) { b.Brokers = []string{"localhost"} },
			wantErr: "invalid broker address",
		},
		{
			name:    "broker invalid port",
			mutate:  func(b *BrokerConfig) { b.Brokers = []string{"localhost:70000"} },
			wantErr: "invalid port",
		},
		{
			name:    "blank client id",
			mutate:  func(b *BrokerConfig) { b.Brokers = []string{"localhost:9092"}; b.ClientID = "  " },
			wantErr: "client ID is required",
		},
		{
			name:    "negative dial timeout",
			mutate:  func(b *BrokerConfig) { b.Brokers = []string{"localhost:9092"}; b.DialTimeout = -time.Second },
			wantErr: "dial timeout",
		},
		{
			name:    "negative read timeout",
			mutate:  func(b *BrokerConfig) { b.Brokers = []string{"localhost:9092"}; b.ReadTimeout = -time.Second },
			wantErr: "read timeout",
		},
		{
			name:    "negative write timeout",
			mutate:  func(b *BrokerConfig) { b.Brokers = []string{"localhost:9092"}; b.WriteTimeout = -time.Second },
			wantErr: "write timeout",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := DefaultBrokerConfig()
			tc.mutate(&b)
			nb, err := normalizeAndValidateBrokerConfig(b)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Verify timeout mapping into both producer and consumer Sarama configs.
			nb.DialTimeout = 5 * time.Second
			nb.ReadTimeout = 7 * time.Second
			nb.WriteTimeout = 9 * time.Second

			ps, err := buildProducerSaramaConfig(nb, DefaultProducerConfig())
			if err != nil {
				t.Fatalf("buildProducerSaramaConfig: %v", err)
			}
			if ps.Net.DialTimeout != 5*time.Second || ps.Net.ReadTimeout != 7*time.Second || ps.Net.WriteTimeout != 9*time.Second {
				t.Fatalf("producer timeouts mismatch: %+v", ps.Net)
			}
			if ps.Metadata.AllowAutoTopicCreation {
				t.Fatalf("expected Metadata.AllowAutoTopicCreation=false")
			}

			cc := DefaultConsumerConfig()
			cc.GroupID = "grp"
			cs, err := buildConsumerSaramaConfig(nb, cc)
			if err != nil {
				t.Fatalf("buildConsumerSaramaConfig: %v", err)
			}
			if cs.Net.DialTimeout != 5*time.Second || cs.Net.ReadTimeout != 7*time.Second || cs.Net.WriteTimeout != 9*time.Second {
				t.Fatalf("consumer timeouts mismatch: %+v", cs.Net)
			}
			if cs.Metadata.AllowAutoTopicCreation {
				t.Fatalf("expected Metadata.AllowAutoTopicCreation=false")
			}
			if ps == cs {
				t.Fatalf("expected distinct Sarama config instances")
			}
		})
	}
}

func TestProducerConfigValidationAndMapping(t *testing.T) {
	broker := DefaultBrokerConfig()
	broker.Brokers = []string{"localhost:9092"}

	tests := []struct {
		name      string
		mutate    func(*ProducerConfig)
		wantErr   string
		checkSarm func(*testing.T, *sarama.Config)
	}{
		{
			name:   "default idempotent producer",
			mutate: func(p *ProducerConfig) {},
			checkSarm: func(t *testing.T, sc *sarama.Config) {
				if !sc.Producer.Idempotent || sc.Producer.RequiredAcks != sarama.WaitForAll ||
					sc.Producer.Retry.Max != 3 || sc.Net.MaxOpenRequests != 1 {
					t.Fatalf("unexpected idempotent config: %+v", sc.Producer)
				}
			},
		},
		{
			name: "non-idempotent with zero retries and acks=0 preserved",
			mutate: func(p *ProducerConfig) {
				p.Idempotent = false
				p.RequiredAcks = 0
				p.MaxRetries = 0
				p.RetryBackoff = 0
				p.Compression = "none"
			},
			checkSarm: func(t *testing.T, sc *sarama.Config) {
				if sc.Producer.Idempotent || sc.Producer.RequiredAcks != sarama.NoResponse ||
					sc.Producer.Retry.Max != 0 || sc.Producer.Retry.Backoff != 0 ||
					sc.Producer.Compression != sarama.CompressionNone {
					t.Fatalf("zero/false values were overwritten: %+v", sc.Producer)
				}
			},
		},
		{
			name: "non-idempotent with retries applied",
			mutate: func(p *ProducerConfig) {
				p.Idempotent = false
				p.RequiredAcks = 1
				p.MaxRetries = 5
				p.Compression = "gzip"
			},
			checkSarm: func(t *testing.T, sc *sarama.Config) {
				if sc.Producer.Idempotent || sc.Producer.RequiredAcks != sarama.WaitForLocal ||
					sc.Producer.Retry.Max != 5 || sc.Producer.Compression != sarama.CompressionGZIP {
					t.Fatalf("unexpected non-idempotent retries/acks: %+v", sc.Producer)
				}
			},
		},
		{
			name: "empty compression defaults to snappy",
			mutate: func(p *ProducerConfig) {
				p.Compression = ""
			},
			checkSarm: func(t *testing.T, sc *sarama.Config) {
				if sc.Producer.Compression != sarama.CompressionSnappy {
					t.Fatalf("expected snappy compression, got %v", sc.Producer.Compression)
				}
			},
		},
		{
			name:    "unknown compression rejected",
			mutate:  func(p *ProducerConfig) { p.Compression = "brotli" },
			wantErr: "unsupported producer compression",
		},
		{
			name:    "idempotent with acks=1 rejected",
			mutate:  func(p *ProducerConfig) { p.Idempotent = true; p.RequiredAcks = 1 },
			wantErr: "idempotent producer requires required_acks=-1",
		},
		{
			name:    "idempotent with max_retries=0 rejected",
			mutate:  func(p *ProducerConfig) { p.Idempotent = true; p.MaxRetries = 0 },
			wantErr: "idempotent producer requires max_retries >= 1",
		},
		{
			name:    "invalid acks value rejected",
			mutate:  func(p *ProducerConfig) { p.Idempotent = false; p.RequiredAcks = 2 },
			wantErr: "required_acks must be -1, 0, or 1",
		},
		{
			name:    "negative retries rejected",
			mutate:  func(p *ProducerConfig) { p.Idempotent = false; p.MaxRetries = -1 },
			wantErr: "max_retries cannot be negative",
		},
		{
			name:    "non-positive max_message_bytes rejected",
			mutate:  func(p *ProducerConfig) { p.MaxMessageBytes = 0 },
			wantErr: "max_message_bytes",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pc := DefaultProducerConfig()
			tc.mutate(&pc)
			npc, err := normalizeAndValidateProducerConfig(pc)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			sc, err := buildProducerSaramaConfig(broker, npc)
			if err != nil {
				t.Fatalf("buildProducerSaramaConfig: %v", err)
			}
			if tc.checkSarm != nil {
				tc.checkSarm(t, sc)
			}
		})
	}
}

func TestConsumerConfigValidationAndMapping(t *testing.T) {
	broker := DefaultBrokerConfig()
	broker.Brokers = []string{"localhost:9092"}

	tests := []struct {
		name      string
		mutate    func(*ConsumerConfig)
		wantErr   string
		checkSarm func(*testing.T, *sarama.Config)
	}{
		{
			name:   "oldest and sticky periodic",
			mutate: func(c *ConsumerConfig) { c.InitialOffset = "oldest"; c.BalanceStrategy = "sticky" },
			checkSarm: func(t *testing.T, sc *sarama.Config) {
				if sc.Consumer.Offsets.Initial != sarama.OffsetOldest {
					t.Fatalf("expected OffsetOldest")
				}
				if len(sc.Consumer.Group.Rebalance.GroupStrategies) != 1 ||
					sc.Consumer.Group.Rebalance.GroupStrategies[0].Name() != sarama.StickyBalanceStrategyName {
					t.Fatalf("expected sticky balance strategy")
				}
				if !sc.Consumer.Offsets.AutoCommit.Enable || sc.Consumer.Offsets.AutoCommit.Interval != 5*time.Second {
					t.Fatalf("expected periodic autocommit")
				}
			},
		},
		{
			name:   "range assignor and sync mode",
			mutate: func(c *ConsumerConfig) { c.BalanceStrategy = "range"; c.CommitMode = CommitSync; c.CommitInterval = 0 },
			checkSarm: func(t *testing.T, sc *sarama.Config) {
				if sc.Consumer.Group.Rebalance.GroupStrategies[0].Name() != sarama.RangeBalanceStrategyName {
					t.Fatalf("expected range balance strategy")
				}
				if sc.Consumer.Offsets.AutoCommit.Enable {
					t.Fatalf("expected AutoCommit.Enable=false in sync mode")
				}
				if sc.Consumer.Offsets.AutoCommit.Interval <= 0 {
					t.Fatalf("expected positive Sarama AutoCommit.Interval even in sync mode")
				}
			},
		},
		{
			name:    "unknown initial offset rejected",
			mutate:  func(c *ConsumerConfig) { c.InitialOffset = "earliest" },
			wantErr: "invalid initial_offset",
		},
		{
			name:    "unknown balance strategy rejected",
			mutate:  func(c *ConsumerConfig) { c.BalanceStrategy = "cooperative-sticky" },
			wantErr: "unsupported balance_strategy",
		},
		{
			name:    "unknown commit mode rejected",
			mutate:  func(c *ConsumerConfig) { c.CommitMode = CommitMode("manual") },
			wantErr: "unsupported commit_mode",
		},
		{
			name:    "zero commit interval rejected in periodic mode",
			mutate:  func(c *ConsumerConfig) { c.CommitMode = CommitPeriodic; c.CommitInterval = 0 },
			wantErr: "commit_interval must be greater than zero",
		},
		{
			name:    "heartbeat >= session rejected",
			mutate:  func(c *ConsumerConfig) { c.HeartbeatInterval = 10 * time.Second; c.SessionTimeout = 10 * time.Second },
			wantErr: "heartbeat_interval",
		},
		{
			name:    "missing group id rejected",
			mutate:  func(c *ConsumerConfig) { c.GroupID = "" },
			wantErr: "group ID is required",
		},
		{
			name:    "negative handler timeout rejected",
			mutate:  func(c *ConsumerConfig) { c.HandlerTimeout = -time.Second },
			wantErr: "handler_timeout",
		},
		{
			name:    "negative shutdown timeout rejected",
			mutate:  func(c *ConsumerConfig) { c.ShutdownTimeout = -time.Second },
			wantErr: "shutdown_timeout",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cc := DefaultConsumerConfig()
			cc.GroupID = "group-a"
			tc.mutate(&cc)
			ncc, err := normalizeAndValidateConsumerConfig(cc)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			sc, err := buildConsumerSaramaConfig(broker, ncc)
			if err != nil {
				t.Fatalf("buildConsumerSaramaConfig: %v", err)
			}
			if tc.checkSarm != nil {
				tc.checkSarm(t, sc)
			}
		})
	}

	// Verify fresh strategy instances per call (no shared mutable sticky strategy).
	cc := DefaultConsumerConfig()
	cc.GroupID = "group-sticky"
	cc.BalanceStrategy = "sticky"
	s1, err := buildConsumerSaramaConfig(broker, cc)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := buildConsumerSaramaConfig(broker, cc)
	if err != nil {
		t.Fatal(err)
	}
	if s1.Consumer.Group.Rebalance.GroupStrategies[0] == s2.Consumer.Group.Rebalance.GroupStrategies[0] {
		t.Fatalf("expected fresh BalanceStrategy instance per config build")
	}
}

func TestTLSSASLValidationAndFixtures(t *testing.T) {
	dir := t.TempDir()
	caPEM, cert1PEM, key1PEM, cert2PEM, key2PEM := generateTestCerts(t)

	caFile := filepath.Join(dir, "ca.crt")
	cert1File := filepath.Join(dir, "client1.crt")
	key1File := filepath.Join(dir, "client1.key")
	key2File := filepath.Join(dir, "client2.key")
	badPEMFile := filepath.Join(dir, "bad.pem")
	_ = cert2PEM

	writeTestFile(t, caFile, caPEM)
	writeTestFile(t, cert1File, cert1PEM)
	writeTestFile(t, key1File, key1PEM)
	writeTestFile(t, key2File, key2PEM)
	writeTestFile(t, badPEMFile, []byte("not-a-pem"))

	// Valid TLS + SCRAM-SHA-256
	b := DefaultBrokerConfig()
	b.Brokers = []string{"localhost:9093"}
	b.Auth.TLS = TLSConfig{
		Enabled:  true,
		CertFile: cert1File,
		KeyFile:  key1File,
		CAFile:   caFile,
	}
	b.Auth.SASL = SASLConfig{
		Enabled:   true,
		Mechanism: "SCRAM-SHA-256",
		Username:  "user",
		Password:  "secret",
	}
	nb, err := normalizeAndValidateBrokerConfig(b)
	if err != nil {
		t.Fatalf("valid TLS/SASL failed validation: %v", err)
	}
	sc, err := buildBaseSaramaConfig(nb)
	if err != nil {
		t.Fatalf("buildBaseSaramaConfig failed: %v", err)
	}
	if !sc.Net.TLS.Enable || sc.Net.TLS.Config == nil || sc.Net.TLS.Config.RootCAs == nil || len(sc.Net.TLS.Config.Certificates) != 1 {
		t.Fatalf("expected TLS config populated with cert and custom CA pool")
	}
	if sc.Net.SASL.SCRAMClientGeneratorFunc == nil {
		t.Fatalf("expected SCRAM generator")
	}
	c1 := sc.Net.SASL.SCRAMClientGeneratorFunc()
	c2 := sc.Net.SASL.SCRAMClientGeneratorFunc()
	if c1 == c2 {
		t.Fatalf("expected distinct SCRAM client instances per generator call")
	}

	// Partial cert/key pair rejected
	partial := b
	partial.Auth.TLS.KeyFile = ""
	if _, err := normalizeAndValidateBrokerConfig(partial); err == nil {
		t.Fatalf("expected error for cert without key")
	}

	// Mismatched cert and key rejected
	mismatch := b
	mismatch.Auth.TLS.KeyFile = key2File
	if _, err := buildBaseSaramaConfig(mismatch); err == nil {
		t.Fatalf("expected error for mismatched cert/key pair")
	}

	// Invalid CA PEM rejected
	badCA := b
	badCA.Auth.TLS.CAFile = badPEMFile
	if _, err := buildBaseSaramaConfig(badCA); err == nil {
		t.Fatalf("expected error for invalid CA PEM")
	}

	// Missing CA file rejected
	missingCA := b
	missingCA.Auth.TLS.CAFile = filepath.Join(dir, "missing.crt")
	if _, err := buildBaseSaramaConfig(missingCA); err == nil {
		t.Fatalf("expected error for missing CA file")
	}

	// Unknown SASL mechanism rejected
	badSASL := b
	badSASL.Auth.SASL.Mechanism = "OAUTHBEARER"
	if _, err := normalizeAndValidateBrokerConfig(badSASL); err == nil {
		t.Fatalf("expected error for unsupported SASL mechanism")
	}

	// F11: SASL PLAIN without TLS requires explicit AllowInsecurePlaintext opt-in.
	plainNoTLS := DefaultBrokerConfig()
	plainNoTLS.Brokers = []string{"localhost:9092"}
	plainNoTLS.Auth.SASL = SASLConfig{
		Enabled:   true,
		Mechanism: "PLAIN",
		Username:  "user",
		Password:  "secret",
	}
	if _, err := normalizeAndValidateBrokerConfig(plainNoTLS); err == nil {
		t.Fatalf("expected error for SASL PLAIN over unencrypted transport without AllowInsecurePlaintext")
	}
	plainNoTLS.Auth.SASL.AllowInsecurePlaintext = true
	if _, err := normalizeAndValidateBrokerConfig(plainNoTLS); err != nil {
		t.Fatalf("expected SASL PLAIN with explicit AllowInsecurePlaintext to succeed: %v", err)
	}

	// F11: Contradictory TLS/SASL options when disabled must be rejected.
	for _, tc := range []struct {
		name   string
		mutate func(*BrokerConfig)
	}{
		{
			name:   "disabled TLS with CertFile",
			mutate: func(bc *BrokerConfig) { bc.Auth.TLS.CertFile = cert1File },
		},
		{
			name:   "disabled TLS with KeyFile",
			mutate: func(bc *BrokerConfig) { bc.Auth.TLS.KeyFile = key1File },
		},
		{
			name:   "disabled TLS with CAFile",
			mutate: func(bc *BrokerConfig) { bc.Auth.TLS.CAFile = caFile },
		},
		{
			name:   "disabled TLS with InsecureSkipVerify",
			mutate: func(bc *BrokerConfig) { bc.Auth.TLS.InsecureSkipVerify = true },
		},
		{
			name:   "disabled SASL with Username",
			mutate: func(bc *BrokerConfig) { bc.Auth.SASL.Username = "stray" },
		},
		{
			name:   "disabled SASL with Password",
			mutate: func(bc *BrokerConfig) { bc.Auth.SASL.Password = "stray" },
		},
		{
			name:   "disabled SASL with AllowInsecurePlaintext",
			mutate: func(bc *BrokerConfig) { bc.Auth.SASL.AllowInsecurePlaintext = true },
		},
		{
			name:   "disabled SASL with non-default Mechanism",
			mutate: func(bc *BrokerConfig) { bc.Auth.SASL.Mechanism = "SCRAM-SHA-256" },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultBrokerConfig()
			cfg.Brokers = []string{"localhost:9092"}
			tc.mutate(&cfg)
			if _, err := normalizeAndValidateBrokerConfig(cfg); err == nil {
				t.Fatalf("expected validation error for %s", tc.name)
			}
		})
	}

	// Valid unauthenticated DefaultBrokerConfig remains accepted.
	plainDefault := DefaultBrokerConfig()
	plainDefault.Brokers = []string{"localhost:9092"}
	nd, err := normalizeAndValidateBrokerConfig(plainDefault)
	if err != nil {
		t.Fatalf("default unauthenticated broker config failed: %v", err)
	}
	dsc, err := buildBaseSaramaConfig(nd)
	if err != nil {
		t.Fatal(err)
	}
	if dsc.Net.TLS.Enable || dsc.Net.SASL.Enable {
		t.Fatalf("disabled TLS/SASL must stay disabled")
	}
}

func TestConfigEnvTagsPresent(t *testing.T) {
	checkTag := func(typ reflect.Type, field, expectedEnv string) {
		t.Helper()
		f, ok := typ.FieldByName(field)
		if !ok {
			t.Fatalf("missing field %s on %s", field, typ.Name())
		}
		if got := f.Tag.Get("env"); got != expectedEnv {
			t.Fatalf("%s.%s env tag = %q, want %q", typ.Name(), field, got, expectedEnv)
		}
	}

	brokerType := reflect.TypeOf(BrokerConfig{})
	checkTag(brokerType, "Brokers", "KAFKA_BROKERS")
	checkTag(brokerType, "ClientID", "KAFKA_CLIENT_ID")
	checkTag(brokerType, "DialTimeout", "KAFKA_DIAL_TIMEOUT")
	checkTag(brokerType, "ReadTimeout", "KAFKA_READ_TIMEOUT")
	checkTag(brokerType, "WriteTimeout", "KAFKA_WRITE_TIMEOUT")

	consumerType := reflect.TypeOf(ConsumerConfig{})
	checkTag(consumerType, "BalanceStrategy", "BALANCE_STRATEGY")
	checkTag(consumerType, "IsolationLevel", "ISOLATION_LEVEL")
	checkTag(consumerType, "ResetInvalidOffsets", "RESET_INVALID_OFFSETS")
	checkTag(consumerType, "RebalanceTimeout", "REBALANCE_TIMEOUT")
	checkTag(consumerType, "CommitMode", "COMMIT_MODE")
	checkTag(consumerType, "HandlerTimeout", "HANDLER_TIMEOUT")
	checkTag(consumerType, "ShutdownTimeout", "SHUTDOWN_TIMEOUT")
	checkTag(consumerType, "CloseTimeout", "CLOSE_TIMEOUT")

	producerType := reflect.TypeOf(ProducerConfig{})
	checkTag(producerType, "Partitioner", "PARTITIONER")

	saslType := reflect.TypeOf(SASLConfig{})
	checkTag(saslType, "AllowInsecurePlaintext", "ALLOW_INSECURE_PLAINTEXT")
}

func TestTopicNameValidationAndNewConfigMappings(t *testing.T) {
	// F12: Topic name validation rules
	validTopics := []string{"a", "orders.created", "telemetry_v1-prod", strings.Repeat("x", 249)}
	for _, topic := range validTopics {
		if err := validateTopicName(topic); err != nil {
			t.Fatalf("expected valid topic %q, got %v", topic, err)
		}
	}
	invalidTopics := []string{"", " ", " orders", "orders ", ".", "..", "a/b", "a*b", "topic$1", "bad topic", strings.Repeat("x", 250)}
	for _, topic := range invalidTopics {
		if err := validateTopicName(topic); err == nil {
			t.Fatalf("expected invalid topic %q to be rejected", topic)
		}
	}

	// F01, F05, F13, F15 mappings
	broker := DefaultBrokerConfig()
	broker.Brokers = []string{"localhost:9092"}

	cc := DefaultConsumerConfig()
	cc.GroupID = "grp-new"
	cc.ResetInvalidOffsets = false
	cc.IsolationLevel = IsolationReadCommitted
	cc.CloseTimeout = 7 * time.Second
	ncc, err := normalizeAndValidateConsumerConfig(cc)
	if err != nil {
		t.Fatalf("normalizeAndValidateConsumerConfig: %v", err)
	}
	cs, err := buildConsumerSaramaConfig(broker, ncc)
	if err != nil {
		t.Fatalf("buildConsumerSaramaConfig: %v", err)
	}
	if cs.Consumer.Group.ResetInvalidOffsets {
		t.Fatalf("expected Sarama ResetInvalidOffsets=false")
	}
	if cs.Consumer.IsolationLevel != sarama.ReadCommitted {
		t.Fatalf("expected Sarama ReadCommitted, got %v", cs.Consumer.IsolationLevel)
	}

	ccBadIso := DefaultConsumerConfig()
	ccBadIso.GroupID = "grp-iso"
	ccBadIso.IsolationLevel = IsolationLevel("serializable")
	if _, err := normalizeAndValidateConsumerConfig(ccBadIso); err == nil {
		t.Fatalf("expected error for invalid isolation_level")
	}

	ccBadClose := DefaultConsumerConfig()
	ccBadClose.GroupID = "grp-close"
	ccBadClose.CloseTimeout = -time.Second
	if _, err := normalizeAndValidateConsumerConfig(ccBadClose); err == nil {
		t.Fatalf("expected error for negative close_timeout")
	}

	for _, part := range []string{"hash", "crc32", "murmur2", "roundrobin", "random"} {
		pc := DefaultProducerConfig()
		pc.Partitioner = part
		npc, err := normalizeAndValidateProducerConfig(pc)
		if err != nil {
			t.Fatalf("partitioner %q failed validation: %v", part, err)
		}
		ps, err := buildProducerSaramaConfig(broker, npc)
		if err != nil {
			t.Fatalf("partitioner %q failed sarama build: %v", part, err)
		}
		if ps.Producer.Partitioner == nil {
			t.Fatalf("partitioner %q produced nil sarama Partitioner", part)
		}
	}

	pcBad := DefaultProducerConfig()
	pcBad.Partitioner = "fnv64"
	if _, err := normalizeAndValidateProducerConfig(pcBad); err == nil {
		t.Fatalf("expected error for unsupported partitioner")
	}
}

func writeTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func generateTestCerts(t *testing.T) (caPEM, cert1PEM, key1PEM, cert2PEM, key2PEM []byte) {
	t.Helper()
	now := time.Now()

	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	genLeaf := func(serial int64, cn string) ([]byte, []byte) {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(serial),
			Subject:      pkix.Name{CommonName: cn},
			NotBefore:    now.Add(-time.Hour),
			NotAfter:     now.Add(24 * time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
			IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, caTmpl, &k.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		cPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		kPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
		return cPEM, kPEM
	}

	cert1PEM, key1PEM = genLeaf(2, "client-1")
	cert2PEM, key2PEM = genLeaf(3, "client-2")
	return caPEM, cert1PEM, key1PEM, cert2PEM, key2PEM
}
