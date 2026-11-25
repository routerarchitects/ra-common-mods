package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/routerarchitects/ra-common-mods/apperror"
)

func TestUploadPreservesStreamingValidationErrors(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusRequestEntityTooLarge} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			cause := errors.New("invalid upload stream")
			meta := map[string]any{"status": status, "field": "artifact"}
			validationErr := apperror.WrapWithMeta(apperror.CodeInvalidInput, "upload validation failed", cause, meta)
			reader := failedUploadReader{err: fmt.Errorf("reading body: %w", validationErr)}
			var aborted bool
			client := newProtocolTestClient(t, Config{}, handlerTransport(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost && r.URL.Query().Has("uploads"):
					_, _ = io.WriteString(w, "<InitiateMultipartUploadResult><UploadId>upload-id</UploadId></InitiateMultipartUploadResult>")
				case r.Method == http.MethodDelete && r.URL.Query().Get("uploadId") == "upload-id":
					aborted = true
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected upload request %s %s", r.Method, r.URL)
					w.WriteHeader(http.StatusBadRequest)
				}
			}))

			err := client.Upload(t.Context(), "test-bucket", "artifact", reader, -1, "application/octet-stream")
			var got *apperror.Error
			if !errors.As(err, &got) || got.Code() != validationErr.Code() || got.Message() != validationErr.Message() ||
				!maps.Equal(got.Meta(), meta) || !errors.Is(err, validationErr) || !errors.Is(err, cause) {
				t.Fatalf("upload did not preserve the streaming validation error: %v", err)
			}
			if !aborted {
				t.Fatal("multipart upload did not abort")
			}
		})
	}
}

type failedUploadReader struct{ err error }

func (r failedUploadReader) Read([]byte) (int, error) { return 0, r.err }

func TestMultipartUploadStreamsConfiguredParts(t *testing.T) {
	const partSize = 6 << 20
	const objectSize = 2*partSize + 17
	for _, size := range []int64{objectSize, -1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			reader := &generatedUpload{remaining: objectSize}
			var uploaded atomic.Int64
			var parts atomic.Int32
			transport := handlerTransport(func(w http.ResponseWriter, r *http.Request) {
				query := r.URL.Query()
				switch {
				case r.Method == http.MethodPost && query.Has("uploads"):
					_, _ = io.WriteString(w, "<InitiateMultipartUploadResult><UploadId>upload-id</UploadId></InitiateMultipartUploadResult>")
				case r.Method == http.MethodPut && query.Get("uploadId") == "upload-id":
					part := parts.Add(1)
					n, err := io.Copy(io.Discard, r.Body)
					wantSize := int64(partSize)
					if part == 3 {
						wantSize = 17
					}
					if n != wantSize || err != nil {
						t.Errorf("part %d sent %d bytes with %v, want %d bytes", part, n, err, wantSize)
					}
					total := uploaded.Add(n)
					if read := reader.read.Load(); read != total {
						t.Errorf("part %d sent after reading %d bytes, want %d", part, read, total)
					}
					w.Header().Set("ETag", `"part-etag"`)
				case r.Method == http.MethodPost && query.Get("uploadId") == "upload-id":
					_, _ = io.WriteString(w, "<CompleteMultipartUploadResult><Bucket>test-bucket</Bucket><Key>multipart</Key><ETag>complete</ETag></CompleteMultipartUploadResult>")
				default:
					t.Errorf("unexpected multipart request %s %s", r.Method, r.URL)
					w.WriteHeader(http.StatusBadRequest)
				}
			})
			client := newProtocolTestClient(t, Config{PartSize: partSize}, transport)
			if err := client.Upload(t.Context(), "test-bucket", "multipart", reader, size, "application/octet-stream"); err != nil {
				t.Fatal(err)
			}
			if n := parts.Load(); n != 3 || uploaded.Load() != objectSize {
				t.Fatalf("uploaded %d parts totaling %d bytes", n, uploaded.Load())
			}
		})
	}
}

type generatedUpload struct {
	remaining int64
	read      atomic.Int64
}

func (r *generatedUpload) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := int(min(int64(len(p)), r.remaining))
	clear(p[:n])
	r.remaining -= int64(n)
	r.read.Add(int64(n))
	return n, nil
}

func TestUploadLimitAndCancellation(t *testing.T) {
	entered := make(chan string, 4)
	release := make(chan struct{})
	transport := handlerTransport(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			return
		}
		entered <- r.URL.Path
		select {
		case <-r.Context().Done():
		case <-release:
			w.Header().Set("ETag", `"etag"`)
			w.WriteHeader(http.StatusOK)
		}
	})
	t.Cleanup(func() { close(release) })

	client := newProtocolTestClient(t, Config{MaxConcurrentUploads: 2}, transport)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	upload := func(ctx context.Context, bucket, key string) <-chan error {
		result := make(chan error, 1)
		go func() {
			result <- client.Upload(ctx, bucket, key, strings.NewReader("payload"), 7, "text/plain")
		}()
		return result
	}

	activeCtx, cancelActive := context.WithCancel(ctx)
	defer cancelActive()
	first := upload(activeCtx, "first-bucket", "first")
	second := upload(ctx, "second-bucket", "second")
	for range 2 {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("both upload slots did not reach the server")
		}
	}

	queuedCtx, cancelQueued := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancelQueued()
	queued := upload(queuedCtx, "third-bucket", "queued")
	select {
	case path := <-entered:
		t.Fatalf("upload %s exceeded the configured limit", path)
	case err := <-queued:
		if !errors.Is(err, context.DeadlineExceeded) || apperror.CodeOf(err) != apperror.CodeTimeout {
			t.Fatalf("queued cancellation returned %v", err)
		}
	case <-ctx.Done():
		t.Fatal("queued upload did not return on deadline")
	}

	cancelActive()
	select {
	case err := <-first:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("active cancellation returned %v", err)
		}
	case <-ctx.Done():
		t.Fatal("active upload did not return on cancellation")
	}

	replacementCtx, cancelReplacement := context.WithCancel(ctx)
	defer cancelReplacement()
	replacement := upload(replacementCtx, "third-bucket", "replacement")
	select {
	case path := <-entered:
		if path != "/third-bucket/replacement" {
			t.Fatalf("unexpected request after cancellation: %s", path)
		}
	case <-ctx.Done():
		t.Fatal("canceled upload did not release its slot")
	}
	cancelReplacement()
	cancel()
	for _, result := range []<-chan error{replacement, second} {
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("expected canceled upload, got %v", err)
		}
	}
}
