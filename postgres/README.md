# PostgreSQL Client & Migration Toolkit (`ra-common-mods/postgres`)

A production-grade, hardened, lightweight PostgreSQL infrastructure module for Go microservices. It provides robust connection pool lifecycle management, safe transactional forward and rollback schema migrations, multi-database transaction isolation, and context-propagated atomic transaction management without unnecessary frameworks or savepoint complexity.

---

## Features

- **Connection Pool Tuning (`pgxpool`)**: Configurable min/max connections, connection lifetimes, idle timeouts, and health-check ping timeouts via environment variables or typed configuration.
- **Secure Configuration & SSL Defaults**:
  - **Secure by Default**: `SSLMODE` defaults to `require` to protect credentials and data from unencrypted transport in production and cloud environments.
  - **Local Development**: Explicitly set `SSLMODE=disable` when running locally against unencrypted PostgreSQL containers or test databases.
  - **Strict Validation**: Validates host, port (1–65535), username, database, pool bounds (`MinConns <= MaxConns`), and valid SSL modes (`disable`, `allow`, `prefer`, `require`, `verify-ca`, `verify-full`). Automatic migrations are disabled by default (`AutoMigrate: false`).
- **Safe Credential Handling**: Passwords and sensitive parameters are never printed in structured logs or connection errors. Provides `RedactedDSN(cfg)` for safe diagnostic logs.
- **Transactional Schema Migrations**:
  - Multi-statement SQL script support (parameterless protocol execution).
  - Cryptographic integrity protection: SHA-256 checksum verification detects tampering of already-applied migrations and automatically backfills legacy records with error propagation.
  - Strict forward (`.up.sql`) and rollback (`.down.sql`) pairing.
  - Namespace isolation: Configurable migration tracking tables (`Config.MigrationsTable` and `MigrationOptions.TableName`) to support multiple services/modules sharing a database.
  - Mandatory session-level advisory locking (`pg_advisory_lock`) with non-zero 64-bit key validation.
  - Supports standard filesystem paths (`RunMigrations`) and embedded filesystems (`RunMigrationsFS` via `//go:embed` or `io/fs.FS`).
- **Hardened Transaction Toolkit**:
  - **Minimal `DBTX` Interface**: Minimal `Exec`, `Query`, `QueryRow` interface for repository implementations, plus optional `ExtendedDBTX` for advanced operations (`CopyFrom`, `SendBatch`).
  - **`WithTx` / `WithTxOptions`**: Atomic transaction block guaranteeing commit on success, detached-context rollback on error or context cancellation, and rollback before re-throwing panics. Rollback errors are preserved and joined via `errors.Join`.
  - **`WithinTx` / `WithinTxOptions`**: Context-propagated transactions. When called within an existing transaction for the same database, it participates directly in the single atomic transaction without SAVEPOINT overhead.
  - **Single-Database Transaction Scope**: Supports single-database transactions. If a context carries an active transaction for another database, `WithinTx` immediately returns a conflict error. Distributed two-phase commit (2PC) is not supported and can be implemented in a future release if required.
  - **Cross-Database Transaction Isolation**: Transactions are strictly bound to their owning `*Database` instance. `db.DB(ctx)` returns `(DBTX, error)` and explicitly rejects cross-database transaction contamination.
  - **`WithRetryTx`**: Automatic backoff and retry on PostgreSQL transient concurrency conflicts (`40001: serialization_failure` and `40P01: deadlock_detected`) respecting context cancellation.
  - **Side-Effect Isolation**: Use `db.Pool` directly when writing out-of-band audit events or alerts that should not be rolled back with the main transaction.

---

## Installation

```bash
go get github.com/routerarchitects/ra-common-mods/postgres
```

---

## Configuration

### 1. Standard (Single Database)

```go
package main

import (
	"context"
	"log/slog"

	"github.com/routerarchitects/ra-common-mods/postgres"
)

func main() {
	ctx := context.Background()

	// Loads from STORAGE_TYPE_POSTGRESQL_* environment variables
	// SSLMODE defaults to "require"
	cfg, err := postgres.DefaultConfig()
	if err != nil {
		panic(err)
	}

	db, err := postgres.Connect(ctx, cfg, slog.Default())
	if err != nil {
		panic(err)
	}
	defer db.Close()
}
```

### 2. Multi-Database (Primary + Read Replica)

```go
package config

import (
	"github.com/caarlos0/env/v11"
	"github.com/routerarchitects/ra-common-mods/postgres"
)

type Config struct {
	// Primary DB: STORAGE_TYPE_POSTGRESQL_HOST, STORAGE_TYPE_POSTGRESQL_PORT, etc.
	PrimaryDB postgres.Config `envPrefix:"STORAGE_TYPE_POSTGRESQL_"`

	// Read Replica: REPLICA_POSTGRESQL_HOST, REPLICA_POSTGRESQL_PORT, etc.
	ReplicaDB postgres.Config `envPrefix:"REPLICA_POSTGRESQL_"`
}
```

### 3. TLS Transport & SSL Mode Configuration

All database connections support configurable `SSLMode` via environment variables (e.g., `STORAGE_TYPE_POSTGRESQL_SSLMODE`). Supported modes are `disable`, `allow`, `prefer`, `require`, `verify-ca`, and `verify-full`.

- **Default policy (`SSLMODE=require`)**:
  OpenWiFi microservices are deployed inside isolated private cloud VPCs and container overlay networks. Defaulting to `require` ensures all network traffic and credentials are encrypted in transit, preventing passive eavesdropping.
  > [!NOTE]
  > Under PostgreSQL/pgx driver conventions, `require` provides transport encryption without verifying server certificate identity or hostname matching against a CA.
- **Server Identity Verification (`SSLMODE=verify-full`)**:
  In zero-trust architectures or environments traversing untrusted networks where server spoofing or man-in-the-middle attacks must be actively mitigated, configure `SSLMODE=verify-full`. When `verify-full` is specified, configure root certificates via standard PostgreSQL client environment variables (`PGSSLROOTCERT=/path/to/ca.crt`) or the system trust store to enforce full certificate chain and hostname verification.
- **Local Development (`SSLMODE=disable`)**:
  For unencrypted local development containers or unit tests, explicitly set `STORAGE_TYPE_POSTGRESQL_SSLMODE=disable`.

---

## Schema Migrations

### Migration File Convention
Migration files are discovered and executed in lexicographical order (`sort.Strings`). To ensure that migrations execute in correct numerical and chronological order, version prefixes **must** use consistent zero-padding across all files in the migration directory:

- **6-digit zero-padding (recommended)**:
  ```text
  db/migrations/
  ├── 000001_initial_schema.up.sql
  ├── 000001_initial_schema.down.sql
  ├── 000002_add_indexes.up.sql
  └── 000002_add_indexes.down.sql
  ```
- **14-digit UTC timestamps (`YYYYMMDDHHMMSS`)**:
  ```text
  db/migrations/
  ├── 20261120120000_core_schema.up.sql
  ├── 20261120120000_core_schema.down.sql
  ├── 20261120120100_add_indexes.up.sql
  └── 20261120120100_add_indexes.down.sql
  ```

> [!IMPORTANT]
> Unpadded version prefixes (such as `1_`, `2_`, ..., `10_`) will sort incorrectly under standard string sorting (`10_` sorts before `2_`). `RunMigrationsWithOptions` validates that all migration files share an identical digit prefix length and rejects inconsistent prefix lengths with `CodeInvalidInput`.

### Running Migrations from Filesystem Path
`RunMigrations` automatically respects the table name configured in `Config.MigrationsTable` (defaulting to `schema_migrations`):

```go
const ServiceMigrationLockKey int64 = 0x4d414e474f535643 // Unique 64-bit advisory lock key

if cfg.AutoMigrate {
    if err := db.RunMigrations(ctx, cfg.MigrationsDir, ServiceMigrationLockKey); err != nil {
        return fmt.Errorf("migration failure: %w", err)
    }
}
```

### Running Migrations with Custom Table Names & Embedded FS (`//go:embed`)
```go
//go:embed db/migrations/*.sql
var migrationFiles embed.FS

opts := postgres.MigrationOptions{
    TableName:       "service_schema_migrations",
    AdvisoryLockKey: ServiceMigrationLockKey,
}

if err := db.RunMigrationsWithOptions(ctx, migrationFiles, "db/migrations", opts); err != nil {
    return fmt.Errorf("embedded migration failure: %w", err)
}
```

### Reversing Migrations (Rollback)
Rollbacks execute strictly in the reverse order of the defined migration sequence (`ORDER BY version DESC`):

```go
// Roll back the latest 2 applied migrations
if err := db.Rollback(ctx, cfg.MigrationsDir, 2, ServiceMigrationLockKey); err != nil {
    return fmt.Errorf("rollback failure: %w", err)
}
```

### Migration Atomicity & Transaction Control Contract

Every `.up.sql` and `.down.sql` script is executed within an atomic PostgreSQL transaction managed exclusively by the migration runner (`pgx.BeginFunc`).

> [!IMPORTANT]
> **No Manual Transaction Control in Migration Files**:
> Migration files **must NOT contain top-level `BEGIN`, `COMMIT`, or `ROLLBACK` statements**.
> Issuing manual transaction control commands inside `.up.sql` or `.down.sql` prematurely closes or aborts the runner's transaction block, decoupling DDL execution from the insertion of the `schema_migrations` tracking record and breaking migration atomicity.
>
> *Note on Enforcement*: This is a documented architectural contract. The runner intentionally avoids client-side SQL regex parsing to avoid falsely rejecting valid transaction keywords appearing inside PL/pgSQL function bodies (e.g. `BEGIN ... END;`), stored procedures, SQL comments, or string literals. Migration authors must adhere to this contract.

---

## Transaction Toolkit

### 1. Minimal Repository Seam (`DBTX`)

Repositories accept `DBTX` to execute queries transparently against either a connection pool or an active transaction:

```go
type AccountRepository struct{}

func (r *AccountRepository) Deduct(ctx context.Context, db postgres.DBTX, id string, amount int) error {
    _, err := db.Exec(ctx, "UPDATE accounts SET balance = balance - $1 WHERE id = $2", amount, id)
    return err
}
```

### 2. Explicit Transaction Closure (`WithTx`)
```go
err := db.WithTx(ctx, func(tx pgx.Tx) error {
    if _, err := tx.Exec(ctx, "INSERT INTO accounts (id, balance) VALUES ($1, $2)", "acc-1", 100); err != nil {
        return err // Automatically rolled back!
    }
    if _, err := tx.Exec(ctx, "INSERT INTO ledgers (account_id, delta) VALUES ($1, $2)", "acc-1", 100); err != nil {
        return err // Automatically rolled back!
    }
    return nil // Automatically committed!
})
```

### 3. Context-Propagated Transactions (`WithinTx`)
`WithinTx` propagates the active transaction through `context.Context`. Repositories retrieve the transaction or connection pool safely using `db.DB(ctx)`:

```go
func (s *Service) TransferFunds(ctx context.Context, from, to string, amount int) error {
    return s.db.WithinTx(ctx, func(txCtx context.Context) error {
        txDB, err := s.db.DB(txCtx)
        if err != nil {
            return err
        }

        if err := s.accountRepo.Deduct(txCtx, txDB, from, amount); err != nil {
            return err
        }
        if err := s.accountRepo.Credit(txCtx, txDB, to, amount); err != nil {
            return err
        }
        return nil
    })
}
```

#### Transaction Semantics & Isolation Rules
- **Same-Database Nesting**: If a function invokes `WithinTx` inside an existing transaction for the same database, it participates directly in the outer transaction without SAVEPOINT overhead.
- **Nested Error Propagation Contract**: Because nested `WithinTx` calls participate directly in the parent transaction without SAVEPOINTs, any business or operational failure in a nested function that requires transaction rollback **must be propagated back to the outer transaction owner** (the root `WithinTx` or `WithTx` caller). If an outer callback intercepts/swallows a Go business error from a nested function and returns `nil`, the outer transaction will commit all preceding writes (provided PostgreSQL did not enter an aborted state). To ensure atomic rollback across nested scopes, always propagate errors upward to the outer transaction boundary.
- **Aborted State Behavior**: Because nested calls participate directly in the parent transaction, any statement failure leaves the PostgreSQL transaction in an aborted state (`SQLSTATE 25P02`). Catching or ignoring the Go error in a nested function will not recover the transaction; the entire transaction must be rolled back by the outer transaction owner.
- **Single-DB Scope & 2PC**: This module currently supports single-database transactions. If `dbB.WithinTx` or `dbB.DB` is called with a context carrying an active transaction from `dbA`, it immediately aborts with a conflict error. If multiple database transactions are injected into a context, all access via `DB()` or `WithinTx()` is rejected to prevent uncoordinated operations. Distributed two-phase commit (2PC) across multiple databases can be implemented in a future release if required.
- **Incompatible Options**: Specifying `pgx.TxOptions` (`IsoLevel`, `AccessMode`, or `DeferrableMode`) inside an existing transaction returns a descriptive conflict error.

### 4. Retry on Serialization & Deadlock (`WithRetryTx`)
```go
err := db.WithRetryTx(ctx, 3, func(tx pgx.Tx) error {
    // Operations susceptible to concurrent row lock conflicts (40001 or 40P01)
    return updateInventory(ctx, tx)
})
```

### 5. Writing Side-Effects Outside the Transaction
When recording an audit log or failure notification that must persist even if the business transaction fails, pass `db.Pool` directly:

```go
func (s *Service) HandleOperation(ctx context.Context) error {
    return s.db.WithinTx(ctx, func(txCtx context.Context) error {
        txDB, err := s.db.DB(txCtx)
        if err != nil {
            return err
        }

        if err := s.doWork(txCtx, txDB); err != nil {
            // Write directly to pool so the audit record is preserved upon transaction rollback
            auditCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
            defer cancel()
            if auditErr := s.auditRepo.RecordFailure(auditCtx, s.db.Pool, "work_failed", err.Error()); auditErr != nil {
                s.log.ErrorContext(ctx, "failed to record out-of-band audit failure", "error", auditErr)
            }
            return err // txCtx operations roll back
        }
        return nil
    })
}
```

> [!WARNING]
> **Pool Saturation & Timeout Considerations**:
> Writing out-of-band via `db.Pool` while inside an active transaction acquires a **second connection** from the pool.
> If all connections in the pool are actively held by concurrent transactions, calling `db.Pool.Exec` will block until a connection becomes available or its context deadline expires. Always execute out-of-band writes with a detached, bounded timeout context (`context.WithTimeout`) and handle errors defensively so that audit failures cannot deadlock or delay outer transaction rollback cleanup.

---

## Running Integration Tests

Unit tests run completely in-memory without external dependencies:
```bash
go test -count=1 -v -race ./...
```

To run the live PostgreSQL integration tests, set `TEST_POSTGRES_DSN` or `DATABASE_URL`:
```bash
export TEST_POSTGRES_DSN="postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable"
go test -count=1 -v -race -run TestIntegration ./...
```

---

## License

This project is licensed under the GNU Affero General Public License v3.0 (AGPLv3) matching the repository root LICENSE file.
