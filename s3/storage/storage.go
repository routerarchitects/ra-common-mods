package storage

import (
	"context"
	"io"
	"iter"
	"reflect"

	"github.com/minio/minio-go/v7/pkg/s3utils"
	"github.com/routerarchitects/ra-common-mods/apperror"
	"github.com/routerarchitects/ra-common-mods/s3"
)

// ObjectInfo aliases the s3.ObjectInfo type for storage contracts.
type ObjectInfo = s3.ObjectInfo

// ObjectSummary aliases the s3.ObjectSummary type for storage contracts.
type ObjectSummary = s3.ObjectSummary

// Storage defines the multi-bucket contract for persisting, retrieving, and inspecting binary objects.
// Implementations can be S3/MinIO, local filesystem, or in-memory mock.
type Storage interface {
	Upload(ctx context.Context, bucket string, objectName string, reader io.Reader, objectSize int64, contentType string) error
	Download(ctx context.Context, bucket string, objectName string) (io.ReadCloser, error)
	Delete(ctx context.Context, bucket string, objectName string) error
	Exists(ctx context.Context, bucket string, objectName string) (bool, error)
	StatObject(ctx context.Context, bucket string, objectName string) (ObjectInfo, error)
	ListObjects(ctx context.Context, bucket string, prefix string) iter.Seq2[ObjectSummary, error]
	BucketExists(ctx context.Context, bucket string) (bool, error)
	MakeBucket(ctx context.Context, bucket string) error
	RemoveBucket(ctx context.Context, bucket string) error
	Ping(ctx context.Context, bucket string) error
}

// BucketStorage defines operations scoped to a specific pre-bound bucket.
type BucketStorage interface {
	Upload(ctx context.Context, objectName string, reader io.Reader, objectSize int64, contentType string) error
	Download(ctx context.Context, objectName string) (io.ReadCloser, error)
	Delete(ctx context.Context, objectName string) error
	Exists(ctx context.Context, objectName string) (bool, error)
	StatObject(ctx context.Context, objectName string) (ObjectInfo, error)
	ListObjects(ctx context.Context, prefix string) iter.Seq2[ObjectSummary, error]
	Ping(ctx context.Context) error
}

// ScopedBucket binds a multi-bucket Storage engine to a dedicated bucket name.
type ScopedBucket struct {
	storage    Storage
	bucketName string
}

// NewScopedBucket binds storage to a valid S3 bucket name. Use Ping to check
// whether the bucket exists and is accessible.
func NewScopedBucket(s Storage, bucketName string) (*ScopedBucket, error) {
	value := reflect.ValueOf(s)
	switch value.Kind() {
	case reflect.Invalid:
		return nil, apperror.New(apperror.CodeInvalidInput, "storage must not be nil")
	case reflect.Chan, reflect.Func, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return nil, apperror.New(apperror.CodeInvalidInput, "storage must not be nil")
		}
	}
	if err := s3utils.CheckValidBucketNameStrict(bucketName); err != nil {
		return nil, apperror.Wrap(apperror.CodeInvalidInput, "invalid bucket name", err)
	}
	return &ScopedBucket{
		storage:    s,
		bucketName: bucketName,
	}, nil
}

// BucketName returns the bound bucket name.
func (b *ScopedBucket) BucketName() string {
	return b.bucketName
}

// Upload delegates object upload to the underlying storage using the bound bucket.
func (b *ScopedBucket) Upload(ctx context.Context, objectName string, reader io.Reader, objectSize int64, contentType string) error {
	return b.storage.Upload(ctx, b.bucketName, objectName, reader, objectSize, contentType)
}

// Download delegates object download to the underlying storage using the bound bucket.
func (b *ScopedBucket) Download(ctx context.Context, objectName string) (io.ReadCloser, error) {
	return b.storage.Download(ctx, b.bucketName, objectName)
}

// Delete delegates object deletion to the underlying storage using the bound bucket.
func (b *ScopedBucket) Delete(ctx context.Context, objectName string) error {
	return b.storage.Delete(ctx, b.bucketName, objectName)
}

// Exists delegates object existence check to the underlying storage using the bound bucket.
func (b *ScopedBucket) Exists(ctx context.Context, objectName string) (bool, error) {
	return b.storage.Exists(ctx, b.bucketName, objectName)
}

// StatObject retrieves full metadata for an object in the bound bucket.
func (b *ScopedBucket) StatObject(ctx context.Context, objectName string) (ObjectInfo, error) {
	return b.storage.StatObject(ctx, b.bucketName, objectName)
}

// ListObjects delegates object enumeration to the underlying storage using the bound bucket.
func (b *ScopedBucket) ListObjects(ctx context.Context, prefix string) iter.Seq2[ObjectSummary, error] {
	return b.storage.ListObjects(ctx, b.bucketName, prefix)
}

// Ping delegates health/readiness check to the underlying storage for the bound bucket.
func (b *ScopedBucket) Ping(ctx context.Context) error {
	return b.storage.Ping(ctx, b.bucketName)
}
