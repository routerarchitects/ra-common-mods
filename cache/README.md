# Cache Module

Common Redis cache module for Go services.

## Features

- Structured key model (`cache.Key`) with opaque final key generation.
- Shared prefixing by service name/key prefix.
- `Get`, `Set`, `Delete`, `Exists`, and `GetOrSet` APIs.
- Redis modes: `standalone`, `sentinel`, `cluster`.
- Optional stampede protection lock flow inside `GetOrSet`.
- Common error wrapping via `apperror`.
- Common logging via `logger`.

## Install

```bash
go get github.com/routerarchitects/ra-common-mods/cache
```

## Basic usage

```go
cfg := cache.Config{
	Mode:        "standalone",
	Addrs:       []string{"localhost:6379"},
	DB:          0,
	ServiceName: "user-service",
	DefaultTTL:  10 * time.Minute,
}

c, err := redis.New(cfg)
if err != nil {
	return err
}
defer c.Close()

key := cache.Key{Domain: "user", Entity: "profile", ID: "123"}

var user User
if err := c.GetOrSet(ctx, key, &user, 10*time.Minute, func(ctx context.Context) (any, error) {
	return repo.GetUser(ctx, "123")
}); err != nil {
	return err
}
```

## Notes

- Services should never build raw Redis keys directly.
- In cluster mode, `DB` must be `0`.
- Locking is internal to `GetOrSet`; plain `Get/Set/Delete/Exists` do not lock.
