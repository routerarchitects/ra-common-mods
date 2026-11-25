package s3

import (
	"context"
	"fmt"
	"io"
	"iter"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/s3utils"
	"github.com/routerarchitects/ra-common-mods/apperror"
)

// Client wraps the MinIO SDK and provides multi-bucket object storage operations.
type Client struct {
	minioClient *minio.Client
	partSize    uint64
	uploadSlots chan struct{}
}

// NewClient creates an S3 client without contacting the storage endpoint.
func NewClient(cfg Config) (*Client, error) {
	return newClient(cfg, nil)
}

func newClient(cfg Config, transport http.RoundTripper) (*Client, error) {
	endpoint, err := cfg.validatedEndpoint()
	if err != nil {
		return nil, err
	}
	secure := endpoint.Scheme == "https"

	lookup := minio.BucketLookupAuto
	if cfg.ForcePathStyle {
		lookup = minio.BucketLookupPath
	}

	var creds *credentials.Credentials
	if strings.TrimSpace(cfg.AccessKey) == "" {
		creds = credentials.NewIAM("")
	} else {
		creds = credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, "")
	}

	region := strings.TrimSpace(cfg.Region)
	if region == "" {
		region = "us-east-1"
	}

	if transport == nil {
		transport, err = minio.DefaultTransport(secure)
		if err != nil {
			return nil, apperror.Wrap(apperror.CodeInternal, "failed to initialize S3 transport", err)
		}
	}
	client, err := minio.New(endpoint.String(), &minio.Options{
		Creds:        creds,
		Secure:       secure,
		Region:       region,
		BucketLookup: lookup,
		Transport:    responseTransport{transport},
	})
	if err != nil {
		return nil, apperror.Wrap(apperror.CodeInvalidInput, "failed to create S3 client", err)
	}

	cfg = cfg.withUploadDefaults()
	return &Client{
		minioClient: client,
		partSize:    cfg.PartSize,
		uploadSlots: make(chan struct{}, cfg.MaxConcurrentUploads),
	}, nil
}

func validateBucket(bucket string) error {
	if bucket == "" {
		return apperror.New(apperror.CodeInvalidInput, "bucket name must not be empty")
	}
	if err := s3utils.CheckValidBucketNameStrict(bucket); err != nil {
		return apperror.Wrap(apperror.CodeInvalidInput, "invalid bucket name", err)
	}
	return nil
}

func validateObject(bucket, key string) error {
	if err := validateBucket(bucket); err != nil {
		return err
	}
	if strings.TrimSpace(key) == "" {
		return apperror.New(apperror.CodeInvalidInput, "object name must not be empty")
	}
	if err := s3utils.CheckValidObjectName(key); err != nil {
		return apperror.Wrap(apperror.CodeInvalidInput, "invalid object name", err)
	}
	return nil
}

// contextReader honors cancellation between reads of a caller-owned stream.
type contextReader struct {
	ctx context.Context
	io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

// Upload streams from the reader's current position. Use -1 for an unknown size.
// Known-size readers with ReadAt and Seek keep their original position.
func (c *Client) Upload(ctx context.Context, bucket string, objectName string, reader io.Reader, objectSize int64, contentType string) error {
	if err := validateObject(bucket, objectName); err != nil {
		return err
	}
	if reader == nil || objectSize < -1 {
		return apperror.New(apperror.CodeInvalidInput, "upload requires a reader and a size of -1 or greater")
	}

	select {
	case c.uploadSlots <- struct{}{}:
		defer func() { <-c.uploadSlots }()
	case <-ctx.Done():
		return wrapS3Error("waiting for S3 upload slot", ctx.Err())
	}
	// Admission and cancellation can become ready together.
	if err := ctx.Err(); err != nil {
		return wrapS3Error("starting S3 upload", err)
	}

	if source, ok := reader.(interface {
		io.ReaderAt
		io.Seeker
	}); ok && objectSize >= 0 {
		offset, err := source.Seek(0, io.SeekCurrent)
		if err != nil {
			return wrapS3Error("failed to determine upload reader position", err)
		}
		if offset < 0 || objectSize > math.MaxInt64-offset {
			return apperror.New(apperror.CodeInvalidInput, "upload reader range is invalid")
		}
		// MinIO's multipart offsets are relative to the section's start.
		reader = io.NewSectionReader(source, offset, objectSize)
	} else {
		if objectSize >= 0 {
			reader = io.LimitReader(reader, objectSize)
		}
		reader = contextReader{ctx, reader}
	}
	opts := minio.PutObjectOptions{
		PartSize:              c.partSize,
		NumThreads:            1,
		ConcurrentStreamParts: false,
		ContentType:           contentType,
	}
	_, err := c.minioClient.PutObject(ctx, bucket, objectName, reader, objectSize, opts)
	if err != nil {
		return wrapS3Error(fmt.Sprintf("failed to upload object %q to bucket %q", objectName, bucket), err)
	}
	return nil
}

// Download starts an object GET and returns a caller-owned response stream.
func (c *Client) Download(ctx context.Context, bucket string, objectName string) (io.ReadCloser, error) {
	if err := validateObject(bucket, objectName); err != nil {
		return nil, err
	}

	getCtx, response := observeResponse(ctx, http.MethodGet)
	core := minio.Core{Client: c.minioClient}
	stream, _, _, err := core.GetObject(getCtx, bucket, objectName, minio.GetObjectOptions{})
	if err != nil {
		_, err = c.objectFailure(ctx, bucket, objectName, response, err, fmt.Sprintf("failed to download object %q from bucket %q", objectName, bucket))
		return nil, err
	}
	return downloadStream{stream}, nil
}

type downloadStream struct {
	io.ReadCloser
}

func (s downloadStream) Read(p []byte) (int, error) {
	n, err := s.ReadCloser.Read(p)
	if err != nil && err != io.EOF {
		return n, wrapS3Error("failed to read S3 object stream", err)
	}
	return n, err
}

func (s downloadStream) Close() error {
	if err := s.ReadCloser.Close(); err != nil {
		return wrapS3Error("failed to close S3 object stream", err)
	}
	return nil
}

// Delete removes an object. A confirmed missing object counts as a successful deletion.
func (c *Client) Delete(ctx context.Context, bucket string, objectName string) error {
	if err := validateObject(bucket, objectName); err != nil {
		return err
	}

	deleteCtx, response := observeResponse(ctx, http.MethodDelete)
	err := c.minioClient.RemoveObject(deleteCtx, bucket, objectName, minio.RemoveObjectOptions{})
	if err == nil {
		return nil
	}
	missing, err := c.objectFailure(ctx, bucket, objectName, response, err, fmt.Sprintf("failed to delete object %q from bucket %q", objectName, bucket))
	if missing {
		return nil
	}
	return err
}

// Exists checks if the specified object exists. Only confirmed object absence returns false with no error.
func (c *Client) Exists(ctx context.Context, bucket string, objectName string) (bool, error) {
	_, missing, err := c.statObject(ctx, bucket, objectName)
	if missing {
		return false, nil
	}
	return err == nil, err
}

// StatObject retrieves object metadata without opening a download stream.
func (c *Client) StatObject(ctx context.Context, bucket string, objectName string) (ObjectInfo, error) {
	object, _, err := c.statObject(ctx, bucket, objectName)
	if err != nil {
		return ObjectInfo{}, err
	}
	return ObjectInfo{
		ObjectSummary: summarizeObject(object),
		ContentType:   object.ContentType,
		Metadata:      object.Metadata,
	}, nil
}

func (c *Client) statObject(ctx context.Context, bucket string, objectName string) (minio.ObjectInfo, bool, error) {
	if err := validateObject(bucket, objectName); err != nil {
		return minio.ObjectInfo{}, false, err
	}

	statCtx, response := observeResponse(ctx, http.MethodHead)
	object, err := c.minioClient.StatObject(statCtx, bucket, objectName, minio.StatObjectOptions{})
	if err == nil {
		return object, false, nil
	}
	missing, err := c.objectFailure(ctx, bucket, objectName, response, err, fmt.Sprintf("failed to stat object %q in bucket %q", objectName, bucket))
	return minio.ObjectInfo{}, missing, err
}

// BucketExists checks if the specified bucket exists and is accessible.
func (c *Client) BucketExists(ctx context.Context, bucket string) (bool, error) {
	if err := validateBucket(bucket); err != nil {
		return false, err
	}

	exists, err := c.minioClient.BucketExists(ctx, bucket)
	if err != nil {
		return false, wrapS3Error(fmt.Sprintf("failed to check existence of bucket %q", bucket), err)
	}
	return exists, nil
}

// MakeBucket creates a new bucket in the S3 storage.
func (c *Client) MakeBucket(ctx context.Context, bucket string) error {
	if err := validateBucket(bucket); err != nil {
		return err
	}

	err := c.minioClient.MakeBucket(ctx, bucket, minio.MakeBucketOptions{})
	if err != nil {
		return wrapS3Error(fmt.Sprintf("failed to create bucket %q", bucket), err)
	}
	return nil
}

// RemoveBucket deletes a bucket from S3 storage.
func (c *Client) RemoveBucket(ctx context.Context, bucket string) error {
	if err := validateBucket(bucket); err != nil {
		return err
	}

	err := c.minioClient.RemoveBucket(ctx, bucket)
	if err != nil {
		return wrapS3Error(fmt.Sprintf("failed to remove bucket %q", bucket), err)
	}
	return nil
}

// ObjectSummary contains the object attributes returned by listing.
type ObjectSummary struct {
	Key          string
	Size         int64
	LastModified time.Time
	ETag         string
}

// ObjectInfo contains the summary and detailed metadata from StatObject.
type ObjectInfo struct {
	ObjectSummary
	ContentType string
	Metadata    http.Header
}

func summarizeObject(object minio.ObjectInfo) ObjectSummary {
	return ObjectSummary{
		Key:          object.Key,
		Size:         object.Size,
		LastModified: object.LastModified,
		ETag:         object.ETag,
	}
}

// ListObjects yields summaries matching prefix and stops after the first error
// or when the caller stops iterating.
func (c *Client) ListObjects(ctx context.Context, bucket string, prefix string) iter.Seq2[ObjectSummary, error] {
	return func(yield func(ObjectSummary, error) bool) {
		if err := validateBucket(bucket); err != nil {
			yield(ObjectSummary{}, err)
			return
		}
		if err := s3utils.CheckValidObjectNamePrefix(prefix); err != nil {
			yield(ObjectSummary{}, apperror.Wrap(apperror.CodeInvalidInput, "invalid object prefix", err))
			return
		}

		opts := minio.ListObjectsOptions{Prefix: prefix, Recursive: true}
		for object := range c.minioClient.ListObjectsIter(ctx, bucket, opts) {
			if err := ctx.Err(); err != nil {
				yield(ObjectSummary{}, listError(bucket, err))
				return
			}
			if object.Err != nil {
				yield(ObjectSummary{}, listError(bucket, object.Err))
				return
			}
			if !yield(summarizeObject(object), nil) {
				return
			}
		}
		// The SDK iterator may stop without yielding a cancellation error.
		if err := ctx.Err(); err != nil {
			yield(ObjectSummary{}, listError(bucket, err))
		}
	}
}

func listError(bucket string, err error) error {
	return wrapS3Error(fmt.Sprintf("failed to list objects in bucket %q", bucket), err)
}

// Ping checks S3 connectivity. If a bucket is supplied, it evaluates accessibility
// for that specific bucket without requiring account-wide s3:ListAllMyBuckets permissions.
// If bucket is empty, it evaluates general cluster reachability.
func (c *Client) Ping(ctx context.Context, bucket string) error {
	if bucket != "" {
		exists, err := c.BucketExists(ctx, bucket)
		if err != nil {
			return apperror.Wrap(apperror.CodeOf(err), fmt.Sprintf("s3 bucket %q readiness check failed", bucket), err)
		}
		if !exists {
			return apperror.New(apperror.CodeNotFound, fmt.Sprintf("s3 bucket %q does not exist", bucket))
		}
		return nil
	}

	// Fallback to cluster-level check if no bucket is specified
	_, err := c.minioClient.ListBuckets(ctx)
	if err != nil {
		return wrapS3Error("s3 storage ping failed", err)
	}
	return nil
}

// Endpoint returns the configured endpoint URL.
func (c *Client) Endpoint() string {
	return c.minioClient.EndpointURL().String()
}
