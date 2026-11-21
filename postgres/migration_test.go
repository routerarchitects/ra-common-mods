package postgres_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/routerarchitects/ra-common-mods/postgres"
)

const testAdvisoryLockKey int64 = 0x4d414e474f474653

func TestMigration_MissingDirectory(t *testing.T) {
	database := &postgres.Database{Pool: nil}
	nonExistentDir := filepath.Join(t.TempDir(), "nonexistent_dir")

	err := database.RunMigrations(context.Background(), nonExistentDir, testAdvisoryLockKey)
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("expected does not exist error on RunMigrations, got: %v", err)
	}

	err = database.Rollback(context.Background(), nonExistentDir, 1, testAdvisoryLockKey)
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("expected does not exist error on Rollback, got: %v", err)
	}
}

func TestMigration_EmptyDirectory(t *testing.T) {
	database := &postgres.Database{Pool: nil}
	emptyDir := t.TempDir()

	err := database.RunMigrations(context.Background(), emptyDir, testAdvisoryLockKey)
	if err == nil || !strings.Contains(err.Error(), "no migration files (*.up.sql) found in directory") {
		t.Fatalf("expected no migration files error, got: %v", err)
	}
}

func TestMigration_MissingDownFile(t *testing.T) {
	database := &postgres.Database{Pool: nil}
	dir := t.TempDir()

	// Create an up migration without a matching down migration
	upPath := filepath.Join(dir, "000001_test.up.sql")
	if err := os.WriteFile(upPath, []byte("CREATE TABLE test_table (id INT);"), 0o600); err != nil {
		t.Fatalf("failed to create dummy up file: %v", err)
	}

	err := database.RunMigrations(context.Background(), dir, testAdvisoryLockKey)
	if err == nil || !strings.Contains(err.Error(), "missing paired rollback file") {
		t.Fatalf("expected missing paired rollback file error, got: %v", err)
	}
}

func TestMigration_InvalidVersionFormat(t *testing.T) {
	database := &postgres.Database{Pool: nil}

	mapFS := fstest.MapFS{
		"migrations/invalid_version_no_digits.up.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE items (id INT);"),
		},
		"migrations/invalid_version_no_digits.down.sql": &fstest.MapFile{
			Data: []byte("DROP TABLE items;"),
		},
	}

	err := database.RunMigrationsFS(context.Background(), mapFS, "migrations", testAdvisoryLockKey)
	if err == nil || !strings.Contains(err.Error(), "invalid migration version") {
		t.Fatalf("expected invalid migration version error, got: %v", err)
	}
}

func TestMigration_InconsistentPrefixLengthRejection(t *testing.T) {
	database := &postgres.Database{Pool: nil}

	mapFS := fstest.MapFS{
		"migrations/000001_first.up.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE items (id INT);"),
		},
		"migrations/000001_first.down.sql": &fstest.MapFile{
			Data: []byte("DROP TABLE items;"),
		},
		"migrations/2_second.up.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE orders (id INT);"),
		},
		"migrations/2_second.down.sql": &fstest.MapFile{
			Data: []byte("DROP TABLE orders;"),
		},
	}

	err := database.RunMigrationsFS(context.Background(), mapFS, "migrations", testAdvisoryLockKey)
	if err == nil || !strings.Contains(err.Error(), "inconsistent migration version prefix length") {
		t.Fatalf("expected inconsistent prefix length error, got: %v", err)
	}
}

func TestMigration_DuplicateNumericPrefixRejection(t *testing.T) {
	database := &postgres.Database{Pool: nil}

	mapFS := fstest.MapFS{
		"migrations/000001_users.up.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE users (id INT);"),
		},
		"migrations/000001_users.down.sql": &fstest.MapFile{
			Data: []byte("DROP TABLE users;"),
		},
		"migrations/000001_orders.up.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE orders (id INT);"),
		},
		"migrations/000001_orders.down.sql": &fstest.MapFile{
			Data: []byte("DROP TABLE orders;"),
		},
	}

	err := database.RunMigrationsFS(context.Background(), mapFS, "migrations", testAdvisoryLockKey)
	if err == nil || !strings.Contains(err.Error(), "duplicate migration version prefix") {
		t.Fatalf("expected duplicate version prefix error, got: %v", err)
	}
}

func TestMigration_ConfiguredTableName(t *testing.T) {
	// Verify that a Database initialized with custom MigrationsTable uses it
	db := &postgres.Database{
		Pool: nil,
	}
	// Test configuration getter
	if db.Config().MigrationsTable != "" {
		t.Fatal("expected empty MigrationsTable on unconfigured db")
	}

	// Validate invalid custom table names in options
	opts := postgres.MigrationOptions{
		TableName:       "; DROP TABLE users; --",
		AdvisoryLockKey: testAdvisoryLockKey,
	}
	mapFS := fstest.MapFS{
		"migrations/000001_init.up.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE items (id INT);"),
		},
		"migrations/000001_init.down.sql": &fstest.MapFile{
			Data: []byte("DROP TABLE items;"),
		},
	}
	err := db.RunMigrationsWithOptions(context.Background(), mapFS, "migrations", opts)
	if err == nil || !strings.Contains(err.Error(), "invalid migrations table name") {
		t.Fatalf("expected invalid migrations table name error, got: %v", err)
	}
}

func TestMigration_AdvisoryLockKeyZeroRejection(t *testing.T) {
	database := &postgres.Database{Pool: nil}

	mapFS := fstest.MapFS{
		"migrations/000001_init.up.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE items (id INT);"),
		},
		"migrations/000001_init.down.sql": &fstest.MapFile{
			Data: []byte("DROP TABLE items;"),
		},
	}

	// Run with advisoryLockKey = 0 must fail fast
	err := database.RunMigrationsFS(context.Background(), mapFS, "migrations", 0)
	if err == nil || !strings.Contains(err.Error(), "advisory lock key must not be 0") {
		t.Fatalf("expected advisory lock key 0 rejection, got: %v", err)
	}

	err = database.RollbackFS(context.Background(), mapFS, "migrations", 1, 0)
	if err == nil || !strings.Contains(err.Error(), "advisory lock key must not be 0") {
		t.Fatalf("expected advisory lock key 0 rejection on rollback, got: %v", err)
	}
}

func TestMigration_InvalidTableName(t *testing.T) {
	database := &postgres.Database{Pool: nil}
	mapFS := fstest.MapFS{
		"migrations/000001_init.up.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE items (id INT);"),
		},
		"migrations/000001_init.down.sql": &fstest.MapFile{
			Data: []byte("DROP TABLE items;"),
		},
	}

	opts := postgres.MigrationOptions{
		TableName:       "123_invalid-table; DROP TABLE users;",
		AdvisoryLockKey: testAdvisoryLockKey,
	}

	err := database.RunMigrationsWithOptions(context.Background(), mapFS, "migrations", opts)
	if err == nil || !strings.Contains(err.Error(), "invalid migrations table name") {
		t.Fatalf("expected invalid table name error, got: %v", err)
	}

	err = database.RollbackWithOptions(context.Background(), mapFS, "migrations", 1, opts)
	if err == nil || !strings.Contains(err.Error(), "invalid migrations table name") {
		t.Fatalf("expected invalid table name error on rollback, got: %v", err)
	}
}

func TestMigration_NilPool(t *testing.T) {
	database := &postgres.Database{Pool: nil}
	dir := t.TempDir()

	// Create paired dummy migration files
	upPath := filepath.Join(dir, "000001_test.up.sql")
	downPath := filepath.Join(dir, "000001_test.down.sql")
	if err := os.WriteFile(upPath, []byte("SELECT 1;"), 0o600); err != nil {
		t.Fatalf("failed to create dummy up file: %v", err)
	}
	if err := os.WriteFile(downPath, []byte("SELECT 1;"), 0o600); err != nil {
		t.Fatalf("failed to create dummy down file: %v", err)
	}

	err := database.RunMigrations(context.Background(), dir, testAdvisoryLockKey)
	if err == nil || !strings.Contains(err.Error(), "database pool is not initialized") {
		t.Fatalf("expected nil pool error on RunMigrations, got: %v", err)
	}
}

func TestMigration_FS_Support(t *testing.T) {
	database := &postgres.Database{Pool: nil}

	// 1. Nil FS error check
	err := database.RunMigrationsFS(context.Background(), nil, ".", testAdvisoryLockKey)
	if err == nil || !strings.Contains(err.Error(), "migrations filesystem must not be nil") {
		t.Fatalf("expected nil filesystem error, got: %v", err)
	}

	err = database.RollbackFS(context.Background(), nil, ".", 1, testAdvisoryLockKey)
	if err == nil || !strings.Contains(err.Error(), "migrations filesystem must not be nil") {
		t.Fatalf("expected nil filesystem error on rollback, got: %v", err)
	}

	// 2. MapFS with missing down migration
	mapFS := fstest.MapFS{
		"migrations/000001_init.up.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE items (id INT);"),
		},
	}

	err = database.RunMigrationsFS(context.Background(), mapFS, "migrations", testAdvisoryLockKey)
	if err == nil || !strings.Contains(err.Error(), "missing paired rollback file") {
		t.Fatalf("expected missing paired rollback file error with MapFS, got: %v", err)
	}

	// 3. MapFS with paired migrations fails on nil pool
	mapFS["migrations/000001_init.down.sql"] = &fstest.MapFile{
		Data: []byte("DROP TABLE items;"),
	}

	err = database.RunMigrationsFS(context.Background(), mapFS, "migrations", testAdvisoryLockKey)
	if err == nil || !strings.Contains(err.Error(), "database pool is not initialized") {
		t.Fatalf("expected nil pool error on RunMigrationsFS, got: %v", err)
	}
}

func TestRollback_InvalidSteps(t *testing.T) {
	database := &postgres.Database{Pool: nil}

	// Steps <= 0 returns nil immediately without attempting DB connection
	if err := database.Rollback(context.Background(), "dummy_dir", 0, testAdvisoryLockKey); err != nil {
		t.Fatalf("expected nil error for steps=0, got: %v", err)
	}
	if err := database.Rollback(context.Background(), "dummy_dir", -1, testAdvisoryLockKey); err != nil {
		t.Fatalf("expected nil error for steps=-1, got: %v", err)
	}
}
