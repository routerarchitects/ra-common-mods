# Logger Module

A production-ready logging module for Go applications, built on top of the standard library's `log/slog`. It provides structured logging, correlation tracking, redaction, and fine-grained level control via environment variables.

## Design Principles

1.  **Standard Library Native**: Built directly on `log/slog` (Go 1.21+), allowing you to use the standard `slog` API and interfaces.
2.  **Configuration via Environment**: Follows 12-factor app principles by being fully configurable through environment variables.
3.  **Context-Aware**: Automatically extracts and logs correlation IDs (Request ID, Trace ID, Span ID) and other fields from the `context.Context`.
4.  **Subsystem Filtering**: specific log levels can be set for different subsystems (e.g., set `db=debug` and `http=info`), allowing for targeted debugging without noisy logs elsewhere.
5.  **Sensitive Data Redaction**: Built-in redaction for sensitive keys (e.g., passwords, tokens) to prevent accidental data leaks.
6.  **Smart Defaults**: Automatically defaults to pretty-printed Text format in `dev` environments and structured JSON in other environments.

## Usage

### Initialization

Initialize the logger at the start of your application. This sets the global `slog.Default()` logger.

```go
package main

import (
	"log/slog"
	"github.com/caarlos0/env/v11"
	"github.com/routerarchitects/ra-common-mods/logger"
)

type Config struct {
	Log logger.Config
}

func main() {
	// 1. Parse Config
	var cfg Config
	if err := env.Parse(&cfg); err != nil {
		panic(err)
	}

	// 2. Init Logger
	root, shutdown, err := logger.Init(cfg.Log)
	if err != nil {
		panic(err)
	}
	defer shutdown()

	// 3. Use it
	slog.Info("Service started")
    
    // Or use the returned root logger
    root.Info("Using root logger directly")
}
```

### Subsystems

Create named loggers for different parts of your application. You can control their levels independently via `LOG_SUBSYSTEM_LEVELS`.

```go
// Create a subsystem logger
dbLog := logger.Subsystem("db")

// Use it with context
dbLog.InfoContext(ctx, "query executed", "duration", "5ms")
```

### Context Logging

The logger automatically extracts correlation IDs if present. You can also bind arbitrary fields to a context.

```go
// Bind user_id to the context
ctx = logger.With(ctx, "user_id", "u-123")

// All subsequent logs using this ctx will have user_id=u-123
slog.InfoContext(ctx, "processing order")
```

### Web Framework Integration (Fiber)

Includes middleware for Fiber to handle request logging and correlation ID propagation.

```go
app := fiber.New()
app.Use(logger.FiberMiddleware())
```

## Configuration

The module is configured via environment variables.

### General

| Variable | Default | Description |
|----------|---------|-------------|
| `SERVICE_NAME` | **Required** | Name of the service. |
| `SERVICE_VERSION` | **Required** | Version of the service. |
| `ENVIRONMENT` | `dev` | Environment name within the system (e.g., `dev`, `stage`, `prod`). |
| `LOG_FORMAT` | `json`* | Output format: `json` or `text`. *Defaults to `text` if `ENVIRONMENT=dev`. |
| `LOG_ADD_SOURCE` | `false` | If true, adds source file and line number to logs. |
| `LOG_LEVEL` | `info` | Default log level for the application (`debug`, `info`, `warn`, `error`). |

### Subsystems

| Variable | Default | Description |
|----------|---------|-------------|
| `LOG_SUBSYSTEM_LEVELS` | `""` | Comma-separated list of subsystem levels. <br>Example: `http=info,db=debug,worker=warn` |

### Correlation

Controls how Request/Trace/Span IDs are handled.

| Variable | Default | Description |
|----------|---------|-------------|
| `LOG_CORR_ENABLED` | `true` | Enable correlation ID tracking. |
| `LOG_CORR_REQUEST_ID_HEADER` | `X-Request-Id` | Header name for Request ID. |
| `LOG_CORR_TRACE_ID_HEADER` | `X-Trace-Id` | Header name for Trace ID. |
| `LOG_CORR_SPAN_ID_HEADER` | `X-Span-Id` | Header name for Span ID. |
| `LOG_CORR_GENERATE_REQUEST_ID` | `true` | Auto-generate Request ID if missing. |
| `LOG_CORR_PROPAGATE` | `true` | Propagate IDs to downstream contexts. |

### Redaction

Automatically masks sensitive fields in logs.

| Variable | Default | Description |
|----------|---------|-------------|
| `LOG_REDACT_ENABLED` | `false` | Enable redaction. |
| `LOG_REDACT_KEYS` | *(See below)* | CSV of keys to redact. |
| `LOG_REDACT_REPLACEMENT` | `******` | String to replace sensitive values with. |

**Default Redaction Keys:**
`authorization`, `cookie`, `set-cookie`, `password`, `passwd`, `token`, `access_token`, `refresh_token`, `secret`, `api_key`, `x-api-key`

## Stacktraces

| Variable | Default | Description |
|----------|---------|-------------|
| `LOG_STACK_ENABLED` | `false` | Enable automatic stacktraces. |
| `LOG_STACK_LEVEL` | `error` | Minimum level to attach stacktraces. |
