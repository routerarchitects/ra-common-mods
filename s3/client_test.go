package s3_test

import (
	"context"
	"strings"
	"testing"

	"github.com/routerarchitects/ra-common-mods/apperror"
	"github.com/routerarchitects/ra-common-mods/s3"
)

func TestNewClient_Success(t *testing.T) {
	tests := []struct {
		name string
		cfg  s3.Config
	}{
		{
			name: "Standard MinIO localhost configuration",
			cfg: s3.Config{
				Endpoint:       "http://127.0.0.1:9000",
				AccessKey:      "minioadmin",
				SecretKey:      "minioadmin",
				Region:         "us-east-1",
				ForcePathStyle: true,
			},
		},
		{
			name: "AWS S3 Virtual Host Style with SSL",
			cfg: s3.Config{
				Endpoint:       "https://s3.us-east-1.amazonaws.com",
				AccessKey:      "AKIAIOSFODNN7EXAMPLE",
				SecretKey:      "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
				Region:         "us-east-1",
				ForcePathStyle: false,
			},
		},
		{
			name: "Endpoint normalization with protocol and trailing slash",
			cfg: s3.Config{
				Endpoint:       "https://s3.us-west-2.amazonaws.com/",
				AccessKey:      "AKIAIOSFODNN7EXAMPLE",
				SecretKey:      "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
				Region:         "us-west-2",
				ForcePathStyle: false,
			},
		},
		{
			name: "Endpoint normalization with HTTP protocol and trailing slash",
			cfg: s3.Config{
				Endpoint:       "http://127.0.0.1:9000/",
				AccessKey:      "minioadmin",
				SecretKey:      "minioadmin",
				Region:         "us-east-1",
				ForcePathStyle: true,
			},
		},
		{
			name: "IAM credentials fallback when keys are omitted",
			cfg: s3.Config{
				Endpoint:       "https://s3.us-east-1.amazonaws.com",
				AccessKey:      "",
				SecretKey:      "",
				Region:         "us-east-1",
				ForcePathStyle: false,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client, err := s3.NewClient(tc.cfg)
			if err != nil {
				t.Fatalf("expected no error initializing client, got: %v", err)
			}
			if client == nil {
				t.Fatal("expected non-nil client instance")
			}
			if want := strings.TrimRight(tc.cfg.Endpoint, "/"); client.Endpoint() != want {
				t.Errorf("Endpoint() = %q, want %q", client.Endpoint(), want)
			}
		})
	}
}

func TestNewClient_ValidationErrors(t *testing.T) {
	for _, endpoint := range []string{
		"", "   ", "storage.example.com", "storage.example.com:9000",
		"://storage.example.com", "ftp://storage.example.com", "https://",
		"https:storage.example.com", "http:///storage.example.com",
		"https://storage.example.com:invalid", "https://storage.example.com:",
		"https://storage.example.com:0", "https://storage.example.com:65536",
		"https://[hostname]:9000", "https://::1:9000", "https://[::1",
		"https://user:password@storage.example.com", "https://storage.example.com/s3",
		"https://storage.example.com/%2F", "https://storage.example.com?",
		"https://storage.example.com?operation=s3", "https://storage.example.com#",
	} {
		t.Run(endpoint, func(t *testing.T) {
			cfg := s3.Config{Endpoint: endpoint}
			client, err := s3.NewClient(cfg)
			if client != nil || apperror.CodeOf(err) != apperror.CodeInvalidInput {
				t.Fatalf("NewClient(%q) returned client=%v error=%v", endpoint, client, err)
			}
		})
	}

	for _, keys := range [][2]string{{"access", ""}, {"", "secret"}, {"access", "  "}, {"  ", "secret"}} {
		cfg := s3.Config{Endpoint: "https://storage.example.com", AccessKey: keys[0], SecretKey: keys[1]}
		client, err := s3.NewClient(cfg)
		if client != nil || apperror.CodeOf(err) != apperror.CodeInvalidInput || !strings.Contains(err.Error(), "credentials incomplete") {
			t.Fatalf("expected incomplete credentials error, got %v", err)
		}
	}
}

func TestClient_MethodBucketValidation(t *testing.T) {
	cfg := s3.Config{
		Endpoint:       "http://127.0.0.1:9000",
		AccessKey:      "minioadmin",
		SecretKey:      "minioadmin",
		Region:         "us-east-1",
		ForcePathStyle: true,
	}

	client, err := s3.NewClient(cfg)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	ctx := context.Background()

	t.Run("Upload rejects empty bucket", func(t *testing.T) {
		err := client.Upload(ctx, "", "test.bin", strings.NewReader("data"), 4, "text/plain")
		if err == nil || !strings.Contains(err.Error(), "bucket name must not be empty") {
			t.Errorf("expected empty bucket error, got: %v", err)
		}
	})

	t.Run("Upload rejects empty objectName", func(t *testing.T) {
		err := client.Upload(ctx, "my-bucket", "", strings.NewReader("data"), 4, "text/plain")
		if err == nil || !strings.Contains(err.Error(), "object name must not be empty") {
			t.Errorf("expected empty objectName error, got: %v", err)
		}
	})

	t.Run("Upload rejects whitespace-only objectName", func(t *testing.T) {
		err := client.Upload(ctx, "my-bucket", "   \t\n   ", strings.NewReader("data"), 4, "text/plain")
		if err == nil || !strings.Contains(err.Error(), "object name must not be empty") {
			t.Errorf("expected whitespace-only objectName error, got: %v", err)
		}
	})

	t.Run("Download rejects empty bucket", func(t *testing.T) {
		_, err := client.Download(ctx, "", "test.bin")
		if err == nil || !strings.Contains(err.Error(), "bucket name must not be empty") {
			t.Errorf("expected empty bucket error, got: %v", err)
		}
	})

	t.Run("Download rejects empty objectName", func(t *testing.T) {
		_, err := client.Download(ctx, "my-bucket", "")
		if err == nil || !strings.Contains(err.Error(), "object name must not be empty") {
			t.Errorf("expected empty objectName error, got: %v", err)
		}
	})

	t.Run("Download rejects whitespace-only objectName", func(t *testing.T) {
		_, err := client.Download(ctx, "my-bucket", "   \t\n   ")
		if err == nil || !strings.Contains(err.Error(), "object name must not be empty") {
			t.Errorf("expected whitespace-only objectName error, got: %v", err)
		}
	})

	t.Run("Delete rejects empty bucket", func(t *testing.T) {
		err := client.Delete(ctx, "", "test.bin")
		if err == nil || !strings.Contains(err.Error(), "bucket name must not be empty") {
			t.Errorf("expected empty bucket error, got: %v", err)
		}
	})

	t.Run("Delete rejects empty objectName", func(t *testing.T) {
		err := client.Delete(ctx, "my-bucket", "")
		if err == nil || !strings.Contains(err.Error(), "object name must not be empty") {
			t.Errorf("expected empty objectName error, got: %v", err)
		}
	})

	t.Run("Delete rejects whitespace-only objectName", func(t *testing.T) {
		err := client.Delete(ctx, "my-bucket", "   \t\n   ")
		if err == nil || !strings.Contains(err.Error(), "object name must not be empty") {
			t.Errorf("expected whitespace-only objectName error, got: %v", err)
		}
	})

	t.Run("Exists rejects empty bucket", func(t *testing.T) {
		_, err := client.Exists(ctx, "", "test.bin")
		if err == nil || !strings.Contains(err.Error(), "bucket name must not be empty") {
			t.Errorf("expected empty bucket error, got: %v", err)
		}
	})

	t.Run("Exists rejects empty objectName", func(t *testing.T) {
		_, err := client.Exists(ctx, "my-bucket", "")
		if err == nil || !strings.Contains(err.Error(), "object name must not be empty") {
			t.Errorf("expected empty objectName error, got: %v", err)
		}
	})

	t.Run("Exists rejects whitespace-only objectName", func(t *testing.T) {
		_, err := client.Exists(ctx, "my-bucket", "   \t\n   ")
		if err == nil || !strings.Contains(err.Error(), "object name must not be empty") {
			t.Errorf("expected whitespace-only objectName error, got: %v", err)
		}
	})

	t.Run("BucketExists rejects empty bucket", func(t *testing.T) {
		_, err := client.BucketExists(ctx, "")
		if err == nil || !strings.Contains(err.Error(), "bucket name must not be empty") {
			t.Errorf("expected empty bucket error, got: %v", err)
		}
	})

	t.Run("MakeBucket rejects empty bucket", func(t *testing.T) {
		err := client.MakeBucket(ctx, "")
		if err == nil || !strings.Contains(err.Error(), "bucket name must not be empty") {
			t.Errorf("expected empty bucket error, got: %v", err)
		}
	})

	t.Run("RemoveBucket rejects empty bucket", func(t *testing.T) {
		err := client.RemoveBucket(ctx, "")
		if err == nil || !strings.Contains(err.Error(), "bucket name must not be empty") {
			t.Errorf("expected empty bucket error, got: %v", err)
		}
	})

	t.Run("ListObjects rejects empty bucket", func(t *testing.T) {
		var count int
		for _, err := range client.ListObjects(ctx, "", "") {
			count++
			if apperror.CodeOf(err) != apperror.CodeInvalidInput {
				t.Errorf("expected empty bucket error, got: %v", err)
			}
		}
		if count != 1 {
			t.Fatalf("expected one error, got %d iterator yields", count)
		}
	})
}
