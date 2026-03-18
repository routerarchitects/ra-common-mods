package kafka

import (
	"context"
	"time"
)

// Message represents a Kafka message with metadata.
type Message struct {
	// Key is the message key
	Key []byte

	// Value is the message value
	Value []byte

	// Topic is the topic this message was received from
	Topic string

	// Partition is the partition this message was received from
	Partition int32

	// Offset is the offset of this message
	Offset int64

	// Headers are the message headers
	Headers []RecordHeader

	// Timestamp is when the message was produced
	Timestamp time.Time

	// Context carries request-scoped values (tracing, cancellation)
	Context context.Context
}

// RecordHeader represents a Kafka record header (key-value pair).
type RecordHeader struct {
	Key   string
	Value []byte
}

// GetHeader retrieves a header value by key.
func (m *Message) GetHeader(key string) ([]byte, bool) {
	for _, h := range m.Headers {
		if h.Key == key {
			return h.Value, true
		}
	}
	return nil, false
}

// SetHeader sets a header value.
func (m *Message) SetHeader(key string, value []byte) {
	// Update existing header if found
	for i := range m.Headers {
		if m.Headers[i].Key == key {
			m.Headers[i].Value = value
			return
		}
	}
	// Add new header
	m.Headers = append(m.Headers, RecordHeader{Key: key, Value: value})
}
