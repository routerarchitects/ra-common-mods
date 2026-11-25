package s3_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/routerarchitects/ra-common-mods/apperror"
	"github.com/routerarchitects/ra-common-mods/s3"
	"github.com/routerarchitects/ra-common-mods/s3/storage"
)

func TestS3Client_LiveIntegration(t *testing.T) {
	if os.Getenv("TEST_S3_INTEGRATION") != "true" {
		t.Skip("set TEST_S3_INTEGRATION=true to run the live backend test")
	}
	if os.Getenv("STORAGE_TYPE_S3_ENDPOINT") == "" {
		t.Fatal("set STORAGE_TYPE_S3_ENDPOINT to an isolated S3 test endpoint")
	}

	cfg, err := s3.DefaultConfig()
	if err != nil {
		t.Fatal(err)
	}
	client, err := s3.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	bucket := os.Getenv("TEST_S3_BUCKET")
	secondBucket := os.Getenv("TEST_S3_SECOND_BUCKET")
	createdBuckets := bucket == ""
	suffix := strings.ToLower(rand.Text())
	if createdBuckets {
		bucket = "s3-integration-" + suffix
		secondBucket = "s3-secondary-" + suffix
		for _, name := range []string{bucket, secondBucket} {
			if err := client.MakeBucket(ctx, name); err != nil {
				t.Fatalf("create test bucket %s: %v", name, err)
			}
			t.Cleanup(func() {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if exists, err := client.BucketExists(cleanupCtx, name); err != nil {
					t.Errorf("check test bucket %s during cleanup: %v", name, err)
				} else if exists {
					if err := client.RemoveBucket(cleanupCtx, name); err != nil {
						t.Errorf("remove test bucket %s: %v", name, err)
					}
				}
			})
		}
	}
	if secondBucket == bucket {
		t.Fatal("the two test buckets must be distinct")
	}

	prefix := "s3-tests/" + suffix + "/"
	object := prefix + "payload"
	spaced := "  " + prefix + "spaced  "
	bound, err := storage.NewScopedBucket(client, bucket)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{object, spaced} {
		cleanupObject(t, client, bucket, key)
	}
	if err := bound.Ping(ctx); err != nil {
		t.Fatalf("ping test bucket: %v", err)
	}

	// Exercise sequential and positioned multipart uploads with the same payload.
	const size = (12 << 20) + 37
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(repeatingReader{}, size)); err != nil {
		t.Fatal(err)
	}
	positioned := strings.NewReader("skip" + strings.Repeat("x", size))
	if _, err := positioned.Seek(4, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	for _, upload := range []struct {
		name   string
		reader io.Reader
		size   int64
	}{
		{"known length", io.LimitReader(repeatingReader{}, size), size},
		{"unknown length", io.LimitReader(repeatingReader{}, size), -1},
		{"positioned", positioned, size},
	} {
		if err := bound.Upload(ctx, object, upload.reader, upload.size, "application/octet-stream"); err != nil {
			t.Fatalf("upload %s stream: %v", upload.name, err)
		}
		stream, err := bound.Download(ctx, object)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.New()
		n, readErr := io.Copy(digest, stream)
		closeErr := stream.Close()
		if readErr != nil || closeErr != nil || n != size || !bytes.Equal(digest.Sum(nil), hash.Sum(nil)) {
			t.Fatalf("%s multipart round trip read %d bytes, read error %v, close error %v", upload.name, n, readErr, closeErr)
		}
	}
	if position, err := positioned.Seek(0, io.SeekCurrent); position != 4 || err != nil {
		t.Fatalf("positioned reader offset = %d, error=%v, want 4", position, err)
	}

	if err := bound.Upload(ctx, spaced, strings.NewReader("payload"), 7, "text/plain"); err != nil {
		t.Fatal(err)
	}
	assertExists(t, ctx, bound, spaced, true)
	assertExists(t, ctx, bound, strings.TrimSpace(spaced), false)
	spacedStream, err := bound.Download(ctx, spaced)
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(spacedStream)
	closeErr := spacedStream.Close()
	if string(data) != "payload" || readErr != nil || closeErr != nil {
		t.Fatalf("whitespace-sensitive key round trip returned %q, read error %v, close error %v", data, readErr, closeErr)
	}
	if err := bound.Delete(ctx, spaced); err != nil {
		t.Fatal(err)
	}

	assertExists(t, ctx, bound, object, true)
	info, err := bound.StatObject(ctx, object)
	if err != nil || info.Key != object || info.Size != size || info.ContentType != "application/octet-stream" ||
		info.LastModified.IsZero() || info.ETag == "" {
		t.Fatalf("stat returned %+v, err %v", info, err)
	}
	var objects []storage.ObjectSummary
	for summary, err := range bound.ListObjects(ctx, prefix) {
		if err != nil {
			t.Fatalf("list by prefix: %v", err)
		}
		objects = append(objects, summary)
	}
	if len(objects) != 1 || objects[0].Key != info.Key || objects[0].Size != info.Size || objects[0].ETag != info.ETag {
		t.Fatalf("list by prefix returned %+v; expected summary for %+v", objects, info.ObjectSummary)
	}

	if secondBucket != "" {
		secondary, err := storage.NewScopedBucket(client, secondBucket)
		if err != nil {
			t.Fatal(err)
		}
		cleanupObject(t, client, secondBucket, object)
		if err := secondary.Upload(ctx, object, strings.NewReader("other"), 5, "text/plain"); err != nil {
			t.Fatal(err)
		}
		if err := bound.Delete(ctx, object); err != nil {
			t.Fatal(err)
		}
		assertExists(t, ctx, secondary, object, true)
	} else if err := bound.Delete(ctx, object); err != nil {
		t.Fatal(err)
	}

	assertExists(t, ctx, bound, object, false)
	if _, err := bound.StatObject(ctx, object); apperror.CodeOf(err) != apperror.CodeNotFound {
		t.Fatalf("missing stat should fail with NOT_FOUND, got %v", err)
	}
	if err := bound.Delete(ctx, object); err != nil {
		t.Fatalf("repeat deletion: %v", err)
	}
	if stream, err := bound.Download(ctx, object); apperror.CodeOf(err) != apperror.CodeNotFound || stream != nil {
		if stream != nil {
			_ = stream.Close()
		}
		t.Fatalf("missing download should fail immediately with NOT_FOUND, got %v", err)
	}

	if createdBuckets {
		if err := client.RemoveBucket(ctx, bucket); err != nil {
			t.Fatal(err)
		}
		if exists, err := client.BucketExists(ctx, bucket); exists || err != nil {
			t.Fatalf("removed bucket unexpectedly accessible, exists=%v err=%v", exists, err)
		}
		if err := bound.Ping(ctx); apperror.CodeOf(err) != apperror.CodeNotFound {
			t.Fatalf("ping of missing bucket should fail with NOT_FOUND, got %v", err)
		}
		if exists, err := bound.Exists(ctx, object); exists || err == nil {
			t.Fatalf("missing bucket must surface as an error, exists=%v err=%v", exists, err)
		}
		if err := bound.Delete(ctx, object); err == nil {
			t.Fatal("deleting from a missing bucket should return an error")
		}
	}
}

func assertExists(t *testing.T, ctx context.Context, bucket storage.BucketStorage, key string, want bool) {
	t.Helper()
	if exists, err := bucket.Exists(ctx, key); err != nil || exists != want {
		t.Fatalf("Exists(%q) = %v, %v, want %v with no error", key, exists, err, want)
	}
}

func cleanupObject(t *testing.T, client *s3.Client, bucket, key string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		exists, err := client.BucketExists(ctx, bucket)
		if err != nil {
			t.Errorf("check test bucket during cleanup: %v", err)
		} else if exists {
			if err := client.Delete(ctx, bucket, key); err != nil {
				t.Errorf("clean up test object %s: %v", key, err)
			}
		}
	})
}

type repeatingReader struct{}

func (repeatingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}
