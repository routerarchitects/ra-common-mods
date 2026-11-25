package s3

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/routerarchitects/ra-common-mods/apperror"
)

type trackedDownloadBody struct {
	io.Reader
	reads  int
	closed bool
}

func (b *trackedDownloadBody) Read(p []byte) (int, error) {
	b.reads++
	return b.Reader.Read(p)
}

func (b *trackedDownloadBody) Close() error {
	b.closed = true
	return nil
}

func successfulDownload(body io.ReadCloser, size int64) *http.Response {
	header := make(http.Header)
	header.Set("Content-Length", strconv.FormatInt(size, 10))
	header.Set("Last-Modified", "Wed, 25 Nov 2026 00:00:00 GMT")
	return &http.Response{StatusCode: http.StatusOK, Header: header, Body: body, ContentLength: size}
}

func TestDownloadStartsOneStreamingGet(t *testing.T) {
	for _, tc := range []struct {
		name       string
		size       int
		closeEarly bool
	}{
		{name: "empty"},
		{name: "large", size: (6 << 20) + 17},
		{name: "close early", size: 1024, closeEarly: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := strings.Repeat("x", tc.size)
			body := &trackedDownloadBody{Reader: strings.NewReader(payload)}
			var requests int
			client := newProtocolTestClient(t, Config{}, func(req *http.Request) (*http.Response, error) {
				requests++
				if req.Method != http.MethodGet || req.URL.Path != "/test-bucket/object" {
					t.Errorf("unexpected download request %s %s", req.Method, req.URL)
				}
				return successfulDownload(body, int64(tc.size)), nil
			})
			stream, err := client.Download(t.Context(), "test-bucket", "object")
			if err != nil {
				t.Fatal(err)
			}
			if requests != 1 || body.reads != 0 || body.closed {
				t.Errorf("Download made %d requests, read body %d times, closed=%v", requests, body.reads, body.closed)
			}
			if !tc.closeEarly {
				got, err := io.ReadAll(stream)
				if err != nil || string(got) != payload {
					t.Errorf("download got %d bytes, want %d, error=%v", len(got), tc.size, err)
				}
				if _, err := stream.Read(make([]byte, 1)); err != io.EOF {
					t.Errorf("final read returned %v, want EOF", err)
				}
			}
			if err := stream.Close(); err != nil || !body.closed || (tc.closeEarly && body.reads != 0) {
				t.Errorf("Close returned %v, body closed=%v, reads=%d", err, body.closed, body.reads)
			}
		})
	}
}

func TestDownloadPreservesErrorsAfterGet(t *testing.T) {
	for _, cause := range []error{context.Canceled, io.ErrUnexpectedEOF} {
		t.Run(cause.Error(), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var body *trackedDownloadBody
			client := newProtocolTestClient(t, Config{}, func(req *http.Request) (*http.Response, error) {
				if req.Method != http.MethodGet || req.URL.Path != "/test-bucket/object" {
					t.Errorf("unexpected request %s %s", req.Method, req.URL)
				}
				body = &trackedDownloadBody{Reader: io.MultiReader(strings.NewReader("abc"), failedUploadReader{err: io.ErrUnexpectedEOF})}
				if cause == context.Canceled {
					body.Reader = contextReader{req.Context(), body.Reader}
				}
				return successfulDownload(body, 7), nil
			})
			stream, err := client.Download(ctx, "test-bucket", "object")
			if err != nil {
				t.Fatal(err)
			}
			if cause == context.Canceled {
				cancel()
			}
			_, err = io.ReadAll(stream)
			if !errors.Is(err, cause) || apperror.CodeOf(err) != apperror.CodeInternal {
				t.Errorf("stream returned %v, expected wrapped %v", err, cause)
			}
			if err := stream.Close(); err != nil || !body.closed {
				t.Errorf("Close returned %v, body closed=%v", err, body.closed)
			}
		})
	}
}
