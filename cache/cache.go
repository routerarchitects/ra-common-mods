package cache

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/routerarchitects/ra-common-mods/apperror"
)

type Key struct {
	Domain     string
	Entity     string
	ID         string
	Version    string
	Qualifiers []string
}

type Config struct {
	Mode string

	Addrs      []string
	Username   string
	Password   string
	DB         int
	MasterName string

	ServiceName string
	KeyPrefix   string

	DefaultTTL      time.Duration
	EnableTTLJitter bool
	TTLJitterPct    float64

	EnableStampedeProtection bool
	LockTTL                  time.Duration
	LockWaitTimeout          time.Duration
	LockRetryInterval        time.Duration

	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	PoolSize     int
	MinIdleConns int
}

type Cache interface {
	Get(ctx context.Context, key Key, dest any) error
	Set(ctx context.Context, key Key, value any, ttl time.Duration) error
	Delete(ctx context.Context, keys ...Key) error
	Exists(ctx context.Context, key Key) (bool, error)
	GetOrSet(ctx context.Context, key Key, dest any, ttl time.Duration, loader func(context.Context) (any, error)) error
	Close() error
}

type HealthChecker interface {
	Ping(ctx context.Context) error
}

type Codec interface {
	Marshal(value any) ([]byte, error)
	Unmarshal(data []byte, dest any) error
	ContentType() string
}

type JSONCodec struct{}

func (JSONCodec) Marshal(value any) ([]byte, error) {
	return json.Marshal(value)
}

func (JSONCodec) Unmarshal(data []byte, dest any) error {
	return json.Unmarshal(data, dest)
}

func (JSONCodec) ContentType() string {
	return "application/json"
}

func errInvalidConfig(msg string, meta map[string]any) error {
	return apperror.WrapWithMeta(apperror.CodeInvalidInput, "cache invalid config", errors.New(msg), meta)
}

func errInvalidKey(msg string, meta map[string]any) error {
	return apperror.WrapWithMeta(apperror.CodeInvalidInput, "cache invalid key", errors.New(msg), meta)
}
