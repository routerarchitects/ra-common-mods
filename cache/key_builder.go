package cache

import (
	"strings"
)

const defaultKeyVersion = "v1"

type KeyBuilder struct {
	prefix string
}

func NewKeyBuilder(serviceName, keyPrefix string) (KeyBuilder, error) {
	prefix := normalizeSegment(keyPrefix)
	if prefix == "" {
		prefix = normalizeSegment(serviceName)
	}
	if prefix == "" {
		return KeyBuilder{}, errInvalidConfig("service name or key prefix must be set", nil)
	}
	if err := validateSegment(prefix, "prefix"); err != nil {
		return KeyBuilder{}, err
	}
	return KeyBuilder{prefix: prefix}, nil
}

func (b KeyBuilder) Build(key Key) (string, error) {
	if err := ValidateKey(key); err != nil {
		return "", err
	}

	segments := []string{
		b.prefix,
		normalizeSegment(key.Domain),
		normalizeSegment(key.Entity),
		normalizeSegment(key.ID),
	}

	for _, q := range key.Qualifiers {
		s := normalizeSegment(q)
		if s == "" {
			continue
		}
		segments = append(segments, s)
	}
	segments = append(segments, resolveVersion(key.Version))
	return strings.Join(segments, ":"), nil
}

func (b KeyBuilder) LockKey(key Key) (string, error) {
	full, err := b.Build(key)
	if err != nil {
		return "", err
	}
	return b.prefix + ":__cache_lock__:" + strings.TrimPrefix(full, b.prefix+":"), nil
}

func ValidateKey(key Key) error {
	domain := normalizeSegment(key.Domain)
	entity := normalizeSegment(key.Entity)
	id := normalizeSegment(key.ID)
	version := resolveVersion(key.Version)

	if domain == "" {
		return errInvalidKey("domain is required", map[string]any{"field": "domain"})
	}
	if entity == "" {
		return errInvalidKey("entity is required", map[string]any{"field": "entity"})
	}
	if id == "" {
		return errInvalidKey("id is required", map[string]any{"field": "id"})
	}
	if err := validateSegment(domain, "domain"); err != nil {
		return err
	}
	if err := validateSegment(entity, "entity"); err != nil {
		return err
	}
	if err := validateSegment(id, "id"); err != nil {
		return err
	}
	if err := validateSegment(version, "version"); err != nil {
		return err
	}
	for i, q := range key.Qualifiers {
		segment := normalizeSegment(q)
		if segment == "" {
			continue
		}
		if err := validateSegment(segment, "qualifiers"); err != nil {
			return errInvalidKey("invalid qualifier segment", map[string]any{"field": "qualifiers", "index": i, "value": segment, "cause": err.Error()})
		}
	}
	return nil
}

func normalizeSegment(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, ":")
	s = strings.ReplaceAll(s, " ", "-")
	s = strings.ToLower(s)
	return s
}

func resolveVersion(version string) string {
	v := normalizeSegment(version)
	if v == "" {
		return defaultKeyVersion
	}
	return v
}

func validateSegment(segment, field string) error {
	if strings.Contains(segment, ":") {
		return errInvalidKey("invalid cache key segment", map[string]any{"field": field, "reason": "contains colon"})
	}
	if strings.Contains(segment, "__") {
		return errInvalidKey("invalid cache key segment", map[string]any{"field": field, "reason": "contains reserved double underscore"})
	}
	for _, r := range segment {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			continue
		}
		return errInvalidKey("invalid cache key segment", map[string]any{"field": field, "reason": "contains unsupported character"})
	}
	return nil
}
