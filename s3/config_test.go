package s3_test

import (
	"math"
	"strconv"
	"testing"

	"github.com/routerarchitects/ra-common-mods/apperror"
	"github.com/routerarchitects/ra-common-mods/s3"
)

func TestLoadConfig(t *testing.T) {
	defaults, err := s3.LoadConfig("RA_S3_UNSET_")
	if err != nil {
		t.Fatal(err)
	}
	if defaults.Endpoint != "https://127.0.0.1:9000" || defaults.Region != "us-east-1" || !defaults.ForcePathStyle ||
		defaults.MaxConcurrentUploads != 4 || defaults.PartSize != 5<<20 || defaults.MaxUploadBufferBytes != 128<<20 || defaults.AccessKey != "" || defaults.SecretKey != "" {
		t.Fatal("unexpected S3 defaults")
	}

	t.Setenv("STORAGE_TYPE_S3_ENDPOINT", "https://primary.example.com")
	t.Setenv("STORAGE_TYPE_S3_ACCESS_KEY", "primary")
	t.Setenv("STORAGE_TYPE_S3_SECRET_KEY", "primary-secret")

	t.Setenv("ARCHIVE_S3_ENDPOINT", "http://archive.example.com:9000")
	t.Setenv("ARCHIVE_S3_ACCESS_KEY", "archive")
	t.Setenv("ARCHIVE_S3_SECRET_KEY", "archive-secret")
	t.Setenv("ARCHIVE_S3_FORCE_PATH_STYLE", "false")
	t.Setenv("ARCHIVE_S3_MAX_CONCURRENT_UPLOADS", "2")
	t.Setenv("ARCHIVE_S3_PART_SIZE", "6291456")
	t.Setenv("ARCHIVE_S3_MAX_UPLOAD_BUFFER_BYTES", "12582912")

	primary, err := s3.DefaultConfig()
	if err != nil || primary.Endpoint != "https://primary.example.com" || primary.AccessKey != "primary" {
		t.Fatalf("default prefix was not applied: %v", err)
	}
	implicit, err := s3.LoadConfig("")
	if err != nil || implicit != primary {
		t.Fatalf("empty prefix did not select the default: %v", err)
	}
	archive, err := s3.LoadConfig("ARCHIVE_S3_")
	if err != nil {
		t.Fatal(err)
	}
	if archive.Endpoint != "http://archive.example.com:9000" || archive.AccessKey != "archive" || archive.SecretKey != "archive-secret" ||
		archive.ForcePathStyle || archive.MaxConcurrentUploads != 2 || archive.PartSize != 6<<20 || archive.MaxUploadBufferBytes != 12<<20 {
		t.Fatal("custom prefix settings were not applied")
	}
}

func TestUploadBufferBudget(t *testing.T) {
	const partSize = 6 << 20
	cfg := s3.Config{Endpoint: "https://storage.example.com", PartSize: partSize, MaxConcurrentUploads: 2}

	cfg.MaxUploadBufferBytes = 2*partSize - 1
	if _, err := s3.NewClient(cfg); apperror.CodeOf(err) != apperror.CodeInvalidInput {
		t.Fatalf("expected rejection below the required budget, got %v", err)
	}
	cfg.MaxUploadBufferBytes++
	if _, err := s3.NewClient(cfg); err != nil {
		t.Fatalf("exact budget should be accepted: %v", err)
	}

	cfg.MaxUploadBufferBytes = 0
	cfg.MaxConcurrentUploads = 22
	if err := cfg.Validate(); apperror.CodeOf(err) != apperror.CodeInvalidInput {
		t.Fatalf("expected default budget to reject 132 MiB, got %v", err)
	}
	if strconv.IntSize == 64 {
		cfg.MaxUploadBufferBytes = math.MaxUint64
		cfg.MaxConcurrentUploads = math.MaxInt
		if err := cfg.Validate(); apperror.CodeOf(err) != apperror.CodeInvalidInput {
			t.Fatalf("overflowing upload buffer requirement was accepted: %v", err)
		}
	}
}

func TestConfigRejectsInvalidUploadSettings(t *testing.T) {
	for _, tc := range []struct {
		name  string
		key   string
		value string
	}{
		{"negative concurrency", "MAX_CONCURRENT_UPLOADS", "-1"},
		{"malformed concurrency", "MAX_CONCURRENT_UPLOADS", "many"},
		{"negative part size", "PART_SIZE", "-1"},
		{"part below minimum", "PART_SIZE", "5242879"},
		{"part above maximum", "PART_SIZE", "5368709121"},
		{"negative budget", "MAX_UPLOAD_BUFFER_BYTES", "-1"},
		{"budget below defaults", "MAX_UPLOAD_BUFFER_BYTES", "20971519"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CONFIG_TEST_S3_"+tc.key, tc.value)
			_, err := s3.LoadConfig("CONFIG_TEST_S3_")
			if apperror.CodeOf(err) != apperror.CodeInvalidInput {
				t.Fatalf("expected invalid configuration error, got %v", err)
			}
		})
	}

	cfg := s3.Config{Endpoint: "http://localhost:9000", MaxConcurrentUploads: -1}
	_, err := s3.NewClient(cfg)
	if apperror.CodeOf(err) != apperror.CodeInvalidInput {
		t.Fatalf("expected invalid client configuration error, got %v", err)
	}
}
