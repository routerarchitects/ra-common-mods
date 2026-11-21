package postgres_test

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/routerarchitects/ra-common-mods/postgres"
)

func TestBuildDSN_SpecialCharacters(t *testing.T) {
	testCases := []struct {
		name             string
		username         string
		password         string
		host             string
		port             int
		database         string
		sslmode          string
		expectedPassword string
	}{
		{
			name:             "Standard credentials",
			username:         "postgres",
			password:         "postgres",
			host:             "localhost",
			port:             5432,
			database:         "testdb",
			sslmode:          "disable",
			expectedPassword: "postgres",
		},
		{
			name:             "Password with percent sign",
			username:         "postgres",
			password:         "sec%zzret",
			host:             "localhost",
			port:             5432,
			database:         "testdb",
			sslmode:          "disable",
			expectedPassword: "sec%zzret",
		},
		{
			name:             "Password with at-sign",
			username:         "appuser",
			password:         "p@ssw@rd",
			host:             "db.internal",
			port:             5432,
			database:         "test_db",
			sslmode:          "require",
			expectedPassword: "p@ssw@rd",
		},
		{
			name:             "Password with slash and spaces",
			username:         "appuser",
			password:         "p/ss word with spaces",
			host:             "localhost",
			port:             5432,
			database:         "testdb",
			sslmode:          "disable",
			expectedPassword: "p/ss word with spaces",
		},
		{
			name:             "Password with hash and question mark",
			username:         "appuser",
			password:         "p#ss?w&rd=123",
			host:             "localhost",
			port:             5432,
			database:         "testdb",
			sslmode:          "verify-full",
			expectedPassword: "p#ss?w&rd=123",
		},
		{
			name:             "Database with leading slash",
			username:         "appuser",
			password:         "secret",
			host:             "localhost",
			port:             5432,
			database:         "/testdb_with_slash",
			sslmode:          "disable",
			expectedPassword: "secret",
		},
		{
			name:             "Empty password without trailing colon",
			username:         "nopassuser",
			password:         "",
			host:             "localhost",
			port:             5432,
			database:         "testdb",
			sslmode:          "disable",
			expectedPassword: "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := postgres.Config{
				Host:     tc.host,
				Port:     tc.port,
				Username: tc.username,
				Password: tc.password,
				Database: tc.database,
				SSLMode:  tc.sslmode,
			}

			dsn := postgres.BuildDSN(cfg)
			t.Logf("Generated DSN (redacted): %s", postgres.RedactedDSN(cfg))

			poolConfig, err := pgxpool.ParseConfig(dsn)
			if err != nil {
				t.Fatalf("pgxpool.ParseConfig failed on valid password %q: %v", tc.password, err)
			}

			if poolConfig.ConnConfig.Password != tc.expectedPassword {
				t.Errorf("Password mismatch: got %q, want %q", poolConfig.ConnConfig.Password, tc.expectedPassword)
			}
			if poolConfig.ConnConfig.User != tc.username {
				t.Errorf("Username mismatch: got %q, want %q", poolConfig.ConnConfig.User, tc.username)
			}
			cleanDB := strings.TrimPrefix(tc.database, "/")
			if poolConfig.ConnConfig.Database != cleanDB {
				t.Errorf("Database mismatch: got %q, want %q", poolConfig.ConnConfig.Database, cleanDB)
			}
		})
	}
}

func TestRedactedDSN(t *testing.T) {
	cfg := postgres.Config{
		Host:     "db.internal",
		Port:     5432,
		Username: "superadmin",
		Password: "supersecretpassword",
		Database: "proddb",
		SSLMode:  "verify-full",
	}

	redacted := postgres.RedactedDSN(cfg)
	if strings.Contains(redacted, "supersecretpassword") {
		t.Errorf("RedactedDSN leaked secret password: %s", redacted)
	}
	if !strings.Contains(redacted, ":xxxxx@") {
		t.Errorf("RedactedDSN did not mask password with xxxxx: %s", redacted)
	}
	if !strings.Contains(redacted, "superadmin") || !strings.Contains(redacted, "proddb") {
		t.Errorf("RedactedDSN lost connection metadata: %s", redacted)
	}
}

func TestConfig_Validate(t *testing.T) {
	tests := []struct {
		name        string
		cfg         postgres.Config
		expectError bool
		errMsg      string
	}{
		{
			name: "Valid default configuration",
			cfg: postgres.Config{
				Host:     "localhost",
				Port:     5432,
				Username: "postgres",
				Database: "postgres",
				SSLMode:  "disable",
			},
			expectError: false,
		},
		{
			name: "Empty host",
			cfg: postgres.Config{
				Host:     "",
				Port:     5432,
				Username: "postgres",
				Database: "postgres",
			},
			expectError: true,
			errMsg:      "postgres host must not be empty",
		},
		{
			name: "Invalid zero port",
			cfg: postgres.Config{
				Host:     "localhost",
				Port:     0,
				Username: "postgres",
				Database: "postgres",
			},
			expectError: true,
			errMsg:      "invalid postgres port",
		},
		{
			name: "Invalid negative port",
			cfg: postgres.Config{
				Host:     "localhost",
				Port:     -5432,
				Username: "postgres",
				Database: "postgres",
			},
			expectError: true,
			errMsg:      "invalid postgres port",
		},
		{
			name: "Port exceeds 65535",
			cfg: postgres.Config{
				Host:     "localhost",
				Port:     70000,
				Username: "postgres",
				Database: "postgres",
			},
			expectError: true,
			errMsg:      "invalid postgres port",
		},
		{
			name: "Empty username",
			cfg: postgres.Config{
				Host:     "localhost",
				Port:     5432,
				Username: "   ",
				Database: "postgres",
			},
			expectError: true,
			errMsg:      "postgres username must not be empty",
		},
		{
			name: "Empty database name",
			cfg: postgres.Config{
				Host:     "localhost",
				Port:     5432,
				Username: "postgres",
				Database: "   ",
			},
			expectError: true,
			errMsg:      "postgres database name must not be empty",
		},
		{
			name: "MinConns exceeds MaxConns",
			cfg: postgres.Config{
				Host:     "localhost",
				Port:     5432,
				Username: "postgres",
				Database: "postgres",
				MaxConns: 5,
				MinConns: 10,
			},
			expectError: true,
			errMsg:      "min_conns (10) cannot exceed max_conns (5)",
		},
		{
			name: "Negative MaxConns",
			cfg: postgres.Config{
				Host:     "localhost",
				Port:     5432,
				Username: "postgres",
				Database: "postgres",
				MaxConns: -1,
			},
			expectError: true,
			errMsg:      "max_conns cannot be negative",
		},
		{
			name: "Negative HealthCheckTimeout",
			cfg: postgres.Config{
				Host:               "localhost",
				Port:               5432,
				Username:           "postgres",
				Database:           "postgres",
				HealthCheckTimeout: -1 * time.Second,
			},
			expectError: true,
			errMsg:      "health_check_timeout cannot be negative",
		},
		{
			name: "Invalid SSLMode",
			cfg: postgres.Config{
				Host:     "localhost",
				Port:     5432,
				Username: "postgres",
				Database: "postgres",
				SSLMode:  "invalid-mode",
			},
			expectError: true,
			errMsg:      "unsupported postgres sslmode",
		},
		{
			name: "Invalid SSLMode none",
			cfg: postgres.Config{
				Host:     "localhost",
				Port:     5432,
				Username: "postgres",
				Database: "postgres",
				SSLMode:  "none",
			},
			expectError: true,
			errMsg:      "unsupported postgres sslmode",
		},
		{
			name: "Valid mixed-case SSLMode DISABLE",
			cfg: postgres.Config{
				Host:     "localhost",
				Port:     5432,
				Username: "postgres",
				Database: "postgres",
				SSLMode:  "DISABLE",
			},
			expectError: false,
		},
		{
			name: "Valid mixed-case SSLMode Require",
			cfg: postgres.Config{
				Host:     "localhost",
				Port:     5432,
				Username: "postgres",
				Database: "postgres",
				SSLMode:  "Require",
			},
			expectError: false,
		},
		{
			name: "Valid mixed-case SSLMode Verify-Full",
			cfg: postgres.Config{
				Host:     "localhost",
				Port:     5432,
				Username: "postgres",
				Database: "postgres",
				SSLMode:  "Verify-Full",
			},
			expectError: false,
		},
		{
			name: "Invalid migrations table identifier",
			cfg: postgres.Config{
				Host:            "localhost",
				Port:            5432,
				Username:        "postgres",
				Database:        "postgres",
				MigrationsTable: "123_invalid-table; DROP TABLE users;",
			},
			expectError: true,
			errMsg:      "invalid migrations table name",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if tt.expectError {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.errMsg)
				}
				if !strings.Contains(err.Error(), tt.errMsg) {
					t.Fatalf("expected error containing %q, got: %v", tt.errMsg, err)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestLoadConfig_Defaults(t *testing.T) {
	t.Setenv("HOST", "")
	t.Setenv("PORT", "")
	t.Setenv("USERNAME", "")
	t.Setenv("PASSWORD", "")
	t.Setenv("DATABASE", "")
	t.Setenv("SSLMODE", "")

	cfg, err := postgres.LoadConfig("")
	if err != nil {
		t.Fatalf("expected no error loading default config, got: %v", err)
	}

	if cfg.Host != "localhost" {
		t.Errorf("expected default Host 'localhost', got %q", cfg.Host)
	}
	if cfg.Port != 5432 {
		t.Errorf("expected default Port 5432, got %d", cfg.Port)
	}
	if cfg.Database != "postgres" {
		t.Errorf("expected default Database 'postgres', got %q", cfg.Database)
	}
	if cfg.MaxConns != 25 {
		t.Errorf("expected default MaxConns 25, got %d", cfg.MaxConns)
	}
	if cfg.MinConns != 2 {
		t.Errorf("expected default MinConns 2, got %d", cfg.MinConns)
	}
	if cfg.MaxConnLifetime != 30*time.Minute {
		t.Errorf("expected default MaxConnLifetime 30m, got %v", cfg.MaxConnLifetime)
	}
	if cfg.AutoMigrate {
		t.Errorf("expected default AutoMigrate to be false, got %v", cfg.AutoMigrate)
	}
	if cfg.MigrationsTable != "schema_migrations" {
		t.Errorf("expected default MigrationsTable 'schema_migrations', got %q", cfg.MigrationsTable)
	}
	if cfg.SSLMode != "require" {
		t.Errorf("expected default SSLMode 'require', got %q", cfg.SSLMode)
	}
}

func TestLoadConfig_CustomPrefix_MultiDatabase(t *testing.T) {
	// Primary DB
	t.Setenv("STORAGE_TYPE_POSTGRESQL_HOST", "primary-db.local")
	t.Setenv("STORAGE_TYPE_POSTGRESQL_PORT", "5432")
	t.Setenv("STORAGE_TYPE_POSTGRESQL_USERNAME", "primary_user")
	t.Setenv("STORAGE_TYPE_POSTGRESQL_PASSWORD", "primary_pass")
	t.Setenv("STORAGE_TYPE_POSTGRESQL_DATABASE", "primary_db")
	t.Setenv("STORAGE_TYPE_POSTGRESQL_MIGRATIONS_TABLE", "fms_schema_migrations")

	// Replica DB
	t.Setenv("REPLICA_POSTGRESQL_HOST", "replica-db.local")
	t.Setenv("REPLICA_POSTGRESQL_PORT", "5433")
	t.Setenv("REPLICA_POSTGRESQL_USERNAME", "replica_user")
	t.Setenv("REPLICA_POSTGRESQL_PASSWORD", "replica_pass")
	t.Setenv("REPLICA_POSTGRESQL_DATABASE", "replica_db")

	primaryCfg, err := postgres.DefaultConfig()
	if err != nil {
		t.Fatalf("expected no error loading primary config, got: %v", err)
	}
	if primaryCfg.Host != "primary-db.local" {
		t.Errorf("expected primary host 'primary-db.local', got %q", primaryCfg.Host)
	}
	if primaryCfg.Database != "primary_db" {
		t.Errorf("expected primary database 'primary_db', got %q", primaryCfg.Database)
	}
	if primaryCfg.MigrationsTable != "fms_schema_migrations" {
		t.Errorf("expected primary migrations table 'fms_schema_migrations', got %q", primaryCfg.MigrationsTable)
	}

	replicaCfg, err := postgres.LoadConfig("REPLICA_POSTGRESQL_")
	if err != nil {
		t.Fatalf("expected no error loading replica config, got: %v", err)
	}
	if replicaCfg.Host != "replica-db.local" {
		t.Errorf("expected replica host 'replica-db.local', got %q", replicaCfg.Host)
	}
	if replicaCfg.Port != 5433 {
		t.Errorf("expected replica port 5433, got %d", replicaCfg.Port)
	}
	if replicaCfg.Database != "replica_db" {
		t.Errorf("expected replica database 'replica_db', got %q", replicaCfg.Database)
	}
}
