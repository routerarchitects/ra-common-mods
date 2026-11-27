package examples_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/routerarchitects/ra-common-mods/kafka"
)

type appConfig struct {
	Broker   kafka.BrokerConfig
	Consumer kafka.ConsumerConfig `envPrefix:"KAFKA_CONSUMER_"`
	Producer kafka.ProducerConfig `envPrefix:"KAFKA_PRODUCER_"`
}

func TestEnvParsingBrokerProducerConsumerConfig(t *testing.T) {
	t.Setenv("KAFKA_BROKERS", "broker1:9092,broker2:9092")
	t.Setenv("KAFKA_CLIENT_ID", "svc-test")
	t.Setenv("KAFKA_DIAL_TIMEOUT", "12s")
	t.Setenv("KAFKA_READ_TIMEOUT", "15s")
	t.Setenv("KAFKA_WRITE_TIMEOUT", "20s")
	t.Setenv("KAFKA_CONSUMER_GROUP_ID", "grp-env")
	t.Setenv("KAFKA_CONSUMER_INITIAL_OFFSET", "oldest")
	t.Setenv("KAFKA_CONSUMER_BALANCE_STRATEGY", "sticky")
	t.Setenv("KAFKA_CONSUMER_REBALANCE_TIMEOUT", "45s")
	t.Setenv("KAFKA_CONSUMER_COMMIT_MODE", "sync")
	t.Setenv("KAFKA_CONSUMER_HANDLER_TIMEOUT", "25s")
	t.Setenv("KAFKA_CONSUMER_SHUTDOWN_TIMEOUT", "15s")
	t.Setenv("KAFKA_CONSUMER_CLOSE_TIMEOUT", "7s")
	t.Setenv("KAFKA_CONSUMER_RESET_INVALID_OFFSETS", "false")
	t.Setenv("KAFKA_CONSUMER_ISOLATION_LEVEL", "read_committed")
	t.Setenv("KAFKA_PRODUCER_COMPRESSION", "gzip")
	t.Setenv("KAFKA_PRODUCER_PARTITIONER", "crc32")
	t.Setenv("KAFKA_PRODUCER_MAX_RETRIES", "5")

	var app appConfig
	if err := env.Parse(&app); err != nil {
		t.Fatalf("env.Parse(app): %v", err)
	}
	if len(app.Broker.Brokers) != 2 || app.Broker.ClientID != "svc-test" ||
		app.Broker.DialTimeout != 12*time.Second || app.Broker.ReadTimeout != 15*time.Second || app.Broker.WriteTimeout != 20*time.Second ||
		app.Consumer.GroupID != "grp-env" || app.Consumer.InitialOffset != "oldest" ||
		app.Consumer.BalanceStrategy != "sticky" || app.Consumer.RebalanceTimeout != 45*time.Second ||
		app.Consumer.CommitMode != kafka.CommitSync || app.Consumer.HandlerTimeout != 25*time.Second ||
		app.Consumer.ShutdownTimeout != 15*time.Second || app.Consumer.CloseTimeout != 7*time.Second ||
		app.Consumer.ResetInvalidOffsets || app.Consumer.IsolationLevel != kafka.IsolationReadCommitted ||
		app.Producer.Compression != "gzip" || app.Producer.Partitioner != "crc32" ||
		app.Producer.MaxRetries != 5 {
		t.Fatalf("unexpected composed parsed config: %+v", app)
	}
}

func TestSampleEnvPassesConsumerValidation(t *testing.T) {
	sample, err := os.ReadFile("../env.sample")
	if err != nil {
		t.Fatal(err)
	}
	var assignments []string
	for _, line := range strings.Split(string(sample), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			assignments = append(assignments, line)
		}
	}

	var app appConfig
	if err := env.ParseWithOptions(&app, env.Options{Environment: env.ToMap(assignments)}); err != nil {
		t.Fatalf("parse env.sample: %v", err)
	}
	consumer, err := kafka.NewConsumer(app.Broker, app.Consumer, nil)
	if err != nil {
		t.Fatalf("validate env.sample: %v", err)
	}
	if err := consumer.Close(); err != nil {
		t.Fatal(err)
	}
}
