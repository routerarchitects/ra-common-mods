package kafka

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type lagKey struct {
	groupID   string
	topic     string
	partition int32
}

// Metrics holds opt-in Prometheus collectors for Kafka producers and consumers.
// Register collectors explicitly via NewMetrics(reg) using a caller-supplied prometheus.Registerer.
//
// Supported instrumentation model: run at most one Consumer member per group_id per process
// when sharing Metrics, because kafka_consumer_ready is labelled by group_id only.
type Metrics struct {
	messagesProcessedTotal   *prometheus.CounterVec
	messagesFailedTotal      *prometheus.CounterVec
	messagesRetriedTotal     *prometheus.CounterVec
	messagesDiscardedTotal   *prometheus.CounterVec
	messagesInterruptedTotal *prometheus.CounterVec
	dlqMessagesTotal         *prometheus.CounterVec
	handlerDurationSeconds   *prometheus.HistogramVec
	publishDurationSeconds   *prometheus.HistogramVec
	publishErrorsTotal       *prometheus.CounterVec
	consumerLag              *prometheus.GaugeVec
	backlogUpdatedAt         *prometheus.GaugeVec
	commitAttemptsTotal      *prometheus.CounterVec
	commitErrorsTotal        *prometheus.CounterVec
	consumerErrorsTotal      *prometheus.CounterVec
	rebalancesTotal          *prometheus.CounterVec
	consumerReady            *prometheus.GaugeVec

	lagMu     sync.Mutex
	lagOwners map[lagKey]uint64
}

// NewMetrics constructs and registers all Kafka Prometheus collectors with reg.
// It never registers collectors against prometheus.DefaultRegisterer automatically.
func NewMetrics(reg prometheus.Registerer) (*Metrics, error) {
	if reg == nil {
		return nil, fmt.Errorf("prometheus registerer is required")
	}

	m := &Metrics{
		messagesProcessedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kafka_messages_processed_total",
			Help: "Total number of messages successfully processed by handlers.",
		}, []string{"group_id", "topic"}),
		messagesFailedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kafka_messages_failed_total",
			Help: "Total number of failed message handler attempts.",
		}, []string{"group_id", "topic"}),
		messagesRetriedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kafka_messages_retried_total",
			Help: "Total number of additional message handler retry attempts initiated.",
		}, []string{"group_id", "topic"}),
		messagesDiscardedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kafka_messages_discarded_total",
			Help: "Total number of messages explicitly discarded.",
		}, []string{"group_id", "topic", "reason"}),
		messagesInterruptedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kafka_messages_interrupted_total",
			Help: "Total number of admitted messages interrupted before resolution or marking.",
		}, []string{"group_id", "topic"}),
		dlqMessagesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kafka_dlq_messages_total",
			Help: "Total number of DLQ publish attempts by outcome.",
		}, []string{"group_id", "topic", "outcome"}),
		handlerDurationSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "kafka_handler_duration_seconds",
			Help:    "Duration of individual message handler attempts in seconds.",
			Buckets: prometheus.DefBuckets,
		}, []string{"group_id", "topic", "outcome"}),
		publishDurationSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "kafka_publish_duration_seconds",
			Help:    "Duration of synchronous Kafka publish calls in seconds.",
			Buckets: prometheus.DefBuckets,
		}, []string{"topic", "outcome"}),
		publishErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kafka_publish_errors_total",
			Help: "Total number of Kafka publish failures.",
		}, []string{"topic", "reason"}),
		consumerLag: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kafka_consumer_in_process_backlog",
			Help: "Observed in-process partition backlog for locally claimed partitions (Sarama high-water mark minus next ack-eligible offset; not authoritative broker-committed group lag).",
		}, []string{"group_id", "topic", "partition"}),
		backlogUpdatedAt: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kafka_consumer_backlog_updated_timestamp_seconds",
			Help: "Unix timestamp in seconds when the observed in-process partition backlog was last updated.",
		}, []string{"group_id", "topic", "partition"}),
		commitAttemptsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kafka_commit_attempts_total",
			Help: "Total number of explicit synchronous commit calls invoked.",
		}, []string{"group_id"}),
		commitErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kafka_commit_errors_total",
			Help: "Total number of positively identified commit-specific errors.",
		}, []string{"group_id"}),
		consumerErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kafka_consumer_errors_total",
			Help: "Total number of consumer group errors observed by classification.",
		}, []string{"group_id", "class"}),
		rebalancesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kafka_rebalances_total",
			Help: "Total number of consumer group sessions established after the initial session.",
		}, []string{"group_id"}),
		consumerReady: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kafka_consumer_ready",
			Help: "Readiness state of the consumer group member (1=ready, 0=not ready).",
		}, []string{"group_id"}),
		lagOwners: make(map[lagKey]uint64),
	}

	collectors := []prometheus.Collector{
		m.messagesProcessedTotal,
		m.messagesFailedTotal,
		m.messagesRetriedTotal,
		m.messagesDiscardedTotal,
		m.messagesInterruptedTotal,
		m.dlqMessagesTotal,
		m.handlerDurationSeconds,
		m.publishDurationSeconds,
		m.publishErrorsTotal,
		m.consumerLag,
		m.backlogUpdatedAt,
		m.commitAttemptsTotal,
		m.commitErrorsTotal,
		m.consumerErrorsTotal,
		m.rebalancesTotal,
		m.consumerReady,
	}

	for _, c := range collectors {
		if err := reg.Register(c); err != nil {
			return nil, fmt.Errorf("register kafka metric: %w", err)
		}
	}

	return m, nil
}

// WithProducerMetrics enables opt-in Prometheus instrumentation on a Producer.
func WithProducerMetrics(m *Metrics) ProducerOption {
	return func(o *producerOptions) {
		o.metrics = m
	}
}

// WithConsumerMetrics enables opt-in Prometheus instrumentation on a Consumer.
func WithConsumerMetrics(m *Metrics) ConsumerOption {
	return func(o *consumerOptions) {
		o.metrics = m
	}
}

func (m *Metrics) recordPublish(topic string, duration time.Duration, err error) {
	if m == nil {
		return
	}
	if err == nil {
		m.publishDurationSeconds.WithLabelValues(topic, "success").Observe(duration.Seconds())
		return
	}
	m.publishDurationSeconds.WithLabelValues(topic, "failure").Observe(duration.Seconds())
	m.publishErrorsTotal.WithLabelValues(topic, classifyPublishError(err)).Inc()
}

func classifyPublishError(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, ErrProducerClosed):
		return "closed"
	default:
		return "broker_error"
	}
}

func (m *Metrics) recordAttemptFail(groupID, topic string) {
	if m != nil {
		m.messagesFailedTotal.WithLabelValues(groupID, topic).Inc()
	}
}

func (m *Metrics) recordRetry(groupID, topic string) {
	if m != nil {
		m.messagesRetriedTotal.WithLabelValues(groupID, topic).Inc()
	}
}

func (m *Metrics) recordAttemptDuration(groupID, topic string, duration time.Duration, outcome string) {
	if m != nil {
		m.handlerDurationSeconds.WithLabelValues(groupID, topic, outcome).Observe(duration.Seconds())
	}
}

func (m *Metrics) recordDLQAttempt(groupID, sourceTopic string, success bool) {
	if m == nil {
		return
	}
	outcome := "failure"
	if success {
		outcome = "success"
	}
	m.dlqMessagesTotal.WithLabelValues(groupID, sourceTopic, outcome).Inc()
}

func (m *Metrics) recordOutcome(groupID, topic string, res processingResult) {
	if m == nil {
		return
	}
	switch res.Outcome {
	case outcomeProcessed:
		m.messagesProcessedTotal.WithLabelValues(groupID, topic).Inc()
	case outcomeDiscarded:
		reason := res.DiscardReason
		if reason == "" {
			reason = "handler_exhausted"
		}
		m.messagesDiscardedTotal.WithLabelValues(groupID, topic, reason).Inc()
	case outcomeInterrupted:
		m.messagesInterruptedTotal.WithLabelValues(groupID, topic).Inc()
	}
}

func (m *Metrics) recordReady(groupID string, ready bool) {
	if m == nil {
		return
	}
	if ready {
		m.consumerReady.WithLabelValues(groupID).Set(1)
	} else {
		m.consumerReady.WithLabelValues(groupID).Set(0)
	}
}

func (m *Metrics) recordRebalance(groupID string) {
	if m != nil {
		m.rebalancesTotal.WithLabelValues(groupID).Inc()
	}
}

func (m *Metrics) recordCommitAttempt(groupID string) {
	if m != nil {
		m.commitAttemptsTotal.WithLabelValues(groupID).Inc()
	}
}

func (m *Metrics) recordCommitError(groupID string) {
	if m != nil {
		m.commitErrorsTotal.WithLabelValues(groupID).Inc()
	}
}

func (m *Metrics) recordConsumerError(groupID, class string) {
	if m != nil {
		m.consumerErrorsTotal.WithLabelValues(groupID, class).Inc()
	}
}

func (m *Metrics) recordLag(ownerID uint64, groupID, topic string, partition int32, lag int64) {
	if m == nil {
		return
	}
	if lag < 0 {
		lag = 0
	}
	partStr := strconv.FormatInt(int64(partition), 10)
	key := lagKey{groupID: groupID, topic: topic, partition: partition}
	nowSec := float64(time.Now().UnixNano()) / 1e9
	m.lagMu.Lock()
	defer m.lagMu.Unlock()
	m.lagOwners[key] = ownerID
	m.consumerLag.WithLabelValues(groupID, topic, partStr).Set(float64(lag))
	m.backlogUpdatedAt.WithLabelValues(groupID, topic, partStr).Set(nowSec)
}

func (m *Metrics) cleanupLag(ownerID uint64, groupID, topic string, partition int32) {
	if m == nil {
		return
	}
	partStr := strconv.FormatInt(int64(partition), 10)
	key := lagKey{groupID: groupID, topic: topic, partition: partition}
	m.lagMu.Lock()
	defer m.lagMu.Unlock()
	cur, ok := m.lagOwners[key]
	if ok && cur == ownerID {
		delete(m.lagOwners, key)
		m.consumerLag.DeleteLabelValues(groupID, topic, partStr)
		m.backlogUpdatedAt.DeleteLabelValues(groupID, topic, partStr)
	}
}
