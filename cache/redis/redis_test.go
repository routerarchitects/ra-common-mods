package redis

import (
	"context"
	"testing"
	"time"

	"github.com/routerarchitects/ra-common-mods/apperror"
	"github.com/routerarchitects/ra-common-mods/cache"
)

func TestValidateConfig(t *testing.T) {
	if err := validateConfig(cache.Config{}); apperror.CodeOf(err) != apperror.CodeInvalidInput {
		t.Fatalf("expected invalid input, got: %v", err)
	}
	if err := validateConfig(cache.Config{ServiceName: "svc", Addrs: []string{"localhost:6379"}}); err != nil {
		t.Fatalf("expected valid config, got: %v", err)
	}
	if err := validateConfig(cache.Config{ServiceName: "svc", Addrs: []string{"localhost:6379"}, TTLJitterPct: 0.6}); apperror.CodeOf(err) != apperror.CodeInvalidInput {
		t.Fatalf("expected invalid input for jitter percent, got: %v", err)
	}
	if err := validateConfig(cache.Config{
		ServiceName:              "svc",
		Addrs:                    []string{"localhost:6379"},
		EnableStampedeProtection: true,
		LockTTL:                  time.Millisecond,
	}); apperror.CodeOf(err) != apperror.CodeInvalidInput {
		t.Fatalf("expected invalid input for lock ttl too small, got: %v", err)
	}
}

func TestClusterDBValidation(t *testing.T) {
	_, err := newClient(cache.Config{Mode: "cluster", Addrs: []string{"127.0.0.1:6379"}, DB: 1})
	if apperror.CodeOf(err) != apperror.CodeInvalidInput {
		t.Fatalf("expected invalid input for cluster db!=0, got: %v", err)
	}
}

func TestApplyDefaultsForLockConfig(t *testing.T) {
	cfg := applyDefaults(cache.Config{})
	if cfg.LockTTL != 3*time.Second {
		t.Fatalf("unexpected LockTTL default: %v", cfg.LockTTL)
	}
	if cfg.LockWaitTimeout != 2*time.Second {
		t.Fatalf("unexpected LockWaitTimeout default: %v", cfg.LockWaitTimeout)
	}
	if cfg.LockRetryInterval != 100*time.Millisecond {
		t.Fatalf("unexpected LockRetryInterval default: %v", cfg.LockRetryInterval)
	}
}

func TestLoadAndFillRejectsNilLoader(t *testing.T) {
	builder, err := cache.NewKeyBuilder("svc", "")
	if err != nil {
		t.Fatalf("builder error: %v", err)
	}
	c := &Cache{
		cfg:     cache.Config{DefaultTTL: time.Minute},
		codec:   cache.JSONCodec{},
		builder: builder,
	}

	var dest map[string]any
	err = c.loadAndFill(context.Background(), cache.Key{Domain: "a", Entity: "b", ID: "1"}, &dest, time.Minute, nil)
	if apperror.CodeOf(err) != apperror.CodeInvalidInput {
		t.Fatalf("expected invalid input for nil loader, got: %v", err)
	}
}

func TestLoadAndFillReturnsInvalidTTL(t *testing.T) {
	builder, err := cache.NewKeyBuilder("svc", "")
	if err != nil {
		t.Fatalf("builder error: %v", err)
	}
	c := &Cache{
		cfg:     cache.Config{DefaultTTL: 0},
		codec:   cache.JSONCodec{},
		builder: builder,
	}

	var dest map[string]any
	err = c.loadAndFill(
		context.Background(),
		cache.Key{Domain: "a", Entity: "b", ID: "1"},
		&dest,
		0,
		func(context.Context) (any, error) {
			return map[string]any{"ok": true}, nil
		},
	)
	if apperror.CodeOf(err) != apperror.CodeInvalidInput {
		t.Fatalf("expected invalid input from Set TTL validation, got: %v", err)
	}
}

func TestWaitForFillReturnsOnContextCancel(t *testing.T) {
	c := &Cache{
		cfg: cache.Config{
			LockWaitTimeout:   5 * time.Second,
			LockRetryInterval: 2 * time.Second,
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var dest any
	start := time.Now()
	err := c.waitForFill(ctx, "k", &dest, time.Now())
	if err == nil {
		t.Fatal("expected context cancellation error")
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Fatalf("waitForFill should return quickly on cancellation, took %v", time.Since(start))
	}
}

func TestGetOrSetRejectsInvalidTTLBeforeLoader(t *testing.T) {
	c := &Cache{
		cfg: cache.Config{DefaultTTL: 0},
	}

	called := false
	err := c.GetOrSet(
		context.Background(),
		cache.Key{},
		new(any),
		0,
		func(context.Context) (any, error) {
			called = true
			return map[string]any{"ok": true}, nil
		},
	)
	if apperror.CodeOf(err) != apperror.CodeInvalidInput {
		t.Fatalf("expected invalid input for effective TTL, got: %v", err)
	}
	if called {
		t.Fatal("loader should not be called when effective TTL is invalid")
	}
}
