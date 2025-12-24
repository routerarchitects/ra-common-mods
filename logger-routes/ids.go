package logger_routes_fiber

import (
	"crypto/rand"
	"encoding/hex"
)

func newRequestID() string {
	// 16 bytes => 32 hex chars
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
