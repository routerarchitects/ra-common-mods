package postgres

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/routerarchitects/ra-common-mods/apperror"
)

var (
	validTableNameRegex = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	validVersionRegex   = regexp.MustCompile(`^[0-9]+[a-zA-Z0-9_-]*$`)
	leadingDigitsRegex  = regexp.MustCompile(`^([0-9]+)`)
)

// MigrationOptions configures the execution of database schema migrations.
type MigrationOptions struct {
	TableName       string // Table name for tracking migrations (defaults to configured or "schema_migrations")
	AdvisoryLockKey int64  // Required 64-bit advisory lock key to serialize concurrent runs
}

func (db *Database) migrationTable() string {
	if db != nil && strings.TrimSpace(db.cfg.MigrationsTable) != "" {
		return strings.TrimSpace(db.cfg.MigrationsTable)
	}
	return "schema_migrations"
}

func sanitizeTableName(table string) (string, error) {
	name := strings.TrimSpace(table)
	if name == "" {
		return "schema_migrations", nil
	}
	if !validTableNameRegex.MatchString(name) {
		return "", apperror.New(apperror.CodeInvalidInput, fmt.Sprintf("invalid migrations table name %q: must match identifier pattern [a-zA-Z_][a-zA-Z0-9_]*", name))
	}
	return name, nil
}

// RunMigrations automatically scans and applies forward SQL migrations (*.up.sql) from migrationsDir.
// It acquires a PostgreSQL session-level advisory lock using advisoryLockKey to serialize migrations across concurrent instances.
// Migration files must use consistent zero-padded numeric prefixes (e.g. 000001_core.up.sql) or fixed-width
// timestamps to ensure correct lexicographical chronological execution.
// Contract: migration files must not contain top-level BEGIN, COMMIT, or ROLLBACK statements; transaction boundaries
// are managed exclusively by the migration runner to ensure atomic schema and tracking history updates.
func (db *Database) RunMigrations(ctx context.Context, migrationsDir string, advisoryLockKey int64) error {
	if _, err := os.Stat(migrationsDir); os.IsNotExist(err) {
		return apperror.New(apperror.CodeNotFound, fmt.Sprintf("migrations directory %q does not exist", migrationsDir))
	}
	return db.RunMigrationsWithOptions(ctx, os.DirFS(migrationsDir), ".", MigrationOptions{
		TableName:       db.migrationTable(),
		AdvisoryLockKey: advisoryLockKey,
	})
}

// RunMigrationsFS executes forward migrations from an arbitrary fs.FS filesystem (e.g. embed.FS or os.DirFS).
func (db *Database) RunMigrationsFS(ctx context.Context, fsys fs.FS, dir string, advisoryLockKey int64) error {
	return db.RunMigrationsWithOptions(ctx, fsys, dir, MigrationOptions{
		TableName:       db.migrationTable(),
		AdvisoryLockKey: advisoryLockKey,
	})
}

// RunMigrationsWithOptions executes forward migrations using custom MigrationOptions (table name and advisory lock key).
func (db *Database) RunMigrationsWithOptions(ctx context.Context, fsys fs.FS, dir string, opts MigrationOptions) error {
	if fsys == nil {
		return apperror.New(apperror.CodeInternal, "migrations filesystem must not be nil")
	}

	if opts.AdvisoryLockKey == 0 {
		return apperror.New(apperror.CodeInvalidInput, "advisory lock key must not be 0: required to prevent concurrent migration race conditions")
	}

	tableName, err := sanitizeTableName(opts.TableName)
	if err != nil {
		return err
	}

	if dir == "" {
		dir = "."
	}

	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return apperror.Wrap(apperror.CodeInternal, fmt.Sprintf("failed to read migrations directory %q", dir), err)
	}

	var upFiles []string
	upFileMap := make(map[string]bool)
	numericPrefixMap := make(map[string]string)
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".up.sql") {
			version := strings.TrimSuffix(entry.Name(), ".up.sql")
			if !validVersionRegex.MatchString(version) {
				return apperror.New(apperror.CodeInvalidInput, fmt.Sprintf("invalid migration version %q in file %q: must start with numbers and contain only alphanumeric, underscore, or dash characters", version, entry.Name()))
			}
			if upFileMap[version] {
				return apperror.New(apperror.CodeConflict, fmt.Sprintf("duplicate migration version detected: %s", version))
			}
			upFileMap[version] = true

			// Validate numeric prefix uniqueness (e.g. reject 000001_users and 000001_orders)
			numericPrefix := leadingDigitsRegex.FindString(version)
			if prevFile, exists := numericPrefixMap[numericPrefix]; exists {
				return apperror.New(apperror.CodeConflict, fmt.Sprintf("duplicate migration version prefix %q detected in %q (previously declared in %q); numeric prefixes must be uniquely numbered", numericPrefix, entry.Name(), prevFile))
			}
			numericPrefixMap[numericPrefix] = entry.Name()

			upFiles = append(upFiles, entry.Name())
		}
	}

	if len(upFiles) == 0 {
		return apperror.New(apperror.CodeInternal, fmt.Sprintf("no migration files (*.up.sql) found in directory '%s'", dir))
	}

	// Enforce that all migration version prefixes have consistent zero-padding (equal digit length)
	// to ensure lexicographical sorting matches numerical chronological execution order.
	var expectedDigitLen int
	var firstPrefixFile string
	for _, file := range upFiles {
		version := strings.TrimSuffix(file, ".up.sql")
		digits := leadingDigitsRegex.FindString(version)
		if expectedDigitLen == 0 {
			expectedDigitLen = len(digits)
			firstPrefixFile = file
		} else if len(digits) != expectedDigitLen {
			return apperror.New(apperror.CodeInvalidInput, fmt.Sprintf("inconsistent migration version prefix length in %q (has %d digits, but %q has %d digits): all versions must use consistent zero-padded prefixes to ensure correct lexicographical ordering", file, len(digits), firstPrefixFile, expectedDigitLen))
		}
	}

	// Verify that each .up.sql file has a matching .down.sql rollback script
	for _, upFile := range upFiles {
		version := strings.TrimSuffix(upFile, ".up.sql")
		downFile := version + ".down.sql"
		downPath := downFile
		if dir != "." {
			downPath = path.Join(dir, downFile)
		}
		if _, err := fs.Stat(fsys, downPath); err != nil {
			if errors.Is(err, fs.ErrNotExist) || os.IsNotExist(err) {
				return apperror.New(apperror.CodeInternal, fmt.Sprintf("migration '%s' is missing paired rollback file '%s'", upFile, downFile))
			}
			return apperror.Wrap(apperror.CodeInternal, fmt.Sprintf("failed to inspect rollback file '%s'", downFile), err)
		}
	}

	if db == nil || db.Pool == nil {
		return apperror.New(apperror.CodeInternal, "database pool is not initialized")
	}

	if db.log != nil {
		db.log.InfoContext(ctx, "checking database migrations", "directory", dir, "table", tableName)
	}

	// Sort migrations lexicographically (e.g. 000001_core_schema.up.sql)
	sort.Strings(upFiles)

	// Acquire a dedicated connection from the pool for advisory locking
	conn, err := db.Pool.Acquire(ctx)
	if err != nil {
		return apperror.Wrap(apperror.CodeInternal, "failed to acquire connection for migrations", err)
	}
	defer conn.Release()

	// Acquire advisory lock
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", opts.AdvisoryLockKey); err != nil {
		return apperror.Wrap(apperror.CodeInternal, "failed to acquire migration advisory lock", err)
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		var unlocked bool
		if err := conn.QueryRow(unlockCtx, "SELECT pg_advisory_unlock($1)", opts.AdvisoryLockKey).Scan(&unlocked); err != nil || !unlocked {
			if db.log != nil {
				db.log.ErrorContext(ctx, "failed to release migration advisory lock; terminating connection", "error", err, "unlocked", unlocked)
			}
			_ = conn.Conn().Close(context.Background())
		}
	}()

	quotedTable := pgx.Identifier{tableName}.Sanitize()

	// Initialize tracking table with checksum column
	createTableSQL := fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			version VARCHAR(255) PRIMARY KEY,
			checksum VARCHAR(64) NOT NULL,
			applied_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
		);
		ALTER TABLE %s ADD COLUMN IF NOT EXISTS checksum VARCHAR(64) DEFAULT '';
	`, quotedTable, quotedTable)
	if _, err := conn.Exec(ctx, createTableSQL); err != nil {
		return apperror.Wrap(apperror.CodeInternal, fmt.Sprintf("failed to create or migrate %s table", tableName), err)
	}

	// Read all currently applied migrations from the database
	rows, err := conn.Query(ctx, fmt.Sprintf("SELECT version, checksum FROM %s", quotedTable))
	if err != nil {
		return apperror.Wrap(apperror.CodeInternal, fmt.Sprintf("failed to query %s table", tableName), err)
	}
	defer rows.Close()

	appliedMigrations := make(map[string]string)
	for rows.Next() {
		var v, chk string
		if err := rows.Scan(&v, &chk); err != nil {
			return apperror.Wrap(apperror.CodeInternal, "failed to scan applied migration row", err)
		}
		appliedMigrations[v] = chk
	}
	if err := rows.Err(); err != nil {
		return apperror.Wrap(apperror.CodeInternal, "failed to read applied migration history", err)
	}

	// Verify that applied database migrations exist in the local migration set
	var latestAppliedVersion string
	for appliedVer := range appliedMigrations {
		if !upFileMap[appliedVer] {
			return apperror.New(apperror.CodeConflict, fmt.Sprintf("migration %q was previously applied to database but is missing from migration source", appliedVer))
		}
		if appliedVer > latestAppliedVersion {
			latestAppliedVersion = appliedVer
		}
	}

	for _, file := range upFiles {
		version := strings.TrimSuffix(file, ".up.sql")
		filePath := file
		if dir != "." {
			filePath = path.Join(dir, file)
		}

		content, err := fs.ReadFile(fsys, filePath)
		if err != nil {
			return apperror.Wrap(apperror.CodeInternal, fmt.Sprintf("failed to read migration file %s", file), err)
		}

		fileChecksum := fmt.Sprintf("%x", sha256.Sum256(content))

		// Check if migration was already applied
		if storedChecksum, alreadyApplied := appliedMigrations[version]; alreadyApplied {
			// Validate checksum integrity
			if storedChecksum != "" && storedChecksum != fileChecksum {
				return apperror.New(apperror.CodeConflict, fmt.Sprintf("migration %q was modified after being applied (stored checksum: %s, file checksum: %s)", version, storedChecksum, fileChecksum))
			}
			if storedChecksum == "" {
				updateSQL := fmt.Sprintf("UPDATE %s SET checksum = $1 WHERE version = $2", quotedTable)
				if _, err := conn.Exec(ctx, updateSQL, fileChecksum, version); err != nil {
					return apperror.Wrap(apperror.CodeInternal, fmt.Sprintf("failed to backfill checksum for migration %q in %s", version, tableName), err)
				}
				if db.log != nil {
					db.log.InfoContext(ctx, "backfilled checksum for legacy migration", "version", version, "table", tableName)
				}
			}
			continue
		}

		// Reject out-of-order unapplied migrations if a higher version was already applied
		if latestAppliedVersion != "" && version < latestAppliedVersion {
			return apperror.New(apperror.CodeConflict, fmt.Sprintf("out-of-order migration detected: %q cannot be applied because higher version %q is already applied", version, latestAppliedVersion))
		}

		if db.log != nil {
			db.log.InfoContext(ctx, "applying database migration", "version", version, "file", file)
		}

		// Run the migration script in an isolated transaction using Simple Protocol (parameterless Exec)
		// Parameterless execution supports multi-statement scripts, functions, triggers, and comments
		err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(content)); err != nil {
				return err
			}

			insertSQL := fmt.Sprintf("INSERT INTO %s (version, checksum) VALUES ($1, $2)", quotedTable)
			if _, err := tx.Exec(ctx, insertSQL, version, fileChecksum); err != nil {
				return err
			}
			return nil
		})

		if err != nil {
			return apperror.Wrap(apperror.CodeInternal, fmt.Sprintf("migration %s failed to execute, transaction rolled back", version), err)
		}

		if db.log != nil {
			db.log.InfoContext(ctx, "successfully applied migration", "version", version)
		}
	}

	if db.log != nil {
		db.log.InfoContext(ctx, "all database migrations are up to date", "table", tableName)
	}
	return nil
}

// Rollback reverses the latest applied migrations up to the specified number of steps.
// It acquires the same session-level advisory lock using advisoryLockKey to ensure concurrency safety.
func (db *Database) Rollback(ctx context.Context, migrationsDir string, steps int, advisoryLockKey int64) error {
	if steps <= 0 {
		return nil
	}
	if _, err := os.Stat(migrationsDir); os.IsNotExist(err) {
		return apperror.New(apperror.CodeNotFound, fmt.Sprintf("migrations directory %q does not exist", migrationsDir))
	}
	return db.RollbackWithOptions(ctx, os.DirFS(migrationsDir), ".", steps, MigrationOptions{
		TableName:       db.migrationTable(),
		AdvisoryLockKey: advisoryLockKey,
	})
}

// RollbackFS reverses the latest applied migrations up to the specified number of steps from an fs.FS filesystem.
func (db *Database) RollbackFS(ctx context.Context, fsys fs.FS, dir string, steps int, advisoryLockKey int64) error {
	return db.RollbackWithOptions(ctx, fsys, dir, steps, MigrationOptions{
		TableName:       db.migrationTable(),
		AdvisoryLockKey: advisoryLockKey,
	})
}

// RollbackWithOptions reverses migrations using custom MigrationOptions (table name and advisory lock key).
func (db *Database) RollbackWithOptions(ctx context.Context, fsys fs.FS, dir string, steps int, opts MigrationOptions) error {
	if steps <= 0 {
		return nil
	}

	if fsys == nil {
		return apperror.New(apperror.CodeInternal, "migrations filesystem must not be nil")
	}

	if opts.AdvisoryLockKey == 0 {
		return apperror.New(apperror.CodeInvalidInput, "advisory lock key must not be 0: required to prevent concurrent migration race conditions")
	}

	tableName, err := sanitizeTableName(opts.TableName)
	if err != nil {
		return err
	}

	if dir == "" {
		dir = "."
	}

	if db == nil || db.Pool == nil {
		return apperror.New(apperror.CodeInternal, "database pool is not initialized")
	}

	// Acquire a dedicated connection from the pool for advisory locking
	conn, err := db.Pool.Acquire(ctx)
	if err != nil {
		return apperror.Wrap(apperror.CodeInternal, "failed to acquire connection for rollback", err)
	}
	defer conn.Release()

	// Acquire advisory lock
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", opts.AdvisoryLockKey); err != nil {
		return apperror.Wrap(apperror.CodeInternal, "failed to acquire migration advisory lock", err)
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		var unlocked bool
		if err := conn.QueryRow(unlockCtx, "SELECT pg_advisory_unlock($1)", opts.AdvisoryLockKey).Scan(&unlocked); err != nil || !unlocked {
			if db.log != nil {
				db.log.ErrorContext(ctx, "failed to release migration advisory lock; terminating connection", "error", err, "unlocked", unlocked)
			}
			_ = conn.Conn().Close(context.Background())
		}
	}()

	quotedTable := pgx.Identifier{tableName}.Sanitize()

	// Verify that the migrations table exists before attempting to read applied records.
	// We preserve exact identifier casing ($1 without lower()) because tables created with quoted
	// identifiers (pgx.Identifier.Sanitize) preserve case in information_schema.tables.
	var tableExists bool
	checkTableSQL := `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.tables 
			WHERE table_name = $1 AND table_schema = current_schema()
		);
	`
	if err := conn.QueryRow(ctx, checkTableSQL, tableName).Scan(&tableExists); err != nil {
		return apperror.Wrap(apperror.CodeInternal, fmt.Sprintf("failed to check existence of migration history table %q", tableName), err)
	}
	if !tableExists {
		return apperror.New(apperror.CodeNotFound, fmt.Sprintf("migration history table %q does not exist; cannot perform rollback", tableName))
	}

	// Query applied migrations in reverse of the defined migration order
	querySQL := fmt.Sprintf(`SELECT version FROM %s ORDER BY version DESC LIMIT $1`, quotedTable)
	rows, err := conn.Query(ctx, querySQL, steps)
	if err != nil {
		return apperror.Wrap(apperror.CodeInternal, fmt.Sprintf("failed to query applied schema migrations from %s", tableName), err)
	}
	defer rows.Close()

	var versions []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return apperror.Wrap(apperror.CodeInternal, "failed to scan migration version", err)
		}
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		return apperror.Wrap(apperror.CodeInternal, "failed to read applied migration versions", err)
	}

	for _, version := range versions {
		downFile := version + ".down.sql"
		downPath := downFile
		if dir != "." {
			downPath = path.Join(dir, downFile)
		}

		content, err := fs.ReadFile(fsys, downPath)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) || os.IsNotExist(err) {
				return apperror.New(apperror.CodeNotFound, fmt.Sprintf("rollback migration file %q for version %q does not exist", downFile, version))
			}
			return apperror.Wrap(apperror.CodeInternal, fmt.Sprintf("failed to read rollback file %s", downFile), err)
		}

		if db.log != nil {
			db.log.InfoContext(ctx, "rolling back database migration", "version", version, "file", downFile)
		}

		// Run rollback script in an isolated transaction using Simple Protocol
		err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(content)); err != nil {
				return err
			}

			deleteSQL := fmt.Sprintf("DELETE FROM %s WHERE version = $1", quotedTable)
			if _, err := tx.Exec(ctx, deleteSQL, version); err != nil {
				return err
			}
			return nil
		})

		if err != nil {
			return apperror.Wrap(apperror.CodeInternal, fmt.Sprintf("failed to roll back migration %s, transaction aborted", version), err)
		}

		if db.log != nil {
			db.log.InfoContext(ctx, "successfully rolled back migration", "version", version)
		}
	}

	return nil
}
