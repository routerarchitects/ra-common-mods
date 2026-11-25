# S3 storage

This Go 1.26 module provides a multi-bucket S3 client built on the MinIO SDK and a generic bucket-scoped adapter in `s3/storage`. The first release is prepared for the repository tag `s3/v0.1.0`. Once tagged, install it with:

```sh
go get github.com/routerarchitects/ra-common-mods/s3@v0.1.0
```

## Configuration

`s3.DefaultConfig()` loads the following settings. Use `s3.LoadConfig("ARCHIVE_S3_")` to load a second configuration with a different prefix. The relative environment tags also support embedding `s3.Config` with `envPrefix:"ARCHIVE_S3_"`.

| Environment variable | Default | Behavior |
| --- | --- | --- |
| `STORAGE_TYPE_S3_ENDPOINT` | `https://127.0.0.1:9000` | Endpoint URL with an explicit `https://` or `http://` scheme and a host or host:port. |
| `STORAGE_TYPE_S3_ACCESS_KEY` | Empty | Static S3 access key. Supply both keys for static authentication. |
| `STORAGE_TYPE_S3_SECRET_KEY` | Empty | Static S3 secret key. Omitting both keys selects the MinIO SDK's IAM provider. |
| `STORAGE_TYPE_S3_REGION` | `us-east-1` | Set to the backend's signing region. |
| `STORAGE_TYPE_S3_FORCE_PATH_STYLE` | `true` | Use path-style bucket addressing. Set to `false` to let the SDK select the addressing style. |
| `STORAGE_TYPE_S3_MAX_CONCURRENT_UPLOADS` | `4` | Maximum active uploads per client, shared by all its buckets. Zero selects the default. |
| `STORAGE_TYPE_S3_PART_SIZE` | `5242880` | Multipart part size in bytes. Use 5 MiB through 5 GiB, within the platform's maximum buffer size. Zero selects the default. |
| `STORAGE_TYPE_S3_MAX_UPLOAD_BUFFER_BYTES` | `134217728` | Upload part-buffer budget in bytes, defaulting to 128 MiB. Zero selects the default. |

`s3.NewClient` also accepts a programmatic `Config`. The endpoint scheme sets the transport. For a local plaintext server, use an endpoint such as `http://127.0.0.1:9000`. HTTPS verifies certificates using system trust and accepts additional roots from `SSL_CERT_FILE`. Endpoint URLs accept an optional root slash and reject credentials, paths, queries and fragments.

## Usage

```go
package example

import (
    "context"
    "io"

    "github.com/routerarchitects/ra-common-mods/s3"
    "github.com/routerarchitects/ra-common-mods/s3/storage"
)

// CopyObject stores a stream in an existing bucket and streams it to dst.
func CopyObject(ctx context.Context, src io.Reader, dst io.Writer) error {
    cfg, err := s3.DefaultConfig()
    if err != nil {
        return err
    }
    client, err := s3.NewClient(cfg)
    if err != nil {
        return err
    }
    archive, err := storage.NewScopedBucket(client, "report-archive")
    if err != nil {
        return err
    }
    const key = "daily/report.csv"
    if err := archive.Upload(ctx, key, src, -1, "text/csv"); err != nil {
        return err
    }
    stream, err := archive.Download(ctx, key)
    if err != nil {
        return err
    }
    _, copyErr := io.Copy(dst, stream)
    closeErr := stream.Close()
    if copyErr != nil {
        return copyErr
    }
    return closeErr
}
```

The client also exposes bucket-aware `Upload`, `Download`, `Delete`, `Exists`, `ListObjects`, `StatObject`, `BucketExists`, `MakeBucket`, `RemoveBucket`, `Ping` and `Endpoint`. The bucket-scoped adapter accepts any non-nil implementation of `storage.Storage` and validates the supplied S3 bucket name locally. Call `Ping` on the adapter to check whether the bucket exists and is accessible.

Both APIs require a valid S3 bucket name and pass it to the backend exactly as supplied. Invalid names, including names with uppercase letters or surrounding whitespace, return `apperror.CodeInvalidInput`. A literal empty string in `client.Ping(ctx, "")` selects the cluster-level check.

### Listing and metadata

`ListObjects` returns an `iter.Seq2[ObjectSummary, error]` over the SDK's paginated listing. Each summary contains the key, size, last modification time and ETag. The iterator yields a terminal error on failure. A `break` stops the listing without fetching another page. Use `StatObject` when you need the content type or detailed response metadata. `ObjectInfo.Metadata` is an `http.Header` and preserves metadata values such as `X-Amz-Meta-*`.

```go
for summary, err := range archive.ListObjects(ctx, "daily/") {
    if err != nil {
        return err
    }
    info, err := archive.StatObject(ctx, summary.Key)
    if err != nil {
        return err
    }
    if info.ContentType == "text/csv" {
        // Use info.Key and any metadata needed for this object.
        break
    }
}
```

### Upload buffering

Uploads start at the reader's current position. Pass the byte length to upload when available or `-1` for an unknown length. A known-size reader that implements both `io.ReaderAt` and `io.Seeker` keeps its original cursor. Other inputs are consumed sequentially.

Streaming multipart uploads use one SDK thread per object and read one part at a time. Each client checks that the active upload limit multiplied by the part size fits its configured part-buffer budget. The SDK also enforces its multipart part-count limit. Uploads waiting for a slot and network requests honor context cancellation. A caller-supplied reader that blocks inside a read must support cancellation itself.

### Missing objects and errors

- `Delete` succeeds when the backend reports success or confirms the object is absent. `Exists` returns `false, nil` for confirmed absence.
- `Download` classifies errors from the object GET before returning a stream. The caller closes the stream and handles any subsequent read error.
- Uncoded 404s and generic `NotFound` or `ResourceNotFound` responses trigger a listing with the requested key as the prefix. The exact key must be absent from the complete listing. Similar keys do not match. Verification requires permission to list the requested prefix.
- An exact match, failed listing, or inconsistent listing produces an error. If a listing page supplies `KeyCount`, the count must match the returned entries. An explicit missing-bucket response returns `apperror.CodeNotFound`. Authorization and infrastructure errors retain their underlying causes.
- Application errors from upload readers retain their original code, metadata, and error chain.
- Use `apperror.CodeOf` to inspect the classification and `errors.Is` or `errors.As` to inspect underlying SDK, transport or context errors.

## Validation

From the `s3` directory:

```sh
go test -count=1 -race ./...
go mod verify
go vet ./...
golangci-lint run ./...
govulncheck ./...
```

CI additionally runs the live suite against MinIO. To run it locally, configure the variables above for an isolated backend, then run:

```sh
TEST_S3_INTEGRATION=true go test -race -count=1 -run TestS3Client_LiveIntegration -v .
```

The live suite creates and removes two temporary buckets by default. For Garage, configure the cluster's S3 region and path-style endpoint, then set `TEST_S3_BUCKET` to an existing test bucket the configured key can read and write. Set `TEST_S3_SECOND_BUCKET` to exercise bucket isolation with a second existing bucket. The suite creates uniquely named objects and cleans them up.
