package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/routerarchitects/ra-common-mods/postgres"
)

// getIntegrationDB connects to the database specified by TEST_POSTGRES_DSN (or DATABASE_URL).
// To ensure complete test safety and prevent destructive changes on shared databases,
// it creates an isolated test schema for the duration of the test and configures search_path accordingly.
func getIntegrationDB(t *testing.T) (*postgres.Database, func()) {
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv("DATABASE_URL"))
	}
	if dsn == "" {
		t.Skip("skipping PostgreSQL integration test: set TEST_POSTGRES_DSN or DATABASE_URL to enable")
	}

	poolConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("failed to parse configured TEST_POSTGRES_DSN: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Initial connection to setup the isolated schema
	setupPool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		t.Fatalf("failed to connect to configured integration test database: %v", err)
	}
	if err := setupPool.Ping(ctx); err != nil {
		setupPool.Close()
		t.Fatalf("configured integration test database is unreachable: %v", err)
	}

	// Create an isolated schema for this test run
	schemaName := fmt.Sprintf("test_integ_%d", time.Now().UnixNano())
	if _, err := setupPool.Exec(ctx, fmt.Sprintf("CREATE SCHEMA %s", schemaName)); err != nil {
		setupPool.Close()
		t.Fatalf("failed to create isolated test schema %s: %v", schemaName, err)
	}
	setupPool.Close()

	// Connect with search_path scoped to the isolated schema
	poolConfig.ConnConfig.RuntimeParams["search_path"] = schemaName
	isolatedPool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		t.Fatalf("failed to initialize pool with isolated schema search_path: %v", err)
	}

	db := &postgres.Database{
		Pool: isolatedPool,
	}

	cleanup := func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanCancel()
		_, _ = isolatedPool.Exec(cleanCtx, fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", schemaName))
		isolatedPool.Close()
	}

	return db, cleanup
}

func TestIntegration_MultiStatementMigrationAndRollback(t *testing.T) {
	db, cleanup := getIntegrationDB(t)
	defer cleanup()

	ctx := context.Background()
	lockKey := int64(0x494e544547303031) // INTEG001
	tableName := "integ_test_schema_migrations"

	migrations := fstest.MapFS{
		"migrations/000001_initial_schema.up.sql": &fstest.MapFile{
			Data: []byte(`
				-- Multi-statement DDL script
				CREATE TABLE integ_users (
					id VARCHAR(64) PRIMARY KEY,
					name VARCHAR(255) NOT NULL,
					created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
				);
				CREATE TABLE integ_orders (
					id VARCHAR(64) PRIMARY KEY,
					user_id VARCHAR(64) REFERENCES integ_users(id),
					amount INT NOT NULL
				);
				CREATE INDEX idx_integ_orders_user ON integ_orders(user_id);
			`),
		},
		"migrations/000001_initial_schema.down.sql": &fstest.MapFile{
			Data: []byte(`
				DROP TABLE IF EXISTS integ_orders CASCADE;
				DROP TABLE IF EXISTS integ_users CASCADE;
			`),
		},
	}

	opts := postgres.MigrationOptions{
		TableName:       tableName,
		AdvisoryLockKey: lockKey,
	}

	// 1. Run migrations with multi-statement SQL and custom table
	if err := db.RunMigrationsWithOptions(ctx, migrations, "migrations", opts); err != nil {
		t.Fatalf("failed to run integration migrations: %v", err)
	}

	// Verify tables were created
	var count int
	if err := db.Pool.QueryRow(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_name = 'integ_users'").Scan(&count); err != nil || count == 0 {
		t.Fatalf("expected integ_users table to exist, count=%d, err=%v", count, err)
	}

	// 2. Checksum validation (running again when unchanged is a no-op)
	if err := db.RunMigrationsWithOptions(ctx, migrations, "migrations", opts); err != nil {
		t.Fatalf("subsequent migration run failed: %v", err)
	}

	// 3. Rollback
	if err := db.RollbackWithOptions(ctx, migrations, "migrations", 1, opts); err != nil {
		t.Fatalf("failed to rollback migration: %v", err)
	}

	// Verify tables were dropped
	if err := db.Pool.QueryRow(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_name = 'integ_users'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("expected integ_users table to be dropped, count=%d", count)
	}
}

func TestIntegration_AdvisoryLockConcurrency(t *testing.T) {
	db, cleanup := getIntegrationDB(t)
	defer cleanup()

	ctx := context.Background()
	lockKey := int64(0x494e544547303032)
	tableName := "integ_concurrent_migrations"

	migrations := fstest.MapFS{
		"migrations/000001_items.up.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE IF NOT EXISTS integ_items (id INT);"),
		},
		"migrations/000001_items.down.sql": &fstest.MapFile{
			Data: []byte("DROP TABLE IF EXISTS integ_items CASCADE;"),
		},
	}

	opts := postgres.MigrationOptions{
		TableName:       tableName,
		AdvisoryLockKey: lockKey,
	}

	// Run concurrent migrations across 5 goroutines
	var wg sync.WaitGroup
	errCh := make(chan error, 5)

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := db.RunMigrationsWithOptions(ctx, migrations, "migrations", opts); err != nil {
				errCh <- err
			}
		}()
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("concurrent migration returned unexpected error: %v", err)
	}
}

func TestIntegration_TransactionCommitRollbackAndPanic(t *testing.T) {
	db, cleanup := getIntegrationDB(t)
	defer cleanup()

	ctx := context.Background()
	if _, err := db.Pool.Exec(ctx, "CREATE TABLE tx_test_items (name VARCHAR(64))"); err != nil {
		t.Fatalf("failed to create test table: %v", err)
	}

	// 1. Commit on success
	err := db.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "INSERT INTO tx_test_items (name) VALUES ('item1')")
		return err
	})
	if err != nil {
		t.Fatalf("WithTx commit failed: %v", err)
	}

	// 2. Rollback on error
	err = db.WithTx(ctx, func(tx pgx.Tx) error {
		_, _ = tx.Exec(ctx, "INSERT INTO tx_test_items (name) VALUES ('item2')")
		return errors.New("simulated business error")
	})
	if err == nil {
		t.Fatal("expected error from WithTx callback")
	}

	// 3. Rollback on panic
	func() {
		defer func() {
			p := recover()
			if p == nil {
				t.Fatal("expected panic to propagate")
			}
		}()
		_ = db.WithTx(ctx, func(tx pgx.Tx) error {
			_, _ = tx.Exec(ctx, "INSERT INTO tx_test_items (name) VALUES ('item3')")
			panic("simulated panic")
		})
	}()

	// Verify only item1 exists
	rows, err := db.Pool.Query(ctx, "SELECT name FROM tx_test_items")
	if err != nil {
		t.Fatalf("failed to query tx_test_items: %v", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		names = append(names, n)
	}

	if len(names) != 1 || names[0] != "item1" {
		t.Fatalf("expected only ['item1'] committed, got: %v", names)
	}
}

func TestIntegration_WithinTx_SameDBReuse_And_CrossDBRejection(t *testing.T) {
	dbA, cleanup := getIntegrationDB(t)
	defer cleanup()

	dbB := &postgres.Database{Pool: nil} // Second db instance without pool

	ctx := context.Background()
	if _, err := dbA.Pool.Exec(ctx, "CREATE TABLE nested_test_items (val INT)"); err != nil {
		t.Fatalf("failed to create test table: %v", err)
	}

	// Test same-DB nesting and cross-DB rejection
	err := dbA.WithinTx(ctx, func(txCtx context.Context) error {
		dbtx, err := dbA.DB(txCtx)
		if err != nil {
			return err
		}
		_, err = dbtx.Exec(txCtx, "INSERT INTO nested_test_items (val) VALUES (42)")
		if err != nil {
			return err
		}

		// Nested WithinTx on dbA participates in the same atomic transaction
		err = dbA.WithinTx(txCtx, func(nestedCtx context.Context) error {
			nestedDBTX, err := dbA.DB(nestedCtx)
			if err != nil {
				return err
			}
			_, err = nestedDBTX.Exec(nestedCtx, "INSERT INTO nested_test_items (val) VALUES (84)")
			return err
		})
		if err != nil {
			return err
		}

		// dbB must detect cross-database transaction contamination and reject without executing callback
		crossCallbackRan := false
		err = dbB.WithinTx(txCtx, func(bCtx context.Context) error {
			crossCallbackRan = true
			return nil
		})
		if err == nil || !strings.Contains(err.Error(), "cross-database transaction detected") {
			t.Fatalf("expected cross-database error on dbB, got: %v", err)
		}
		if crossCallbackRan {
			t.Fatal("expected dbB callback NOT to run")
		}

		return nil
	})

	if err != nil {
		t.Fatalf("expected WithinTx to succeed, got: %v", err)
	}
}

func TestIntegration_OutOfOrderMigrationRejection(t *testing.T) {
	db, cleanup := getIntegrationDB(t)
	defer cleanup()

	ctx := context.Background()
	lockKey := int64(0x494e544547303033)
	tableName := "integ_ooo_migrations"

	opts := postgres.MigrationOptions{
		TableName:       tableName,
		AdvisoryLockKey: lockKey,
	}

	// Step 1: Apply version 000001 and 000003
	initialMigrations := fstest.MapFS{
		"migrations/000001_first.up.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE integ_m1 (id INT);"),
		},
		"migrations/000001_first.down.sql": &fstest.MapFile{
			Data: []byte("DROP TABLE integ_m1;"),
		},
		"migrations/000003_third.up.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE integ_m3 (id INT);"),
		},
		"migrations/000003_third.down.sql": &fstest.MapFile{
			Data: []byte("DROP TABLE integ_m3;"),
		},
	}

	if err := db.RunMigrationsWithOptions(ctx, initialMigrations, "migrations", opts); err != nil {
		t.Fatalf("failed to apply initial migrations: %v", err)
	}

	// Step 2: Introduce unapplied older version 000002
	oooMigrations := fstest.MapFS{
		"migrations/000001_first.up.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE integ_m1 (id INT);"),
		},
		"migrations/000001_first.down.sql": &fstest.MapFile{
			Data: []byte("DROP TABLE integ_m1;"),
		},
		"migrations/000002_second.up.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE integ_m2 (id INT);"),
		},
		"migrations/000002_second.down.sql": &fstest.MapFile{
			Data: []byte("DROP TABLE integ_m2;"),
		},
		"migrations/000003_third.up.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE integ_m3 (id INT);"),
		},
		"migrations/000003_third.down.sql": &fstest.MapFile{
			Data: []byte("DROP TABLE integ_m3;"),
		},
	}

	err := db.RunMigrationsWithOptions(ctx, oooMigrations, "migrations", opts)
	if err == nil {
		t.Fatal("expected out-of-order migration to be rejected, got nil")
	}
	if !strings.Contains(err.Error(), "out-of-order migration detected") {
		t.Fatalf("expected out-of-order conflict error, got: %v", err)
	}
}

func TestIntegration_ChecksumBackfillAndTamperDetection(t *testing.T) {
	db, cleanup := getIntegrationDB(t)
	defer cleanup()

	ctx := context.Background()
	lockKey := int64(0x494e544547303034)
	tableName := "integ_checksum_migrations"

	opts := postgres.MigrationOptions{
		TableName:       tableName,
		AdvisoryLockKey: lockKey,
	}

	migrations := fstest.MapFS{
		"migrations/000001_init.up.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE integ_chktable (id INT);"),
		},
		"migrations/000001_init.down.sql": &fstest.MapFile{
			Data: []byte("DROP TABLE integ_chktable;"),
		},
	}

	// 1. Run migration normally
	if err := db.RunMigrationsWithOptions(ctx, migrations, "migrations", opts); err != nil {
		t.Fatalf("failed initial migration: %v", err)
	}

	// 2. Clear the checksum in database to simulate a legacy record
	quotedTable := pgx.Identifier{tableName}.Sanitize()
	if _, err := db.Pool.Exec(ctx, fmt.Sprintf("UPDATE %s SET checksum = '' WHERE version = '000001_init'", quotedTable)); err != nil {
		t.Fatalf("failed to simulate legacy empty checksum: %v", err)
	}

	// 3. Re-run migration -> should backfill checksum successfully
	if err := db.RunMigrationsWithOptions(ctx, migrations, "migrations", opts); err != nil {
		t.Fatalf("expected successful checksum backfill, got: %v", err)
	}

	// Verify backfilled checksum is not empty
	var backfilledChecksum string
	if err := db.Pool.QueryRow(ctx, fmt.Sprintf("SELECT checksum FROM %s WHERE version = '000001_init'", quotedTable)).Scan(&backfilledChecksum); err != nil || backfilledChecksum == "" {
		t.Fatalf("expected non-empty backfilled checksum, got %q, err: %v", backfilledChecksum, err)
	}

	// 4. Tamper with file content
	tamperedMigrations := fstest.MapFS{
		"migrations/000001_init.up.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE integ_chktable (id INT, tampered BOOLEAN);"),
		},
		"migrations/000001_init.down.sql": &fstest.MapFile{
			Data: []byte("DROP TABLE integ_chktable;"),
		},
	}

	err := db.RunMigrationsWithOptions(ctx, tamperedMigrations, "migrations", opts)
	if err == nil || !strings.Contains(err.Error(), "was modified after being applied") {
		t.Fatalf("expected checksum mismatch error, got: %v", err)
	}
}

func TestIntegration_Rollback_MixedCaseTableName(t *testing.T) {
	db, cleanup := getIntegrationDB(t)
	defer cleanup()

	ctx := context.Background()
	lockKey := int64(0x494e544547303035)
	mixedCaseTable := "App_MigrationHistory_Records"

	opts := postgres.MigrationOptions{
		TableName:       mixedCaseTable,
		AdvisoryLockKey: lockKey,
	}

	migrations := fstest.MapFS{
		"migrations/000001_sample.up.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE integ_mixed_case_test (id INT);"),
		},
		"migrations/000001_sample.down.sql": &fstest.MapFile{
			Data: []byte("DROP TABLE integ_mixed_case_test;"),
		},
	}

	// 1. Run migrations with mixed-case table name
	if err := db.RunMigrationsWithOptions(ctx, migrations, "migrations", opts); err != nil {
		t.Fatalf("failed to run migrations with mixed-case table: %v", err)
	}

	// 2. Rollback with same mixed-case table name
	// This tests that information_schema existence check preserves exact casing and does not fail
	if err := db.RollbackWithOptions(ctx, migrations, "migrations", 1, opts); err != nil {
		t.Fatalf("failed to rollback migration with mixed-case table: %v", err)
	}

	// 3. Rollback with non-existent mixed-case table name must return CodeNotFound
	nonExistentOpts := postgres.MigrationOptions{
		TableName:       "NonExistent_MigrationTable",
		AdvisoryLockKey: lockKey,
	}
	err := db.RollbackWithOptions(ctx, migrations, "migrations", 1, nonExistentOpts)
	if err == nil || !strings.Contains(err.Error(), "does not exist; cannot perform rollback") {
		t.Fatalf("expected CodeNotFound for non-existent table, got: %v", err)
	}
}

func TestIntegration_MigrationWithPLpgSQLFunctionsAndSemicolons(t *testing.T) {
	db, cleanup := getIntegrationDB(t)
	defer cleanup()

	ctx := context.Background()
	lockKey := int64(0x494e544547303036)
	tableName := "integ_function_migrations"

	opts := postgres.MigrationOptions{
		TableName:       tableName,
		AdvisoryLockKey: lockKey,
	}

	migrations := fstest.MapFS{
		"migrations/000001_create_fn_and_trigger.up.sql": &fstest.MapFile{
			Data: []byte(`
				CREATE TABLE integ_audit_items (
					id SERIAL PRIMARY KEY,
					name VARCHAR(100) NOT NULL,
					counter INT NOT NULL DEFAULT 0,
					updated_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
				);

				-- PL/pgSQL function containing internal semicolons and logic blocks
				CREATE OR REPLACE FUNCTION integ_update_audit_items()
				RETURNS TRIGGER AS $$
				DECLARE
					v_threshold INT := 10;
				BEGIN
					IF NEW.counter > v_threshold THEN
						NEW.name = NEW.name || '_elevated';
					END IF;
					NEW.updated_at = CURRENT_TIMESTAMP;
					RETURN NEW;
				END;
				$$ LANGUAGE plpgsql;

				CREATE TRIGGER trg_integ_update_audit_items
				BEFORE UPDATE ON integ_audit_items
				FOR EACH ROW
				EXECUTE FUNCTION integ_update_audit_items();
			`),
		},
		"migrations/000001_create_fn_and_trigger.down.sql": &fstest.MapFile{
			Data: []byte(`
				DROP TRIGGER IF EXISTS trg_integ_update_audit_items ON integ_audit_items;
				DROP FUNCTION IF EXISTS integ_update_audit_items();
				DROP TABLE IF EXISTS integ_audit_items;
			`),
		},
	}

	// 1. Run migrations containing PL/pgSQL function and trigger
	if err := db.RunMigrationsWithOptions(ctx, migrations, "migrations", opts); err != nil {
		t.Fatalf("failed to apply migration with PL/pgSQL function: %v", err)
	}

	// 2. Insert and update row to trigger the function
	var itemID int
	if err := db.Pool.QueryRow(ctx, "INSERT INTO integ_audit_items (name, counter) VALUES ($1, $2) RETURNING id", "item1", 5).Scan(&itemID); err != nil {
		t.Fatalf("failed to insert test item: %v", err)
	}

	if _, err := db.Pool.Exec(ctx, "UPDATE integ_audit_items SET counter = 15 WHERE id = $1", itemID); err != nil {
		t.Fatalf("failed to update test item: %v", err)
	}

	var updatedName string
	if err := db.Pool.QueryRow(ctx, "SELECT name FROM integ_audit_items WHERE id = $1", itemID).Scan(&updatedName); err != nil {
		t.Fatalf("failed to query updated item: %v", err)
	}
	if updatedName != "item1_elevated" {
		t.Fatalf("expected trigger to set name to 'item1_elevated', got %q", updatedName)
	}

	// 3. Rollback the migration
	if err := db.RollbackWithOptions(ctx, migrations, "migrations", 1, opts); err != nil {
		t.Fatalf("failed to roll back migration with PL/pgSQL function: %v", err)
	}

	// 4. Verify table and function were dropped
	var tableCount int
	if err := db.Pool.QueryRow(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_name = 'integ_audit_items'").Scan(&tableCount); err != nil || tableCount != 0 {
		t.Fatalf("expected integ_audit_items table to be dropped, count=%d", tableCount)
	}
}

func TestIntegration_Connect_Lifecycle(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv("DATABASE_URL"))
	}
	if dsn == "" {
		t.Skip("skipping PostgreSQL integration test: set TEST_POSTGRES_DSN or DATABASE_URL to enable")
	}

	poolConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("failed to parse configured TEST_POSTGRES_DSN: %v", err)
	}

	sslMode := poolConfig.ConnConfig.RuntimeParams["sslmode"]
	if sslMode == "" {
		sslMode = "disable"
	}

	cfg := postgres.Config{
		Host:               poolConfig.ConnConfig.Host,
		Port:               int(poolConfig.ConnConfig.Port),
		Username:           poolConfig.ConnConfig.User,
		Password:           poolConfig.ConnConfig.Password,
		Database:           poolConfig.ConnConfig.Database,
		SSLMode:            sslMode,
		MaxConns:           12,
		MinConns:           3,
		MaxConnLifetime:    20 * time.Minute,
		MaxConnIdleTime:    5 * time.Minute,
		HealthCheckTimeout: 3 * time.Second,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 1. Test successful Connect()
	db, err := postgres.Connect(ctx, cfg, nil)
	if err != nil {
		t.Fatalf("postgres.Connect failed: %v", err)
	}

	// 2. Verify pool was initialized and configured with custom limits
	if db.Pool == nil {
		t.Fatal("expected non-nil db.Pool after Connect")
	}
	stat := db.Pool.Stat()
	if stat.MaxConns() != 12 {
		t.Fatalf("expected MaxConns=12, got %d", stat.MaxConns())
	}
	if db.Config().MaxConns != 12 || db.Config().MinConns != 3 {
		t.Fatalf("expected stored Config MaxConns=12/MinConns=3, got %d/%d", db.Config().MaxConns, db.Config().MinConns)
	}

	// 3. Verify Ping() succeeds against active pool
	if err := db.Ping(ctx); err != nil {
		t.Fatalf("db.Ping failed on active database: %v", err)
	}

	// 4. Test Close() and subsequent operations
	db.Close()

	// After Close(), Ping should report pool closed or error
	if err := db.Ping(ctx); err == nil {
		t.Fatal("expected Ping to fail after Close(), got nil")
	}
}
