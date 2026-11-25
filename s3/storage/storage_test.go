package storage_test

import (
	"context"
	"io"
	"iter"
	"strings"
	"testing"

	"github.com/routerarchitects/ra-common-mods/apperror"
	"github.com/routerarchitects/ra-common-mods/s3"
	"github.com/routerarchitects/ra-common-mods/s3/storage"
)

// Compile-time assertion that *s3.Client satisfies storage.Storage interface
var _ storage.Storage = (*s3.Client)(nil)

// Compile-time assertion that *storage.ScopedBucket satisfies storage.BucketStorage interface
var _ storage.BucketStorage = (*storage.ScopedBucket)(nil)

func TestStorageInterfaceContract(t *testing.T) {
	cfg := s3.Config{
		Endpoint:       "http://127.0.0.1:9000",
		AccessKey:      "minioadmin",
		SecretKey:      "minioadmin",
		Region:         "us-east-1",
		ForcePathStyle: true,
	}

	client, err := s3.NewClient(cfg)
	if err != nil {
		t.Fatalf("failed to create s3 client: %v", err)
	}

	scoped, err := storage.NewScopedBucket(client, "archive-bucket")
	if err != nil || scoped == nil {
		t.Fatalf("expected a scoped bucket, got %v", err)
	}
	if scoped.BucketName() != "archive-bucket" {
		t.Errorf("expected bucket name 'archive-bucket', got: %s", scoped.BucketName())
	}
}

func TestNewScopedBucketValidatesLocally(t *testing.T) {
	// Any storage method on this value would panic.
	inert := &struct{ storage.Storage }{}
	var nilClient *s3.Client
	for _, tc := range []struct {
		name    string
		backend storage.Storage
		bucket  string
	}{
		{"nil storage", nil, "valid-bucket"},
		{"typed nil storage", nilClient, "valid-bucket"},
		{"empty bucket", inert, ""},
		{"whitespace bucket", inert, " "},
		{"bucket with uppercase", inert, "Invalid-Bucket"},
		{"bucket with surrounding spaces", inert, " valid-bucket "},
		{"bucket with path separator", inert, "invalid/bucket"},
		{"bucket with underscore", inert, "invalid_bucket"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scoped, err := storage.NewScopedBucket(tc.backend, tc.bucket)
			if scoped != nil || apperror.CodeOf(err) != apperror.CodeInvalidInput {
				t.Fatalf("expected an invalid input error, got bucket=%v error=%v", scoped, err)
			}
		})
	}
	scoped, err := storage.NewScopedBucket(inert, "valid.bucket")
	if err != nil || scoped == nil || scoped.BucketName() != "valid.bucket" {
		t.Fatalf("expected a locally constructed scoped bucket, got bucket=%v error=%v", scoped, err)
	}
}

// MockStorage implements storage.Storage for testing ScopedBucket delegation
type mockStorage struct {
	lastBucket     string
	lastObjectName string
	lastSize       int64
	uploadCalled   bool
	downloadCalled bool
	deleteCalled   bool
	existsCalled   bool
	statCalled     bool
	listCalled     bool
	pingCalled     bool
}

func (m *mockStorage) ListObjects(ctx context.Context, bucket string, prefix string) iter.Seq2[storage.ObjectSummary, error] {
	m.listCalled = true
	m.lastBucket = bucket
	m.lastObjectName = prefix
	return func(yield func(storage.ObjectSummary, error) bool) {
		yield(storage.ObjectSummary{Key: "manifest.json", Size: 123}, nil)
	}
}

func (m *mockStorage) StatObject(ctx context.Context, bucket string, objectName string) (storage.ObjectInfo, error) {
	m.statCalled = true
	m.lastBucket = bucket
	m.lastObjectName = objectName
	return storage.ObjectInfo{ObjectSummary: storage.ObjectSummary{Key: objectName}, ContentType: "application/json"}, nil
}

func (m *mockStorage) Upload(ctx context.Context, bucket string, objectName string, reader io.Reader, size int64, contentType string) error {
	m.uploadCalled = true
	m.lastBucket = bucket
	m.lastObjectName = objectName
	m.lastSize = size
	return nil
}

func (m *mockStorage) Download(ctx context.Context, bucket string, objectName string) (io.ReadCloser, error) {
	m.downloadCalled = true
	m.lastBucket = bucket
	m.lastObjectName = objectName
	return io.NopCloser(strings.NewReader("mock-data")), nil
}

func (m *mockStorage) Delete(ctx context.Context, bucket string, objectName string) error {
	m.deleteCalled = true
	m.lastBucket = bucket
	m.lastObjectName = objectName
	return nil
}

func (m *mockStorage) Exists(ctx context.Context, bucket string, objectName string) (bool, error) {
	m.existsCalled = true
	m.lastBucket = bucket
	m.lastObjectName = objectName
	return true, nil
}

func (m *mockStorage) BucketExists(ctx context.Context, bucket string) (bool, error) {
	return true, nil
}

func (m *mockStorage) MakeBucket(ctx context.Context, bucket string) error {
	return nil
}

func (m *mockStorage) RemoveBucket(ctx context.Context, bucket string) error {
	return nil
}

func (m *mockStorage) Ping(ctx context.Context, bucket string) error {
	m.pingCalled = true
	m.lastBucket = bucket
	return nil
}

func TestScopedBucket_Delegation(t *testing.T) {
	mock := &mockStorage{}
	bucketName := "custom-archive-bucket"
	scoped, err := storage.NewScopedBucket(mock, bucketName)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	// 1. Upload delegation
	err = scoped.Upload(ctx, "object.bin", strings.NewReader("content"), 7, "application/octet-stream")
	if err != nil {
		t.Fatalf("unexpected upload error: %v", err)
	}
	if !mock.uploadCalled || mock.lastBucket != bucketName || mock.lastObjectName != "object.bin" || mock.lastSize != 7 {
		t.Errorf("upload delegation mismatch: %+v", mock)
	}

	// 2. Download delegation
	rc, err := scoped.Download(ctx, "object.bin")
	if err != nil {
		t.Fatalf("unexpected download error: %v", err)
	}
	_ = rc.Close()
	if !mock.downloadCalled || mock.lastBucket != bucketName || mock.lastObjectName != "object.bin" {
		t.Errorf("download delegation mismatch: %+v", mock)
	}

	// 3. Exists delegation
	exists, err := scoped.Exists(ctx, "object.bin")
	if err != nil || !exists {
		t.Fatalf("unexpected exists result: %v, err: %v", exists, err)
	}
	if !mock.existsCalled || mock.lastBucket != bucketName || mock.lastObjectName != "object.bin" {
		t.Errorf("exists delegation mismatch: %+v", mock)
	}

	// 4. Delete delegation
	err = scoped.Delete(ctx, "object.bin")
	if err != nil {
		t.Fatalf("unexpected delete error: %v", err)
	}
	if !mock.deleteCalled || mock.lastBucket != bucketName || mock.lastObjectName != "object.bin" {
		t.Errorf("delete delegation mismatch: %+v", mock)
	}

	info, err := scoped.StatObject(ctx, "manifest.json")
	if err != nil || !mock.statCalled || mock.lastBucket != bucketName ||
		mock.lastObjectName != "manifest.json" || info.Key != "manifest.json" || info.ContentType != "application/json" {
		t.Fatalf("StatObject delegation mismatch: info=%+v storage=%+v error=%v", info, mock, err)
	}

	var objects []storage.ObjectSummary
	for object, err := range scoped.ListObjects(ctx, "manifests/") {
		if err != nil {
			t.Fatalf("unexpected list error: %v", err)
		}
		objects = append(objects, object)
	}
	if !mock.listCalled || mock.lastBucket != bucketName || mock.lastObjectName != "manifests/" ||
		len(objects) != 1 || objects[0].Key != "manifest.json" {
		t.Errorf("list delegation mismatch: %+v", mock)
	}

	err = scoped.Ping(ctx)
	if err != nil {
		t.Fatalf("unexpected ping error: %v", err)
	}
	if !mock.pingCalled || mock.lastBucket != bucketName {
		t.Errorf("ping delegation mismatch: %+v", mock)
	}
}
