# apperror

`apperror` provides a consistent application error model for services and shared libraries.

It includes:

- Semantic error codes.
- Cause wrapping compatible with `errors.Is` and `errors.As`.
- Optional metadata.
- Caller frame capture (`file`, `line`, `function`).
- Structured logging support for `log/slog`.

## Installation

From another Go module:

```bash
go get github.com/routerarchitects/ra-common-mods/apperror
```

## Error Codes

Available code constants:

- `CodeNotFound`
- `CodeInvalidInput`
- `CodeUnauthorized`
- `CodeForbidden`
- `CodeConflict`
- `CodeInternal`
- `CodeTimeout`
- `CodeNotImplemented`
- `CodeUnknown`

## Constructors

### `New(code, message)`

Creates an error without a cause.

```go
err := apperror.New(apperror.CodeConflict, "resource already exists")
```

### `Wrap(code, message, cause)`

Creates an error with a wrapped cause.

```go
root := errors.New("database timeout")
err := apperror.Wrap(apperror.CodeInternal, "query failed", root)
```

### `WrapWithMeta(code, message, cause, meta)`

Creates an error with wrapped cause and metadata.

```go
root := errors.New("id is empty")
err := apperror.WrapWithMeta(
	apperror.CodeInvalidInput,
	"request validation failed",
	root,
	map[string]any{"field": "id"},
)
```

## Accessors

`Error` uses unexported fields and accessor methods:

- `Code() Code`
- `Message() string`
- `Cause() error`
- `Meta() map[string]any`
- `Frame() *Frame`

`Meta()` and `Frame()` return copies so callers cannot mutate internal state.

## Helpers

### `CodeOf(err error) Code`

Returns the code from `*Error`, else `CodeUnknown`.

### `MessageOf(err error) string`

Returns the message from `*Error`; for non-`*Error`, returns `err.Error()`.

## Logging (`slog`)

### `LogValue()`

`*Error` implements `slog.LogValuer`, so it can be logged directly:

```go
log.Error("operation failed", "error", err)
```

### `SlogAttrs(err error) []slog.Attr`

Utility for building structured attributes from any `error`.

```go
log.LogAttrs(ctx, slog.LevelError, "operation failed", apperror.SlogAttrs(err)...)
```

## Structured Output Notes

- For `*Error`, output includes `code`, `message`, optional `frame`, optional `meta`, and recursive `causes`.
- For non-`*Error` values, output includes `message` (no synthetic `UNKNOWN` `code` field in serialized logs).
- Cause trees support both single unwrap and multi-cause unwrap (`errors.Join`).

## Example

See [examples/example.go](examples/example.go).

Run it:

```bash
cd apperror
go run ./examples
```

## Test

```bash
cd apperror
go test ./...
```
