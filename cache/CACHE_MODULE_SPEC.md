# Redis Cache Common Module Spec

## 1. Requirement Spec

### 1.1 Purpose
The cache module provides a shared Redis-backed caching abstraction for all services. It centralizes key generation, TTL handling, serialization, Redis operations, and error/logging conventions.

### 1.2 Business and Engineering Goals
- Reduce duplicated cache code across services.
- Hide Redis-specific details from business code.
- Enforce consistent key schema and service isolation.
- Support horizontally scaled service instances with predictable cache behavior.
- Provide graceful degradation when Redis is unavailable.

### 1.3 Functional Requirements
- Expose a common cache interface with:
1. `Get`
2. `Set`
3. `Delete`
4. `Exists`
5. `GetOrSet`
6. `Close`
- Accept only structured cache keys from services.
- Internally generate final Redis key strings.
- Support Redis deployment modes:
1. `standalone`
2. `sentinel`
3. `cluster`
- Support configurable default TTL and per-write TTL override.
- Provide cache-aside read flow through `GetOrSet`.
- Provide optional stampede protection lock for `GetOrSet` misses.
- Provide Redis health check via `Ping`.
- Provide baseline metrics for cache operability.
- Support TTL jitter to reduce synchronized expiry bursts.

### 1.4 Non-Functional Requirements
- All module errors must follow `apperror` conventions.
- All module logs must use shared `logger` module.
- Logging should avoid full key leakage; hashed key preferred.
- Latency-sensitive operations should avoid unnecessary lock usage (`Get/Set/Delete/Exists` remain lock-free).

### 1.5 Constraints and Rules
- Service must not pass raw Redis key strings into APIs.
- `ServiceName` or `KeyPrefix` must be configured.
- In cluster mode, logical DB must be `0`.
- Effective TTL for `Set` must be positive.
- Qualifier entries are optional and appended as normalized segments.

---

## 2. Design Spec

### 2.1 Package Structure
- `cache/`
1. `cache.go`: public interfaces
2. `config.go`: configuration model
3. `key.go`: structured key model
4. `key_builder.go`: key build/validation/normalization logic
5. `codec.go`: codec interface
6. `json_codec.go`: default JSON codec
7. `errors.go`: cache-level error helpers
- `cache/redis/`
1. `redis.go`: Redis cache implementation
2. `client.go`: mode-aware Redis client creation
3. `lock.go`: lock acquire/wait/release behavior
4. `health.go`: ping health check

### 2.2 Key Model and Generation
Input key (service-level):
- `Domain`, `Entity`, `ID` are mandatory.
- `Version` is optional (defaults to `v1` when omitted/empty).
- `Qualifiers` is optional.

Generated key format:
`<prefix>:<domain>:<entity>:<id>:<qualifiers...>:<version>`

Example:
- Input: `{Domain:user, Entity:profile, ID:123, Version:v1}`
- Output: `user-service:user:profile:123:v1`
- Input without version: `{Domain:user, Entity:profile, ID:123}`
- Output: `user-service:user:profile:123:v1`

Normalization rules:
- `trimSpace`
- remove surrounding `:`
- lowercase
- replace spaces with `-`
- reject empty required fields after normalization
- reject values containing internal `:` for required fields and qualifiers
- reject values containing `__` for required fields and qualifiers (reserved internal namespace guard)
- allowed characters per segment: `a-z`, `0-9`, `.`, `_`, `-`

### 2.3 Data Flow Design
#### Get Flow
1. Validate and build redis key.
2. Execute Redis `GET`.
3. If nil -> return cache miss (`NOT_FOUND`).
4. Decode payload into destination object.

#### Set Flow
1. Validate and build redis key.
2. Serialize value via codec.
3. Resolve TTL:
- use method TTL if `>0`
- else use `Config.DefaultTTL`
4. Apply TTL jitter when enabled (after TTL resolution, before Redis `SET`).
4. Execute Redis `SET key value ttl`.

#### GetOrSet Flow
1. Attempt cache `Get`.
2. If hit, return.
3. If miss and stampede protection disabled:
- run loader
- attempt cache `Set`
- return loader value
4. If miss and stampede protection enabled:
- try lock acquire
- if lock acquired: double-check cache, then loader+set, release lock
- if lock not acquired: retry cache read until timeout, else fallback loader

Loader-to-destination rule:
- `loader` returns `any` while `GetOrSet` returns only `error`.
- The module serializes loader output with configured codec and deserializes into `dest`.
- `dest` must be a non-nil writable pointer target.

TTL rule:
- `GetOrSet` uses the same TTL resolution as `Set`:
1. method TTL wins when `ttl > 0`
2. otherwise use `Config.DefaultTTL`
3. if effective TTL `<= 0`, return `CodeInvalidInput`

### 2.4 Stampede Protection Design
- Lock key shape:
`<prefix>:__cache_lock__:<original-key-suffix>`
- `__cache_lock__` namespace is reserved for internal module use and must not be used by service business keys.
- Acquire uses `SET NX PX` semantics via `SetNX` with TTL.
- Lock token uses unique UUID.
- Release uses Lua compare-and-delete for ownership-safe unlock.

Lua:
```lua
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
else
  return 0
end
```

Default lock tuning (if unset):
- `LockTTL = 3s`
- `LockWaitTimeout = 2s`
- `LockRetryInterval = 100ms`

Lock safety contract:
- `LockTTL` must exceed the expected loader timeout.
- Loader execution must run with a bounded context timeout shorter than `LockTTL`.
- Recommended: keep safety margin between loader timeout and `LockTTL` (for example 20-30%).

### 2.5 Error and Logging Design
Error mapping:
- invalid config/key -> `apperror.CodeInvalidInput`
- cache miss -> `apperror.CodeNotFound`
- lock wait timeout -> `apperror.CodeTimeout`
- redis/serialization/deserialization/loader failures -> `apperror.CodeInternal`

Logging approach:
- logger source: `logger.Subsystem("cache")`
- structured fields include: `operation`, `domain`, `entity`, `backend`, `error`, `key_hash`
- redis failures are logged at warn/error based on path criticality

Graceful degradation contract (`GetOrSet`):
- Redis `Get` failure: log warn and fallback to loader.
- Lock acquire failure: log warn and fallback to loader.
- Lock wait timeout: fallback to loader (current default behavior).
- Cache `Set` failure after successful loader: log warn and still return loaded value (DB/source remains truth).

### 2.6 Baseline Metrics
The module should emit minimum metrics in Phase 1:
- `cache_hit_total`
- `cache_miss_total`
- `cache_error_total`
- basic latency for:
1. `Get`
2. `Set`
3. loader execution in `GetOrSet`

Metric labels should remain minimal in Phase 1 (`operation`, `result`, `service`).

---

## 3. Interface Spec

### 3.1 Public Struct Definitions

```go
// key.go
type Key struct {
    Domain     string
    Entity     string
    ID         string
    Version    string
    Qualifiers []string
}
```

```go
// config.go
type Config struct {
    Mode string // standalone | sentinel | cluster

    Addrs      []string
    Username   string
    Password   string
    DB         int
    MasterName string

    ServiceName string
    KeyPrefix   string

    DefaultTTL time.Duration
    EnableTTLJitter bool
    TTLJitterPct    float64 // for example 0.10 => +/-10% jitter, valid range [0.0, 0.5]

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
```

### 3.2 Public Interface Definitions

```go
// cache.go
type Cache interface {
    Get(ctx context.Context, key Key, dest any) error
    Set(ctx context.Context, key Key, value any, ttl time.Duration) error
    Delete(ctx context.Context, keys ...Key) error
    Exists(ctx context.Context, key Key) (bool, error)

    GetOrSet(
        ctx context.Context,
        key Key,
        dest any,
        ttl time.Duration,
        loader func(context.Context) (any, error),
    ) error

    Close() error
}

type HealthChecker interface {
    Ping(ctx context.Context) error
}
```

```go
// codec.go
type Codec interface {
    Marshal(value any) ([]byte, error)
    Unmarshal(data []byte, dest any) error
    ContentType() string
}
```

### 3.3 Public Constructor and Helpers

```go
// redis/redis.go
func New(cfg cache.Config) (cache.Cache, error)
```

```go
// key_builder.go
func NewKeyBuilder(serviceName, keyPrefix string) (KeyBuilder, error)
func ValidateKey(key Key) error
```

### 3.4 Method-Level Semantics

#### `Get(ctx, key, dest) error`
- Input:
1. `dest` must be a writable target (typically pointer).
- Output:
1. `nil` on hit and successful decode.
2. `CodeNotFound` for cache miss.
3. `CodeInternal` for Redis or decode failure.

#### `Set(ctx, key, value, ttl) error`
- TTL resolution:
1. if `ttl > 0`, use `ttl`
2. else use `DefaultTTL`
3. if resolved TTL `<= 0`, return `CodeInvalidInput`
4. if TTL jitter is enabled, apply jitter before write
- Output:
1. `nil` on success
2. `CodeInternal` on serialize/redis failure

#### `Delete(ctx, keys...) error`
- No-op if no keys provided.
- Validates and builds all keys before executing delete.
- If any key is invalid, returns validation error and performs no delete.

#### `Exists(ctx, key) (bool, error)`
- Returns `true` if key exists, else `false`.
- Redis failure returns `CodeInternal`.

#### `GetOrSet(ctx, key, dest, ttl, loader) error`
- Loader called only on miss or fallback conditions.
- If loader succeeds, module tries to cache value and returns loader result even when cache set fails.
- Loader failure returns `CodeInternal` wrapping loader cause.
- TTL handling is identical to `Set` (same resolution and validation rules).

#### `Close() error`
- Closes underlying Redis client.

#### `Ping(ctx) error`
- Returns `nil` if backend reachable.
- Returns `CodeInternal` wrapped Redis error on failure.

Health-check interface note:
- `Ping` is intentionally not part of the minimal `Cache` interface.
- Redis implementation satisfies both `cache.Cache` and `cache.HealthChecker`.

---

## 4. Integration Spec

### 4.1 Service Bootstrap Contract
1. Parse service config/env into `cache.Config`.
2. Create cache instance once at startup via `redis.New(cfg)`.
3. Pass around as `cache.Cache` interface.
4. Close at shutdown.

### 4.1.1 Config Validation and Defaults Contract
- Mode default:
1. Empty `Mode` is treated as `standalone`.
- Address requirements:
1. `standalone`: at least one address; first address is used.
2. `sentinel`: one or more sentinel addresses and `MasterName` required.
3. `cluster`: one or more node addresses required.
- Prefix precedence:
1. If `KeyPrefix` is set, it is used.
2. Else normalized `ServiceName` is used.
3. At least one of `KeyPrefix` or `ServiceName` must be non-empty after normalization.
- TTL behavior:
1. `Set` uses method TTL when `ttl > 0`.
2. Otherwise uses `DefaultTTL`.
3. If effective TTL is not positive, return `CodeInvalidInput`.
4. `GetOrSet` follows the exact same TTL rules.
- TTL ownership:
1. The library enforces TTL mechanics and validation.
2. Service teams own the business TTL decision per data type/use case.
- Non-expiring keys:
1. Currently does not allow immortal cache entries.
2. If effective TTL is not positive, write must fail with `CodeInvalidInput`.
- TTL jitter:
1. Optional.
2. Applies only after effective TTL is resolved.
3. Should keep TTL positive after jitter.
4. `TTLJitterPct` valid range: `0.0 <= TTLJitterPct <= 0.5`.
5. Effective TTL should be calculated as: `effectiveTTL * (1 ± random*jitterPct)`.
6. Final jittered TTL must be clamped to a positive duration.
- Timeout defaults (when unset):
1. `DialTimeout = 2s`
2. `ReadTimeout = 500ms`
3. `WriteTimeout = 500ms`
- Lock defaults (when stampede protection settings are unset):
1. `LockTTL = 3s`
2. `LockWaitTimeout = 2s`
3. `LockRetryInterval = 100ms`
- Cluster DB rule:
1. In `cluster` mode, `DB` must be `0`.

### 4.2 Example Initialization

```go
cfg := cache.Config{
    Mode:        "standalone",
    Addrs:       []string{"localhost:6379"},
    DB:          0,
    ServiceName: "user-service",
    DefaultTTL:  10 * time.Minute,

    EnableStampedeProtection: true,
    LockTTL:                  3 * time.Second,
    LockWaitTimeout:          2 * time.Second,
    LockRetryInterval:        100 * time.Millisecond,
}

c, err := redis.New(cfg)
if err != nil {
    return err
}
defer c.Close()

if hc, ok := c.(cache.HealthChecker); ok {
    if err := hc.Ping(ctx); err != nil {
        // mark service as degraded or fail readiness based on policy
    }
}
```

### 4.3 Example Read Path (`GetOrSet`)

```go
key := cache.Key{
    Domain:  "user",
    Entity:  "profile",
    ID:      userID,
    Version: "v1",
}

var user User
err := c.GetOrSet(ctx, key, &user, 10*time.Minute, func(ctx context.Context) (any, error) {
    return repo.GetByID(ctx, userID)
})
```

### 4.4 Example Write Path (DB + Invalidate)

```go
if err := repo.Update(ctx, user); err != nil {
    return err
}

key := cache.Key{Domain: "user", Entity: "profile", ID: user.ID, Version: "v1"}
if err := c.Delete(ctx, key); err != nil {
    // log and continue; DB remains source of truth
}
```

### 4.5 Integration with Common Modules
- `apperror` integration:
1. cache module wraps validation/backend/codec/loader errors with standard codes.
2. callers can use `apperror.CodeOf(err)` for handling logic.
- `logger` integration:
1. cache uses `logger.Subsystem("cache")`.
2. structured logs are emitted on backend/lock/set degradation paths.

### 4.6 Redis Mode Integration Details
- `standalone`:
1. use first `Addrs` entry as primary address.
2. `DB` respected.
- `sentinel`:
1. requires `MasterName` + `Addrs` as sentinel nodes.
2. `DB` respected.
- `cluster`:
1. requires one or more cluster node addresses.
2. must use `DB=0`.

### 4.7 Operational Guidelines
- Use moderate TTLs based on data volatility.
- Enable stampede protection for hot keys/high concurrency reads.
- Avoid using cache as source of truth.
- Treat Redis outages as degraded performance where business permits.
- Keep TTL jitter enabled for high-cardinality/hot domains to reduce synchronized expirations.
- Redis deployment must remain private/internal network only in current phase.
- Redis must not be directly reachable from public internet/external networks.

---

## 5. Future Scope

### 5.1 Observability Enhancements
- Expand baseline metrics into a richer observability contract:
1. `cache_get_total`
2. `cache_set_total`
3. `cache_delete_total`
4. per-operation latency histograms with finer buckets
5. lock-acquire/lock-timeout counters for stampede analysis
- Add tracing hooks for cache spans and loader spans.

### 5.2 Resilience and Fallback Controls
- Add configurable behavior for lock wait timeout:
1. strict timeout error
2. loader fallback (current behavior)
- Add optional circuit-breaker style protection for repeated Redis failures.

### 5.3 Serialization and Data Handling
- Support pluggable codecs beyond JSON (for example MsgPack/Protobuf).
- Add optional payload compression for large cache values.
- Add optional encryption for sensitive cache payloads.
- Add Redis TLS transport support for in-transit encryption and stronger network security.

### 5.4 TTL Strategy Improvements
- Add policy-driven TTL profiles by data category (profile/config/search/aggregate).

### 5.5 Negative Caching (Future)
- Add optional negative caching policy for `NOT_FOUND` loader results.
- Support short configurable TTL for negative entries.
- Keep feature opt-in and scoped to explicitly allowed domains/entities.
- Ensure negative cache entries are distinguishable from normal cached payloads.

### 5.6 Invalidation and Key Management
- Add first-class support for batch/group invalidation patterns.
- Define event-driven invalidation integration patterns for write-heavy domains.

### 5.7 Operational and Compliance Guidance
- Add service integration checklist for rollout readiness.
- Add conformance test guidelines to validate service-side usage against this spec.

### 5.8 Advanced Redis Operations (Optional)
- Add bulk APIs for high-throughput paths:
1. `MGet(ctx, keys...)`
2. `MSet(ctx, items, ttl)` (or equivalent batch set contract)
- Add counter APIs for quota/rate/statistics use cases:
1. `Incr(ctx, key)`
2. `IncrBy(ctx, key, delta)`
3. `Decr(ctx, key)`
4. `DecrBy(ctx, key, delta)`
- Add hash APIs for partial object operations:
1. `HGet(ctx, key, field)`
2. `HSet(ctx, key, fieldValues...)`
3. `HMGet(ctx, key, fields...)`
- Keep these capabilities as optional sub-interfaces so core cache consumers can continue using the minimal `Cache` interface.
