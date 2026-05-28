package cache

import "testing"

func TestBuildKey(t *testing.T) {
	b, err := NewKeyBuilder("User-Service", "")
	if err != nil {
		t.Fatalf("NewKeyBuilder failed: %v", err)
	}
	key, err := b.Build(Key{Domain: "user", Entity: "profile", ID: "123", Version: "v1"})
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}
	if key != "user-service:user:profile:123:v1" {
		t.Fatalf("unexpected key: %s", key)
	}
}

func TestBuildKeyDefaultsVersion(t *testing.T) {
	b, err := NewKeyBuilder("User-Service", "")
	if err != nil {
		t.Fatalf("NewKeyBuilder failed: %v", err)
	}
	key, err := b.Build(Key{Domain: "user", Entity: "profile", ID: "123"})
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}
	if key != "user-service:user:profile:123:v1" {
		t.Fatalf("unexpected key: %s", key)
	}
}

func TestBuildKeyWithQualifiersAndLockKey(t *testing.T) {
	b, err := NewKeyBuilder("catalog-service", "")
	if err != nil {
		t.Fatalf("NewKeyBuilder failed: %v", err)
	}
	key, err := b.Build(Key{Domain: "product", Entity: "recommendation", ID: "123", Qualifiers: []string{"region", "IN", "lang", "en"}, Version: "v1"})
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}
	if key != "catalog-service:product:recommendation:123:region:in:lang:en:v1" {
		t.Fatalf("unexpected key: %s", key)
	}

	lock, err := b.LockKey(Key{Domain: "product", Entity: "recommendation", ID: "123", Version: "v1"})
	if err != nil {
		t.Fatalf("LockKey failed: %v", err)
	}
	if lock != "catalog-service:__cache_lock__:product:recommendation:123:v1" {
		t.Fatalf("unexpected lock key: %s", lock)
	}
}

func TestValidateKey(t *testing.T) {
	if err := ValidateKey(Key{Domain: "", Entity: "b", ID: "1", Version: "v1"}); err == nil {
		t.Fatal("expected invalid domain")
	}
	if err := ValidateKey(Key{Domain: "a", Entity: "", ID: "1", Version: "v1"}); err == nil {
		t.Fatal("expected invalid entity")
	}
	if err := ValidateKey(Key{Domain: "a", Entity: "b", ID: "", Version: "v1"}); err == nil {
		t.Fatal("expected invalid id")
	}
	if err := ValidateKey(Key{Domain: "a:b", Entity: "b", ID: "1", Version: "v1"}); err == nil {
		t.Fatal("expected invalid domain with colon")
	}
	if err := ValidateKey(Key{Domain: "a__b", Entity: "b", ID: "1", Version: "v1"}); err == nil {
		t.Fatal("expected invalid domain with double underscore")
	}
	if err := ValidateKey(Key{Domain: "a", Entity: "b", ID: "1", Version: "v1", Qualifiers: []string{"ok", "ba:d"}}); err == nil {
		t.Fatal("expected invalid qualifier with colon")
	}
	if err := ValidateKey(Key{Domain: "a", Entity: "b", ID: "1", Version: "v1", Qualifiers: []string{"bad__value"}}); err == nil {
		t.Fatal("expected invalid qualifier with double underscore")
	}
}
