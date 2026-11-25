package s3

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/routerarchitects/ra-common-mods/apperror"
)

func TestClientRejectsInvalidBucketNamesBeforeRequests(t *testing.T) {
	client := newProtocolTestClient(t, Config{}, handlerTransport(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("invalid bucket reached the backend: %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusBadRequest)
	}))
	ctx := t.Context()
	const key = "key"
	ops := []struct {
		name string
		call func(string) error
	}{
		{"Upload", func(bucket string) error {
			return client.Upload(ctx, bucket, key, strings.NewReader("payload"), 7, "text/plain")
		}},
		{"Download", func(bucket string) error { _, err := client.Download(ctx, bucket, key); return err }},
		{"Delete", func(bucket string) error { return client.Delete(ctx, bucket, key) }},
		{"Exists", func(bucket string) error { _, err := client.Exists(ctx, bucket, key); return err }},
		{"StatObject", func(bucket string) error { _, err := client.StatObject(ctx, bucket, key); return err }},
		{"ListObjects", func(bucket string) error {
			var result error
			for _, err := range client.ListObjects(ctx, bucket, "") {
				result = err
			}
			return result
		}},
		{"MakeBucket", func(bucket string) error { return client.MakeBucket(ctx, bucket) }},
		{"RemoveBucket", func(bucket string) error { return client.RemoveBucket(ctx, bucket) }},
		{"BucketExists", func(bucket string) error { _, err := client.BucketExists(ctx, bucket); return err }},
		{"Ping", func(bucket string) error { return client.Ping(ctx, bucket) }},
	}
	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			for _, bucket := range []string{"Test-Bucket", " test-bucket ", " ", "invalid/bucket", "invalid_bucket"} {
				if err := op.call(bucket); apperror.CodeOf(err) != apperror.CodeInvalidInput {
					t.Errorf("%s(%q) = %v, want invalid input", op.name, bucket, err)
				}
			}
		})
	}
}

func TestClientPingUsesLiteralBucket(t *testing.T) {
	var requests []string
	client := newProtocolTestClient(t, Config{}, handlerTransport(func(w http.ResponseWriter, r *http.Request) {
		path := r.Method + " " + r.URL.Path
		requests = append(requests, path)
		switch path {
		case "HEAD /valid.bucket/":
			w.WriteHeader(http.StatusOK)
		case "GET /":
			_, _ = io.WriteString(w, "<ListAllMyBucketsResult><Buckets/></ListAllMyBucketsResult>")
		default:
			t.Errorf("unexpected readiness request %s", path)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	for _, bucket := range []string{"valid.bucket", ""} {
		if err := client.Ping(t.Context(), bucket); err != nil {
			t.Fatalf("Ping(%q): %v", bucket, err)
		}
	}
	if len(requests) != 2 {
		t.Fatalf("got %v, want one bucket request and one cluster request", requests)
	}
}
