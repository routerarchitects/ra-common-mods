package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/routerarchitects/ra-common-mods/apperror"
)

func objectListXML(prefix, next string, keys ...string) string {
	var page strings.Builder
	fmt.Fprintf(&page, "<ListBucketResult><Name>test-bucket</Name><Prefix>%s</Prefix><IsTruncated>%t</IsTruncated>", prefix, next != "")
	if next != "" {
		fmt.Fprintf(&page, "<NextContinuationToken>%s</NextContinuationToken>", next)
	}
	for _, key := range keys {
		fmt.Fprintf(&page, "<Contents><Key>%s</Key><LastModified>2026-11-25T00:00:00Z</LastModified><Size>7</Size><ETag>&quot;etag&quot;</ETag></Contents>", key)
	}
	page.WriteString("</ListBucketResult>")
	return page.String()
}

func TestListObjectsIterator(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pages   int
		keys    []string
		wantErr bool
	}{
		{name: "pagination", pages: 2, keys: []string{"dir/one", "dir/two", "dir/three"}},
		{name: "early break", pages: 1, keys: []string{"dir/one"}},
		{name: "cancel before page", wantErr: true},
		{name: "cancel during page", pages: 1, keys: []string{"dir/one"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var requests int
			client := newProtocolTestClient(t, Config{}, handlerTransport(func(w http.ResponseWriter, r *http.Request) {
				requests++
				query := r.URL.Query()
				if r.Method != http.MethodGet || query.Get("prefix") != "dir/" || query.Get("list-type") != "2" || query.Get("delimiter") != "" {
					t.Errorf("unexpected listing request %s %s", r.Method, r.URL)
				}
				switch query.Get("continuation-token") {
				case "":
					_, _ = io.WriteString(w, objectListXML("dir/", "next", "dir/one", "dir/two"))
				case "next":
					_, _ = io.WriteString(w, objectListXML("dir/", "", "dir/three"))
				default:
					t.Errorf("unexpected continuation token in %s", r.URL)
					w.WriteHeader(http.StatusBadRequest)
				}
			}))
			seq := client.ListObjects(ctx, "test-bucket", "dir/")
			if requests != 0 {
				t.Fatal("constructing the iterator made a request")
			}
			if tc.name == "cancel before page" {
				cancel()
			}
			var keys []string
			var failures int
			for summary, err := range seq {
				if err != nil {
					failures++
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("listing error = %v", err)
					}
					continue
				}
				if summary.Size != 7 || summary.ETag != "etag" || summary.LastModified.IsZero() {
					t.Fatalf("incomplete summary: %+v", summary)
				}
				keys = append(keys, summary.Key)
				if tc.name == "early break" {
					break
				}
				if tc.name == "cancel during page" {
					cancel()
				}
			}
			if !slices.Equal(keys, tc.keys) || requests != tc.pages ||
				(tc.wantErr && failures != 1) || (!tc.wantErr && failures != 0) {
				t.Fatalf("got keys %v, %d requests and %d errors", keys, requests, failures)
			}
		})
	}
}

func TestListObjectsYieldsPageError(t *testing.T) {
	client := newProtocolTestClient(t, Config{}, handlerTransport(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("continuation-token") != "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = io.WriteString(w, objectListXML("dir/", "next", "dir/one"))
	}))
	var keys []string
	var failures int
	for summary, err := range client.ListObjects(t.Context(), "test-bucket", "dir/") {
		if err != nil {
			failures++
			if apperror.CodeOf(err) != apperror.CodeForbidden {
				t.Fatalf("got %v, want forbidden listing error", err)
			}
			continue
		}
		keys = append(keys, summary.Key)
	}
	if !slices.Equal(keys, []string{"dir/one"}) || failures != 1 {
		t.Fatalf("got keys %v and %d errors", keys, failures)
	}
}

func TestStatObjectReturnsMetadata(t *testing.T) {
	modified := time.Date(2026, time.November, 25, 0, 0, 0, 0, time.UTC)
	client := newProtocolTestClient(t, Config{}, handlerTransport(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead || r.URL.Path != "/test-bucket/dir/object" {
			t.Errorf("unexpected metadata request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Last-Modified", modified.Format(http.TimeFormat))
		w.Header().Set("Content-Length", "7")
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("ETag", `"some-etag"`)
		w.Header().Add("X-Amz-Meta-Team", "storage")
		w.Header().Add("X-Amz-Meta-Team", "operations")
	}))
	info, err := client.StatObject(t.Context(), "test-bucket", "dir/object")
	if err != nil {
		t.Fatal(err)
	}
	if info.Key != "dir/object" || info.Size != 7 || !info.LastModified.Equal(modified) ||
		info.ETag != "some-etag" || info.ContentType != "text/plain" ||
		!slices.Equal(info.Metadata.Values("X-Amz-Meta-Team"), []string{"storage", "operations"}) {
		t.Fatalf("unexpected metadata: %+v", info)
	}
}
