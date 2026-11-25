package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"os"
	"path/filepath"
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

func TestUploadReadsFromCurrentPosition(t *testing.T) {
	const partSize = 5 << 20
	payload := make([]byte, partSize+64)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	filePath := filepath.Join(t.TempDir(), "object")
	if err := os.WriteFile(filePath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"bytes", "file"} {
		for _, tc := range []struct {
			name          string
			start         int64
			size          int64
			omitSeek      bool
			wantMultipart bool
		}{
			{name: "positioned single put", start: 11, size: 37},
			{name: "positioned multipart", start: 11, size: partSize + 17, wantMultipart: true},
			{name: "zero-offset multipart", size: partSize + 17, wantMultipart: true},
			{name: "reader-at without seek", start: 11, size: partSize + 17, omitSeek: true, wantMultipart: true},
			{name: "unknown size", start: 11, size: -1, wantMultipart: true},
		} {
			t.Run(source+"/"+tc.name, func(t *testing.T) {
				var reader interface {
					io.ReaderAt
					io.ReadSeeker
				} = bytes.NewReader(payload)
				if source == "file" {
					file, err := os.Open(filePath)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = file.Close() })
					reader = file
				}
				if _, err := reader.Seek(tc.start, io.SeekStart); err != nil {
					t.Fatal(err)
				}
				var input io.Reader = reader
				if tc.omitSeek {
					input = struct {
						io.Reader
						io.ReaderAt
					}{reader, reader}
				}

				var uploaded bytes.Buffer
				var multipart bool
				client := newProtocolTestClient(t, Config{}, handlerTransport(func(w http.ResponseWriter, r *http.Request) {
					query := r.URL.Query()
					switch {
					case r.Method == http.MethodPost && query.Has("uploads"):
						multipart = true
						_, _ = io.WriteString(w, "<InitiateMultipartUploadResult><UploadId>upload-id</UploadId></InitiateMultipartUploadResult>")
					case r.Method == http.MethodPut:
						if _, err := io.Copy(&uploaded, r.Body); err != nil {
							t.Errorf("read upload request: %v", err)
						}
						w.Header().Set("ETag", `"etag"`)
					case r.Method == http.MethodPost && query.Get("uploadId") == "upload-id":
						_, _ = io.WriteString(w, "<CompleteMultipartUploadResult><Bucket>test-bucket</Bucket><Key>object</Key><ETag>complete</ETag></CompleteMultipartUploadResult>")
					default:
						t.Errorf("unexpected upload request %s %s", r.Method, r.URL)
						w.WriteHeader(http.StatusBadRequest)
					}
				}))
				if err := client.Upload(t.Context(), "test-bucket", "object", input, tc.size, "application/octet-stream"); err != nil {
					t.Fatal(err)
				}
				end := int64(len(payload))
				if tc.size >= 0 {
					end = tc.start + tc.size
				}
				if multipart != tc.wantMultipart || !bytes.Equal(uploaded.Bytes(), payload[tc.start:end]) {
					t.Fatalf("unexpected bytes from offset %d, multipart=%v, size=%d", tc.start, multipart, uploaded.Len())
				}
				wantPosition := tc.start
				if tc.size < 0 || tc.omitSeek {
					wantPosition = end
				}
				if position, err := reader.Seek(0, io.SeekCurrent); position != wantPosition || err != nil {
					t.Fatalf("reader position = %d, error=%v, want %d", position, err, wantPosition)
				}
			})
		}
	}
}

type fixedUploadPosition struct {
	*bytes.Reader
	position int64
	err      error
}

func (r fixedUploadPosition) Seek(int64, int) (int64, error) { return r.position, r.err }

func TestUploadRejectsInvalidReaderRangeBeforeRequests(t *testing.T) {
	client := newProtocolTestClient(t, Config{}, handlerTransport(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("invalid reader reached the backend: %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusBadRequest)
	}))
	seekErr := errors.New("seek failed")
	for _, tc := range []struct {
		position int64
		cause    error
	}{
		{cause: seekErr},
		{position: -1},
		{position: math.MaxInt64},
	} {
		reader := fixedUploadPosition{bytes.NewReader([]byte("abc")), tc.position, tc.cause}
		err := client.Upload(t.Context(), "test-bucket", "object", reader, 3, "application/octet-stream")
		if tc.cause != nil {
			if !errors.Is(err, tc.cause) {
				t.Errorf("seek error was lost: %v", err)
			}
		} else if apperror.CodeOf(err) != apperror.CodeInvalidInput {
			t.Errorf("position %d returned %v, want invalid input", tc.position, err)
		}
	}
}

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
