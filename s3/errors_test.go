package s3

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/routerarchitects/ra-common-mods/apperror"
)

type testTransport func(*http.Request) (*http.Response, error)

func (transport testTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return transport(req)
}

func handlerTransport(handler http.HandlerFunc) testTransport {
	return func(req *http.Request) (*http.Response, error) {
		if req.Body != nil {
			defer func() { _ = req.Body.Close() }()
		}
		recorder := httptest.NewRecorder()
		handler(recorder, req)
		if err := req.Context().Err(); err != nil {
			return nil, err
		}
		return recorder.Result(), nil
	}
}

func newProtocolTestClient(t *testing.T, cfg Config, transport testTransport) *Client {
	t.Helper()
	cfg.Endpoint = "https://s3.test"
	cfg.AccessKey = "example-access-key"
	cfg.SecretKey = "example-secret-key"
	cfg.ForcePathStyle = true
	client, err := newClient(cfg, transport)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestMissingObjectCode(t *testing.T) {
	for _, tc := range []struct {
		code   string
		status int
		want   bool
	}{
		{"NoSuchKey", 404, true},
		{"ObjectNotFound", 404, true},
		{"ResourceNotFound", 404, false},
		{"NotFound", 404, false},
		{"", 404, false},
		{"NoSuchBucket", 404, false},
		{"BucketNotFound", 404, false},
		{"AccessDenied", 403, false},
		{"InvalidAccessKeyId", 403, false},
		{"InternalError", 500, false},
		{"NoSuchKey", 503, false},
	} {
		if got := isMissingObjectCode(minio.ErrorResponse{Code: tc.code, StatusCode: tc.status}); got != tc.want {
			t.Errorf("isMissingObjectCode(%q, %d) = %v, want %v", tc.code, tc.status, got, tc.want)
		}
	}
}

func TestObjectOperationsClassifyStorageResponses(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		wireCode   string
		errorBody  string
		listStatus int
		listBodies []string
		wantError  apperror.Code
	}{
		{name: "NoSuchKey", status: 404, wireCode: "NoSuchKey"},
		{name: "ObjectNotFound", status: 404, wireCode: "ObjectNotFound"},
		{name: "ResourceNotFound, exact key absent", status: 404, wireCode: "ResourceNotFound", listStatus: 200},
		{name: "NotFound, exact key exists", status: 404, wireCode: "NotFound", listStatus: 200,
			listBodies: []string{objectListXML("object", "", "object")}, wantError: apperror.CodeInternal},
		{name: "NoSuchBucket", status: 404, wireCode: "NoSuchBucket", wantError: apperror.CodeNotFound},
		{name: "BucketNotFound", status: 404, wireCode: "BucketNotFound", wantError: apperror.CodeNotFound},
		{name: "bare 404, exact key absent", status: 404, listStatus: 200},
		{name: "bare 404, matching key count", status: 404, listStatus: 200,
			listBodies: []string{strings.Replace(objectListXML("object", "", "object-copy"), "<Contents>", "<KeyCount>1</KeyCount><Contents>", 1)}},
		{name: "bare 404, key count overstates entries", status: 404, listStatus: 200,
			listBodies: []string{strings.Replace(objectListXML("object", ""), "</ListBucketResult>", "<KeyCount>1</KeyCount></ListBucketResult>", 1)},
			wantError:  apperror.CodeInternal},
		{name: "bare 404, key count understates entries", status: 404, listStatus: 200,
			listBodies: []string{strings.Replace(objectListXML("object", "", "object-copy"), "<Contents>", "<KeyCount>0</KeyCount><Contents>", 1)},
			wantError:  apperror.CodeInternal},
		{name: "bare 404, empty key count", status: 404, listStatus: 200,
			listBodies: []string{strings.Replace(objectListXML("object", ""), "</ListBucketResult>", "<KeyCount></KeyCount></ListBucketResult>", 1)},
			wantError:  apperror.CodeInternal},
		{name: "proxy 404, similar key only", status: 404, errorBody: "<html>Not found</html>", listStatus: 200,
			listBodies: []string{objectListXML("object", "", "object-copy")}},
		{name: "uncoded S3 404, exact key on second page", status: 404, errorBody: "<Error><Message>Not found</Message></Error>", listStatus: 200,
			listBodies: []string{objectListXML("object", "more", "object-copy"), objectListXML("object", "", "object")}, wantError: apperror.CodeInternal},
		{name: "exact key absent across pages", status: 404, listStatus: 200,
			listBodies: []string{objectListXML("object", "more", "object-copy"), objectListXML("object", "", "object.txt")}},
		{name: "bare 404, listing bucket missing", status: 404, listStatus: 404,
			listBodies: []string{"<Error><Code>NoSuchBucket</Code></Error>"}, wantError: apperror.CodeNotFound},
		{name: "bare 404, listing access denied", status: 404, listStatus: 403, wantError: apperror.CodeForbidden},
		{name: "bare 404, listing service failure", status: 404, listStatus: 501, wantError: apperror.CodeInternal},
		{name: "bare 404, unexpected XML", status: 404, listStatus: 200,
			listBodies: []string{"<Error><Code>InternalError</Code></Error>"}, wantError: apperror.CodeInternal},
		{name: "bare 404, wrong prefix", status: 404, listStatus: 200,
			listBodies: []string{objectListXML("unrelated", "")}, wantError: apperror.CodeInternal},
		{name: "bare 404, unfiltered listing", status: 404, listStatus: 200,
			listBodies: []string{objectListXML("object", "", "unrelated")}, wantError: apperror.CodeInternal},
		{name: "bare 404, missing pagination token", status: 404, listStatus: 200,
			listBodies: []string{"<ListBucketResult><Name>test-bucket</Name><Prefix>object</Prefix><IsTruncated>true</IsTruncated></ListBucketResult>"},
			wantError:  apperror.CodeInternal},
		{name: "bare 404, repeated pagination token", status: 404, listStatus: 200,
			listBodies: []string{objectListXML("object", "more"), objectListXML("object", "more")}, wantError: apperror.CodeInternal},
		{name: "unexpected explicit 404", status: 404, wireCode: "UnknownCode", wantError: apperror.CodeInternal},
		{name: "unauthorized", status: 401, wantError: apperror.CodeUnauthorized},
		{name: "forbidden despite object code", status: 403, wireCode: "NoSuchKey", wantError: apperror.CodeForbidden},
		{name: "server failure despite object code", status: 501, wireCode: "NoSuchKey", wantError: apperror.CodeInternal},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, method := range []string{http.MethodHead, http.MethodDelete, http.MethodGet} {
				t.Run(method, func(t *testing.T) {
					var lists int
					transport := handlerTransport(func(w http.ResponseWriter, r *http.Request) {
						switch {
						case r.URL.Path == "/test-bucket/object" && r.Method == method:
							if r.Method == http.MethodHead && tc.wireCode != "" {
								w.Header().Set("X-Minio-Error-Code", tc.wireCode)
							}
							w.WriteHeader(tc.status)
							if r.Method != http.MethodHead && tc.wireCode != "" {
								_, _ = fmt.Fprintf(w, "<Error><Code>%s</Code><BucketName>test-bucket</BucketName><Key>object</Key></Error>", tc.wireCode)
							} else if r.Method != http.MethodHead && tc.errorBody != "" {
								_, _ = w.Write([]byte(tc.errorBody))
							}
						case strings.TrimSuffix(r.URL.Path, "/") == "/test-bucket" && r.Method == http.MethodGet &&
							r.URL.Query().Get("list-type") == "2" && r.URL.Query().Get("prefix") == "object" && tc.listStatus != 0:
							page := lists
							lists++
							w.WriteHeader(tc.listStatus)
							if page < len(tc.listBodies) {
								_, _ = w.Write([]byte(tc.listBodies[page]))
							} else if len(tc.listBodies) == 0 && tc.listStatus == 200 {
								_, _ = w.Write([]byte(objectListXML("object", "")))
							}
						default:
							t.Errorf("unexpected request %s %s", r.Method, r.URL)
							w.WriteHeader(http.StatusBadRequest)
						}
					})
					client := newProtocolTestClient(t, Config{}, transport)
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()

					var err error
					wantError := tc.wantError
					switch method {
					case http.MethodDelete:
						err = client.Delete(ctx, "test-bucket", "object")
					case http.MethodHead:
						var exists bool
						exists, err = client.Exists(ctx, "test-bucket", "object")
						if exists {
							t.Fatal("object unexpectedly reported as present despite the failed stat")
						}
					case http.MethodGet:
						var stream io.ReadCloser
						stream, err = client.Download(ctx, "test-bucket", "object")
						if stream != nil {
							_ = stream.Close()
							t.Fatal("unexpected download stream after a failed GET")
						}
						if wantError == "" {
							wantError = apperror.CodeNotFound
						}
					}
					if wantError == "" {
						if err != nil {
							t.Fatalf("confirmed absent object returned an error: %v", err)
						}
					} else {
						if apperror.CodeOf(err) != wantError {
							t.Fatalf("got %v, want error code %s", err, wantError)
						}
						var underlying minio.ErrorResponse
						if !errors.As(err, &underlying) || underlying.StatusCode != tc.status {
							t.Fatalf("underlying object response was lost: %v", err)
						}
						if tc.listStatus >= 400 {
							var appErr *apperror.Error
							errors.As(err, &appErr)
							causes, ok := appErr.Cause().(interface{ Unwrap() []error })
							if !ok || len(causes.Unwrap()) != 2 {
								t.Fatalf("underlying listing failure was lost: %v", err)
							}
						}
					}
					wantLists := 0
					if tc.listStatus != 0 {
						wantLists = max(1, len(tc.listBodies))
					}
					if lists != wantLists {
						t.Fatalf("got %d listing requests, want %d", lists, wantLists)
					}
				})
			}
		})
	}
}

func TestObjectOperationsPreserveTransportErrors(t *testing.T) {
	// Simulate a transport-level certificate verification failure.
	cause := x509.UnknownAuthorityError{}
	client := newProtocolTestClient(t, Config{}, func(req *http.Request) (*http.Response, error) {
		return nil, cause
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	exists, existsErr := client.Exists(ctx, "test-bucket", "object")
	deleteErr := client.Delete(ctx, "test-bucket", "object")
	_, downloadErr := client.Download(ctx, "test-bucket", "object")
	if exists {
		t.Fatal("object unexpectedly exists")
	}
	for _, err := range []error{existsErr, deleteErr, downloadErr} {
		if apperror.CodeOf(err) != apperror.CodeInternal || !errors.Is(err, cause) {
			t.Fatalf("expected wrapped certificate verification error, got %v", err)
		}
	}
}
