package kafka

import (
	"strconv"
	"strings"
	"time"
)

const (
	// Reserved DLQ metadata headers added or replaced by the library on dead-lettered records.
	HeaderOriginalTopic         = "x-original-topic"
	HeaderOriginalPartition     = "x-original-partition"
	HeaderOriginalOffset        = "x-original-offset"
	HeaderOriginalTimestamp     = "x-original-timestamp"
	HeaderOriginalConsumerGroup = "x-original-consumer-group"
	HeaderOriginalError         = "x-original-error"
	HeaderOriginalAttempts      = "x-original-attempts"

	maxOriginalErrorHeaderLen = 512
)

// Message represents a Kafka message with metadata.
// Handlers and interceptors must treat message slices and metadata fields as read-only.
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

func cloneBytesPreserveNil(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

func cloneMessageSnapshot(msg *Message) *Message {
	if msg == nil {
		return nil
	}
	return &Message{
		Key:       cloneBytesPreserveNil(msg.Key),
		Value:     cloneBytesPreserveNil(msg.Value),
		Topic:     msg.Topic,
		Partition: msg.Partition,
		Offset:    msg.Offset,
		Headers:   cloneHeaders(msg.Headers),
		Timestamp: msg.Timestamp,
	}
}

func cloneHeaders(headers []RecordHeader) []RecordHeader {
	if len(headers) == 0 {
		if headers == nil {
			return nil
		}
		return []RecordHeader{}
	}
	out := make([]RecordHeader, len(headers))
	for i, h := range headers {
		out[i] = RecordHeader{
			Key:   h.Key,
			Value: cloneBytesPreserveNil(h.Value),
		}
	}
	return out
}

func isReservedOriginalHeader(key string) bool {
	switch key {
	case HeaderOriginalTopic,
		HeaderOriginalPartition,
		HeaderOriginalOffset,
		HeaderOriginalTimestamp,
		HeaderOriginalConsumerGroup,
		HeaderOriginalError,
		HeaderOriginalAttempts:
		return true
	default:
		return false
	}
}

type dlqMetadata struct {
	Topic         string
	Partition     int32
	Offset        int64
	Timestamp     time.Time
	ConsumerGroup string
	Error         string
	Attempts      int
}

func buildDLQHeaders(orig []RecordHeader, meta dlqMetadata) []RecordHeader {
	headers := make([]RecordHeader, 0, len(orig)+7)
	for _, h := range orig {
		if isReservedOriginalHeader(h.Key) {
			continue
		}
		headers = append(headers, RecordHeader{
			Key:   h.Key,
			Value: cloneBytesPreserveNil(h.Value),
		})
	}

	ts := ""
	if !meta.Timestamp.IsZero() {
		ts = meta.Timestamp.UTC().Format(time.RFC3339Nano)
	}

	headers = append(headers,
		RecordHeader{Key: HeaderOriginalTopic, Value: []byte(meta.Topic)},
		RecordHeader{Key: HeaderOriginalPartition, Value: []byte(strconv.FormatInt(int64(meta.Partition), 10))},
		RecordHeader{Key: HeaderOriginalOffset, Value: []byte(strconv.FormatInt(meta.Offset, 10))},
		RecordHeader{Key: HeaderOriginalTimestamp, Value: []byte(ts)},
		RecordHeader{Key: HeaderOriginalConsumerGroup, Value: []byte(meta.ConsumerGroup)},
		RecordHeader{Key: HeaderOriginalError, Value: []byte(sanitizeAndBoundErrorString(meta.Error, maxOriginalErrorHeaderLen))},
		RecordHeader{Key: HeaderOriginalAttempts, Value: []byte(strconv.Itoa(meta.Attempts))},
	)
	return headers
}

func sanitizeAndBoundErrorString(raw string, maxLen int) string {
	s := strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, raw)
	s = strings.TrimSpace(s)
	if maxLen > 0 && len(s) > maxLen {
		if maxLen <= 3 {
			return s[:maxLen]
		}
		return s[:maxLen-3] + "..."
	}
	return s
}
