package s3

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/routerarchitects/ra-common-mods/apperror"
)

// isMissingObjectCode matches object-specific codes. A synthetic NoSuchKey
// still requires verification before it establishes absence.
func isMissingObjectCode(resp minio.ErrorResponse) bool {
	if resp.StatusCode != 0 && resp.StatusCode != http.StatusNotFound {
		return false
	}
	switch resp.Code {
	case "NoSuchKey", "ObjectNotFound":
		return true
	}
	return false
}

func isBucketNotFound(code string) bool {
	return code == "NoSuchBucket" || code == "BucketNotFound"
}

func errorCode(err error) apperror.Code {
	if errors.Is(err, context.DeadlineExceeded) {
		return apperror.CodeTimeout
	}
	var resp minio.ErrorResponse
	if !errors.As(err, &resp) {
		return apperror.CodeInternal
	}
	switch {
	case resp.StatusCode >= 500:
		return apperror.CodeInternal
	case resp.StatusCode == http.StatusUnauthorized:
		return apperror.CodeUnauthorized
	case resp.StatusCode == http.StatusForbidden:
		return apperror.CodeForbidden
	}
	switch resp.Code {
	case "InvalidAccessKeyId", "SignatureDoesNotMatch", "ExpiredToken", "InvalidToken":
		return apperror.CodeUnauthorized
	case "AccessDenied", "Forbidden":
		return apperror.CodeForbidden
	case "InvalidArgument", "InvalidBucketName", "InvalidObjectName", "EntityTooLarge":
		return apperror.CodeInvalidInput
	case "BucketAlreadyExists", "BucketAlreadyOwnedByYou", "BucketNotEmpty", "Conflict":
		return apperror.CodeConflict
	}
	if isBucketNotFound(resp.Code) || isMissingObjectCode(resp) {
		return apperror.CodeNotFound
	}
	return apperror.CodeInternal
}

func wrapS3Error(message string, err error) error {
	var appErr *apperror.Error
	if errors.As(err, &appErr) && appErr != nil {
		return err
	}
	return apperror.Wrap(errorCode(err), message, err)
}

func (c *Client) objectFailure(ctx context.Context, bucket, key string, wire *responseObservation, err error, message string) (bool, error) {
	var resp minio.ErrorResponse
	if !errors.As(err, &resp) {
		return false, wrapS3Error(message, err)
	}

	// A decoded XML error has an XMLName. The SDK's synthetic NoSuchKey has
	// neither an XML name nor a wire error code.
	syntheticNoSuchKey := resp.Code == "NoSuchKey" && wire.headerCode == "" && resp.XMLName.Local == ""
	if resp.StatusCode == http.StatusNotFound &&
		(resp.Code == "" || resp.Code == "NotFound" || resp.Code == "ResourceNotFound" || syntheticNoSuchKey) {
		verification := &listingVerification{bucket: bucket, key: key}
		verifyCtx := context.WithValue(ctx, listingVerificationKey{}, verification)
		for object, listErr := range c.ListObjects(verifyCtx, bucket, key) {
			if listErr != nil {
				return false, apperror.Wrap(apperror.CodeOf(listErr), message, errors.Join(err, listErr))
			}
			if object.Key == key {
				return false, apperror.Wrap(apperror.CodeInternal, message+": exact key exists in prefix listing", err)
			}
			if !strings.HasPrefix(object.Key, key) {
				return false, apperror.Wrap(apperror.CodeInternal, message+": listing returned a key outside the requested prefix", err)
			}
		}
		if !verification.complete {
			return false, apperror.Wrap(apperror.CodeInternal, message+": prefix listing did not establish absence", err)
		}
		return true, apperror.Wrap(apperror.CodeNotFound, message, err)
	}
	return isMissingObjectCode(resp), wrapS3Error(message, err)
}

type responseContextKey struct{}

// responseObservation retains the wire header before the SDK interprets the 404.
// One observation is scoped to one synchronous SDK operation.
type responseObservation struct {
	method     string
	headerCode string
}

func observeResponse(ctx context.Context, method string) (context.Context, *responseObservation) {
	observation := &responseObservation{method: method}
	return context.WithValue(ctx, responseContextKey{}, observation), observation
}

type responseTransport struct {
	http.RoundTripper
}

func (t responseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.RoundTripper.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	if o, ok := req.Context().Value(responseContextKey{}).(*responseObservation); ok &&
		o.method == req.Method && (req.Method != http.MethodGet || !req.URL.Query().Has("location")) {
		o.headerCode = resp.Header.Get("X-Minio-Error-Code")
	}
	if v, ok := req.Context().Value(listingVerificationKey{}).(*listingVerification); ok &&
		req.Method == http.MethodGet && req.URL.Query().Get("list-type") == "2" && resp.StatusCode == http.StatusOK {
		v.validatePage(resp)
	}
	return resp, nil
}

type listingVerificationKey struct{}

type listingVerification struct {
	bucket   string
	key      string
	complete bool
	tokens   map[string]struct{}
}

// The SDK exposes the objects, but does not verify the response envelope.
// Inspect bounded pages only when a successful listing may establish absence.
func (v *listingVerification) validatePage(resp *http.Response) {
	const maxPageBytes = 8 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPageBytes+1))
	_ = resp.Body.Close()
	if err == nil && len(data) > maxPageBytes {
		err = errors.New("prefix listing page exceeds the verification limit")
	}
	if err == nil {
		err = v.checkPage(data)
	}
	if err != nil {
		// Surface a listing error through the SDK decoder without triggering
		// network retries for an invalid response.
		resp.Body = io.NopCloser(listingReadError{err})
		return
	}
	resp.Body = io.NopCloser(bytes.NewReader(data))
}

func (v *listingVerification) checkPage(data []byte) error {
	var page struct {
		XMLName               xml.Name `xml:"ListBucketResult"`
		Name                  string
		Prefix                string
		EncodingType          string
		IsTruncated           *bool
		NextContinuationToken string
		KeyCount              *string
		Contents              []struct{}
		CommonPrefixes        []struct{}
	}
	if err := xml.Unmarshal(data, &page); err != nil {
		return fmt.Errorf("invalid prefix listing response: %w", err)
	}
	prefix := page.Prefix
	if page.EncodingType == "url" {
		var err error
		prefix, err = url.QueryUnescape(prefix)
		if err != nil {
			return fmt.Errorf("invalid prefix listing encoding: %w", err)
		}
	}
	if page.Name != v.bucket || prefix != v.key || page.IsTruncated == nil || len(page.CommonPrefixes) != 0 {
		return errors.New("prefix listing response did not match the requested bucket and prefix")
	}
	if page.KeyCount != nil {
		count, err := strconv.Atoi(strings.TrimSpace(*page.KeyCount))
		if err != nil {
			return fmt.Errorf("invalid prefix listing key count: %w", err)
		}
		if count != len(page.Contents) {
			return errors.New("prefix listing key count does not match the returned entries")
		}
	}
	if !*page.IsTruncated {
		if page.NextContinuationToken != "" {
			return errors.New("complete prefix listing has an unexpected continuation token")
		}
		v.complete = true
		return nil
	}
	token := page.NextContinuationToken
	if _, seen := v.tokens[token]; token == "" || seen {
		return errors.New("prefix listing has a missing or repeated continuation token")
	}
	if v.tokens == nil {
		v.tokens = make(map[string]struct{})
	}
	v.tokens[token] = struct{}{}
	return nil
}

type listingReadError struct{ err error }

func (r listingReadError) Read([]byte) (int, error) { return 0, r.err }
