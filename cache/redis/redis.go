package redis

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"expvar"
	"log/slog"
	"math/rand"
	"strings"
	"time"

	"github.com/google/uuid"
	red "github.com/redis/go-redis/v9"
	"github.com/routerarchitects/ra-common-mods/apperror"
	"github.com/routerarchitects/ra-common-mods/cache"
	"github.com/routerarchitects/ra-common-mods/logger"
)

type Cache struct {
	cfg     cache.Config
	client  red.UniversalClient
	codec   cache.Codec
	builder cache.KeyBuilder
	log     *slog.Logger
}

var (
	cacheHitTotal   = expvar.NewInt("cache_hit_total")
	cacheMissTotal  = expvar.NewInt("cache_miss_total")
	cacheErrorTotal = expvar.NewInt("cache_error_total")

	cacheGetLatencyMsTotal    = expvar.NewInt("cache_get_latency_ms_total")
	cacheSetLatencyMsTotal    = expvar.NewInt("cache_set_latency_ms_total")
	cacheLoaderLatencyMsTotal = expvar.NewInt("cache_loader_latency_ms_total")
)

const unlockScript = `if redis.call("GET", KEYS[1]) == ARGV[1] then return redis.call("DEL", KEYS[1]) else return 0 end`

func New(cfg cache.Config) (cache.Cache, error) {
	cfg = applyDefaults(cfg)

	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	builder, err := cache.NewKeyBuilder(cfg.ServiceName, cfg.KeyPrefix)
	if err != nil {
		return nil, err
	}
	client, err := newClient(cfg)
	if err != nil {
		return nil, err
	}
	return &Cache{
		cfg:     cfg,
		client:  client,
		codec:   cache.JSONCodec{},
		builder: builder,
		log:     logger.Subsystem("cache"),
	}, nil
}

func (c *Cache) Get(ctx context.Context, key cache.Key, dest any) error {
	redisKey, err := c.builder.Build(key)
	if err != nil {
		return err
	}
	return c.getByStringKey(ctx, redisKey, dest)
}

func (c *Cache) getByStringKey(ctx context.Context, redisKey string, dest any) error {
	start := time.Now()
	defer observeGetLatency(start)

	payload, err := c.client.Get(ctx, redisKey).Bytes()
	if err != nil {
		if errors.Is(err, red.Nil) {
			incMiss()
			return apperror.New(apperror.CodeNotFound, "cache miss")
		}
		incError()
		return wrapRedisError("get", err)
	}
	if err := c.codec.Unmarshal(payload, dest); err != nil {
		incError()
		return apperror.Wrap(apperror.CodeInternal, "cache deserialization failed", err)
	}
	incHit()
	return nil
}

func (c *Cache) Set(ctx context.Context, key cache.Key, value any, ttl time.Duration) error {
	start := time.Now()
	defer observeSetLatency(start)

	redisKey, err := c.builder.Build(key)
	if err != nil {
		incError()
		return err
	}
	payload, err := c.codec.Marshal(value)
	if err != nil {
		incError()
		return apperror.Wrap(apperror.CodeInternal, "cache serialization failed", err)
	}
	effectiveTTL, err := c.resolveEffectiveTTL(ttl)
	if err != nil {
		incError()
		return err
	}
	effectiveTTL = applyTTLJitter(effectiveTTL, c.cfg.EnableTTLJitter, c.cfg.TTLJitterPct)
	if err := c.client.Set(ctx, redisKey, payload, effectiveTTL).Err(); err != nil {
		incError()
		return wrapRedisError("set", err)
	}
	return nil
}

func (c *Cache) Delete(ctx context.Context, keys ...cache.Key) error {
	if len(keys) == 0 {
		return nil
	}
	redisKeys := make([]string, 0, len(keys))
	for _, key := range keys {
		redisKey, err := c.builder.Build(key)
		if err != nil {
			return err
		}
		redisKeys = append(redisKeys, redisKey)
	}
	if err := c.client.Del(ctx, redisKeys...).Err(); err != nil {
		return wrapRedisError("delete", err)
	}
	return nil
}

func (c *Cache) Exists(ctx context.Context, key cache.Key) (bool, error) {
	redisKey, err := c.builder.Build(key)
	if err != nil {
		return false, err
	}
	count, err := c.client.Exists(ctx, redisKey).Result()
	if err != nil {
		return false, wrapRedisError("exists", err)
	}
	return count > 0, nil
}

func (c *Cache) GetOrSet(ctx context.Context, key cache.Key, dest any, ttl time.Duration, loader func(context.Context) (any, error)) error {
	if loader == nil {
		incError()
		return apperror.New(apperror.CodeInvalidInput, "cache loader is required")
	}
	if _, err := c.resolveEffectiveTTL(ttl); err != nil {
		incError()
		return err
	}

	redisKey, err := c.builder.Build(key)
	if err != nil {
		incError()
		return err
	}
	if err := c.getByStringKey(ctx, redisKey, dest); err == nil {
		return nil
	} else if apperror.CodeOf(err) != apperror.CodeNotFound {
		c.log.WarnContext(ctx, "cache get failed, falling back to loader", "operation", "get", "key_hash", hashKey(redisKey), "error", err)
		return c.loadAndFill(ctx, key, dest, ttl, loader)
	}

	if !c.cfg.EnableStampedeProtection {
		return c.loadAndFill(ctx, key, dest, ttl, loader)
	}

	lockKey, err := c.builder.LockKey(key)
	if err != nil {
		return err
	}
	started := time.Now()
	token, acquired, err := c.tryAcquireLock(ctx, lockKey)
	if err != nil {
		c.log.WarnContext(ctx, "cache lock acquire failed, using loader", "operation", "lock", "key_hash", hashKey(redisKey), "error", err)
		return c.loadAndFill(ctx, key, dest, ttl, loader)
	}
	if acquired {
		defer func() {
			if releaseErr := c.releaseLock(context.Background(), lockKey, token); releaseErr != nil {
				c.log.Error("cache lock release failed", "operation", "unlock", "key_hash", hashKey(redisKey), "error", releaseErr)
			}
		}()
		if err := c.getByStringKey(ctx, redisKey, dest); err == nil {
			return nil
		}
		return c.loadAndFill(ctx, key, dest, ttl, loader)
	}

	if err := c.waitForFill(ctx, redisKey, dest, started); err == nil {
		return nil
	}
	return c.loadAndFill(ctx, key, dest, ttl, loader)
}

func (c *Cache) loadAndFill(ctx context.Context, key cache.Key, dest any, ttl time.Duration, loader func(context.Context) (any, error)) error {
	if loader == nil {
		incError()
		return apperror.New(apperror.CodeInvalidInput, "cache loader is required")
	}

	loaderCtx, cancel, err := c.loaderContext(ctx)
	if err != nil {
		incError()
		return err
	}
	defer cancel()

	loaderStart := time.Now()
	value, err := loader(loaderCtx)
	observeLoaderLatency(loaderStart)
	if err != nil {
		incError()
		return apperror.Wrap(apperror.CodeInternal, "cache loader failed", err)
	}
	if err := c.Set(ctx, key, value, ttl); err != nil {
		// Invalid input (for example non-positive effective TTL) should not silently succeed.
		if apperror.CodeOf(err) == apperror.CodeInvalidInput {
			return err
		}
		c.log.WarnContext(ctx, "cache set failed after loader", "operation", "set", "domain", key.Domain, "entity", key.Entity, "error", err)
	}
	payload, err := c.codec.Marshal(value)
	if err != nil {
		incError()
		return apperror.Wrap(apperror.CodeInternal, "cache serialization failed", err)
	}
	if err := c.codec.Unmarshal(payload, dest); err != nil {
		incError()
		return apperror.Wrap(apperror.CodeInternal, "cache deserialization failed", err)
	}
	return nil
}

func (c *Cache) Close() error {
	return c.client.Close()
}

func (c *Cache) Ping(ctx context.Context) error {
	if err := c.client.Ping(ctx).Err(); err != nil {
		return wrapRedisError("ping", err)
	}
	return nil
}

func validateConfig(cfg cache.Config) error {
	if cfg.ServiceName == "" && cfg.KeyPrefix == "" {
		return cacheErrInvalidConfig("service name or key prefix is required")
	}
	if len(cfg.Addrs) == 0 {
		return cacheErrInvalidConfig("at least one redis address is required")
	}
	if cfg.TTLJitterPct < 0 || cfg.TTLJitterPct > 0.5 {
		return cacheErrInvalidConfig("ttl jitter percent must be in range [0.0, 0.5]")
	}
	if cfg.EnableStampedeProtection && cfg.LockTTL <= time.Millisecond {
		return cacheErrInvalidConfig("lock ttl must be greater than 1ms when stampede protection is enabled")
	}
	return nil
}

func applyDefaults(cfg cache.Config) cache.Config {
	if cfg.LockTTL <= 0 {
		cfg.LockTTL = 3 * time.Second
	}
	if cfg.LockWaitTimeout <= 0 {
		cfg.LockWaitTimeout = 2 * time.Second
	}
	if cfg.LockRetryInterval <= 0 {
		cfg.LockRetryInterval = 100 * time.Millisecond
	}
	return cfg
}

func wrapRedisError(operation string, err error) error {
	return apperror.WrapWithMeta(apperror.CodeInternal, "cache redis command failed", err, map[string]any{"operation": operation, "backend": "redis"})
}

func cacheErrInvalidConfig(msg string) error {
	return apperror.Wrap(apperror.CodeInvalidInput, "cache invalid config", errors.New(msg))
}

func hashKey(k string) string {
	sum := sha256.Sum256([]byte(k))
	return hex.EncodeToString(sum[:8])
}

func applyTTLJitter(ttl time.Duration, enabled bool, jitterPct float64) time.Duration {
	if !enabled || jitterPct <= 0 {
		return ttl
	}
	factor := 1 + ((rand.Float64()*2 - 1) * jitterPct)
	jittered := time.Duration(float64(ttl) * factor)
	if jittered <= 0 {
		return time.Millisecond
	}
	return jittered
}

func (c *Cache) resolveEffectiveTTL(ttl time.Duration) (time.Duration, error) {
	effective := ttl
	if effective <= 0 {
		effective = c.cfg.DefaultTTL
	}
	if effective <= 0 {
		return 0, apperror.New(apperror.CodeInvalidInput, "cache ttl must be positive")
	}
	return effective, nil
}

func newClient(cfg cache.Config) (red.UniversalClient, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Mode)) {
	case "", "standalone":
		if len(cfg.Addrs) == 0 {
			return nil, cacheErrInvalidConfig("addrs are required")
		}
		return red.NewClient(&red.Options{
			Addr:         cfg.Addrs[0],
			Username:     cfg.Username,
			Password:     cfg.Password,
			DB:           cfg.DB,
			DialTimeout:  nonZero(cfg.DialTimeout, 2*time.Second),
			ReadTimeout:  nonZero(cfg.ReadTimeout, 500*time.Millisecond),
			WriteTimeout: nonZero(cfg.WriteTimeout, 500*time.Millisecond),
			PoolSize:     cfg.PoolSize,
			MinIdleConns: cfg.MinIdleConns,
		}), nil
	case "sentinel":
		if len(cfg.Addrs) == 0 || strings.TrimSpace(cfg.MasterName) == "" {
			return nil, cacheErrInvalidConfig("sentinel mode requires addrs and master name")
		}
		return red.NewFailoverClient(&red.FailoverOptions{
			MasterName:    cfg.MasterName,
			SentinelAddrs: cfg.Addrs,
			Username:      cfg.Username,
			Password:      cfg.Password,
			DB:            cfg.DB,
			DialTimeout:   nonZero(cfg.DialTimeout, 2*time.Second),
			ReadTimeout:   nonZero(cfg.ReadTimeout, 500*time.Millisecond),
			WriteTimeout:  nonZero(cfg.WriteTimeout, 500*time.Millisecond),
			PoolSize:      cfg.PoolSize,
			MinIdleConns:  cfg.MinIdleConns,
		}), nil
	case "cluster":
		if len(cfg.Addrs) == 0 {
			return nil, cacheErrInvalidConfig("cluster mode requires addrs")
		}
		if cfg.DB != 0 {
			return nil, cacheErrInvalidConfig("cluster mode requires db=0")
		}
		return red.NewClusterClient(&red.ClusterOptions{
			Addrs:        cfg.Addrs,
			Username:     cfg.Username,
			Password:     cfg.Password,
			DialTimeout:  nonZero(cfg.DialTimeout, 2*time.Second),
			ReadTimeout:  nonZero(cfg.ReadTimeout, 500*time.Millisecond),
			WriteTimeout: nonZero(cfg.WriteTimeout, 500*time.Millisecond),
			PoolSize:     cfg.PoolSize,
			MinIdleConns: cfg.MinIdleConns,
		}), nil
	default:
		return nil, cacheErrInvalidConfig("unsupported cache mode")
	}
}

func nonZero(value, fallback time.Duration) time.Duration {
	if value <= 0 {
		return fallback
	}
	return value
}

func observeGetLatency(start time.Time) {
	cacheGetLatencyMsTotal.Add(time.Since(start).Milliseconds())
}

func observeSetLatency(start time.Time) {
	cacheSetLatencyMsTotal.Add(time.Since(start).Milliseconds())
}

func observeLoaderLatency(start time.Time) {
	cacheLoaderLatencyMsTotal.Add(time.Since(start).Milliseconds())
}

func incHit() {
	cacheHitTotal.Add(1)
}

func incMiss() {
	cacheMissTotal.Add(1)
}

func incError() {
	cacheErrorTotal.Add(1)
}

func (c *Cache) tryAcquireLock(ctx context.Context, lockKey string) (string, bool, error) {
	token := uuid.NewString()
	ok, err := c.client.SetNX(ctx, lockKey, token, c.cfg.LockTTL).Result()
	if err != nil {
		return "", false, apperror.Wrap(apperror.CodeInternal, "cache lock acquire failed", err)
	}
	return token, ok, nil
}

func (c *Cache) releaseLock(ctx context.Context, lockKey, token string) error {
	if token == "" {
		return nil
	}
	if err := c.client.Eval(ctx, unlockScript, []string{lockKey}, token).Err(); err != nil && err != red.Nil {
		return apperror.Wrap(apperror.CodeInternal, "cache lock release failed", err)
	}
	return nil
}

func (c *Cache) waitForFill(ctx context.Context, key string, dest any, started time.Time) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if c.cfg.LockWaitTimeout > 0 && time.Since(started) > c.cfg.LockWaitTimeout {
			return apperror.New(apperror.CodeTimeout, "cache lock wait timeout")
		}
		if err := c.getByStringKey(ctx, key, dest); err == nil {
			return nil
		} else if apperror.CodeOf(err) != apperror.CodeNotFound {
			return err
		}
		retryInterval := c.cfg.LockRetryInterval
		if retryInterval <= 0 {
			retryInterval = 100 * time.Millisecond
		}
		timer := time.NewTimer(retryInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *Cache) loaderContext(parent context.Context) (context.Context, context.CancelFunc, error) {
	if !c.cfg.EnableStampedeProtection || c.cfg.LockTTL <= 0 {
		return parent, func() {}, nil
	}

	// Keep loader bounded below lock TTL with safety margin.
	safety := c.cfg.LockTTL / 5 // 20%
	if safety < 50*time.Millisecond {
		safety = 50 * time.Millisecond
	}
	loaderBudget := c.cfg.LockTTL - safety
	if loaderBudget <= 0 {
		return nil, nil, apperror.New(apperror.CodeInvalidInput, "lock ttl too small for bounded loader timeout")
	}

	if deadline, ok := parent.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return parent, func() {}, nil
		}
		if remaining < loaderBudget {
			loaderBudget = remaining
		}
	}
	ctx, cancel := context.WithTimeout(parent, loaderBudget)
	return ctx, cancel, nil
}
