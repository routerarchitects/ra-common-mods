# Logger Module

A production-ready logging module for Go applications, built on top of the standard library's `log/slog` (Go 1.21+). It provides structured logging, redaction, and fine-grained level control via environment variables.

## Design Principles

1.  **Standard Library Native**: Built directly on `log/slog`, ensuring compatibility with the standard Go ecosystem.
2.  **Configuration via Environment**: Fully configurable through environment variables, adhering to 12-factor app principles.
3.  **Context-Aware**: Support for binding fields to context and automatically extracting correlation IDs (when populated by compatible middleware).
4.  **Subsystem Filtering**: Configure specific log levels for different subsystems (e.g., `db=debug,http=info`), enabling targeted debugging.
5.  **Sensitive Data Redaction**: Built-in redaction for sensitive keys (e.g., passwords, tokens) to prevent data leaks.
6.  **Smart Defaults**: Automatically defaults to pretty-printed Text format in `dev` environments and structured JSON in otherwise.

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
	// detailedLog is the configured logger.
	// shutdown is a cleanup function.
	detailedLog, shutdown, err := logger.Init(cfg.Log)
	if err != nil {
		panic(err)
	}
	defer shutdown()

	// 3. Use standard slog or the returned logger
	slog.Info("Service started")
    
    // Or use the returned logger directly
    detailedLog.Info("Using specific logger instance")
}
```

### Subsystems

Create named loggers for different components. You can control their levels independently via `LOG_SUBSYSTEM_LEVELS`.

```go
// Create a subsystem logger
dbLog := logger.Subsystem("db")

// Use it with context
dbLog.InfoContext(ctx, "query executed", "duration", "5ms")
```

### Context Logging

Bind arbitrary fields to a context to have them automatically included in all subsequent logs.

```go
// Bind user_id to the context
ctx = logger.With(ctx, "user_id", "u-123")

// All logs using this ctx will include "user_id": "u-123"
slog.InfoContext(ctx, "processing order")
```

## Configuration

The module is configured via environment variables.

### General Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `SERVICE_NAME` | **Required** | Name of the service. |
| `SERVICE_VERSION` | **Required** | Version of the service. |
| `ENVIRONMENT` | `dev` | Environment name (e.g., `dev`, `stage`, `prod`). |
| `LOG_FORMAT` | `json`* | Output format: `json` or `text`. *Defaults to `text` if `ENVIRONMENT=dev`. |
| `LOG_ADD_SOURCE` | `false` | If true, adds source file and line number to logs. |
| `LOG_LEVEL` | `info` | Default log level (`debug`, `info`, `warn`, `error`). |

### Subsystems

| Variable | Default | Description |
|----------|---------|-------------|
| `LOG_SUBSYSTEM_LEVELS` | `""` | Comma-separated list of subsystem levels. <br>Example: `http=info,db=debug,worker=warn` |

### Redaction

Automatically masks sensitive fields in logs.

| Variable | Default | Description |
|----------|---------|-------------|
| `LOG_REDACT_ENABLED` | `false` | Enable redaction. |
| `LOG_REDACT_KEYS` | *(See below)* | CSV of keys to redact (case-insensitive keys). |
| `LOG_REDACT_REPLACEMENT` | `******` | String to replace sensitive values with. |

**Default Redaction Keys:**
`authorization`, `cookie`, `set-cookie`, `password`, `passwd`, `token`, `access_token`, `refresh_token`, `secret`, `api_key`, `x-api-key`

### Stacktraces

| Variable | Default | Description |
|----------|---------|-------------|
| `LOG_STACK_ENABLED` | `false` | Enable automatic stacktrace logging. |
| `LOG_STACK_LEVEL` | `error` | Minimum log level to attach stacktraces to. |
