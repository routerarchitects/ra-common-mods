package postgres

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/routerarchitects/ra-common-mods/apperror"
)

// DefaultEnvPrefix is the standard OpenWiFi prefix for primary database configuration.
const DefaultEnvPrefix = "STORAGE_TYPE_POSTGRESQL_"

var validTableRegex = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// Config encapsulates configuration parameters for establishing and tuning
// the PostgreSQL connection pool and migration harness.
// Field tags use relative names so consumers can bind arbitrary prefixes
// (e.g., envPrefix:"STORAGE_TYPE_POSTGRESQL_" or envPrefix:"REPLICA_POSTGRESQL_").
type Config struct {
	Host               string        `env:"HOST" envDefault:"localhost"`
	Port               int           `env:"PORT" envDefault:"5432"`
	Username           string        `env:"USERNAME" envDefault:"postgres"`
	Password           string        `env:"PASSWORD"`
	Database           string        `env:"DATABASE" envDefault:"postgres"`
	SSLMode            string        `env:"SSLMODE" envDefault:"require"`
	MaxConns           int32         `env:"MAX_CONNS" envDefault:"25"`
	MinConns           int32         `env:"MIN_CONNS" envDefault:"2"`
	MaxConnLifetime    time.Duration `env:"MAX_CONN_LIFETIME" envDefault:"30m"`
	MaxConnIdleTime    time.Duration `env:"MAX_CONN_IDLE_TIME" envDefault:"10m"`
	HealthCheckTimeout time.Duration `env:"HEALTH_CHECK_TIMEOUT" envDefault:"5s"`
	AutoMigrate        bool          `env:"AUTO_MIGRATE" envDefault:"false"`
	MigrationsDir      string        `env:"MIGRATIONS_DIR" envDefault:"db/migrations"`
	MigrationsTable    string        `env:"MIGRATIONS_TABLE" envDefault:"schema_migrations"`
}

// Validate validates that configuration fields adhere to PostgreSQL boundaries and constraints.
func (cfg Config) Validate() error {
	host := strings.TrimSpace(cfg.Host)
	if host == "" {
		return apperror.New(apperror.CodeInvalidInput, "postgres host must not be empty")
	}

	if cfg.Port <= 0 || cfg.Port > 65535 {
		return apperror.New(apperror.CodeInvalidInput, fmt.Sprintf("invalid postgres port: %d (must be between 1 and 65535)", cfg.Port))
	}

	database := strings.TrimSpace(cfg.Database)
	if database == "" {
		return apperror.New(apperror.CodeInvalidInput, "postgres database name must not be empty")
	}

	username := strings.TrimSpace(cfg.Username)
	if username == "" {
		return apperror.New(apperror.CodeInvalidInput, "postgres username must not be empty")
	}

	if cfg.MaxConns < 0 {
		return apperror.New(apperror.CodeInvalidInput, "max_conns cannot be negative")
	}

	if cfg.MinConns < 0 {
		return apperror.New(apperror.CodeInvalidInput, "min_conns cannot be negative")
	}

	maxConns := cfg.MaxConns
	if maxConns == 0 {
		maxConns = 25
	}
	if cfg.MinConns > maxConns {
		return apperror.New(apperror.CodeInvalidInput, fmt.Sprintf("min_conns (%d) cannot exceed max_conns (%d)", cfg.MinConns, maxConns))
	}

	if cfg.MaxConnLifetime < 0 {
		return apperror.New(apperror.CodeInvalidInput, "max_conn_lifetime cannot be negative")
	}

	if cfg.MaxConnIdleTime < 0 {
		return apperror.New(apperror.CodeInvalidInput, "max_conn_idle_time cannot be negative")
	}

	if cfg.HealthCheckTimeout < 0 {
		return apperror.New(apperror.CodeInvalidInput, "health_check_timeout cannot be negative")
	}

	sslMode := strings.ToLower(strings.TrimSpace(cfg.SSLMode))
	if sslMode == "" {
		sslMode = "require"
	}
	switch sslMode {
	case "disable", "allow", "prefer", "require", "verify-ca", "verify-full":
		// Valid SSL modes
	default:
		return apperror.New(apperror.CodeInvalidInput, fmt.Sprintf("unsupported postgres sslmode: %q", cfg.SSLMode))
	}

	tableName := strings.TrimSpace(cfg.MigrationsTable)
	if tableName != "" && !validTableRegex.MatchString(tableName) {
		return apperror.New(apperror.CodeInvalidInput, fmt.Sprintf("invalid migrations table name %q: must match identifier pattern [a-zA-Z_][a-zA-Z0-9_]*", tableName))
	}

	return nil
}

// BuildDSN constructs a properly URL-escaped PostgreSQL connection string.
// Default SSL mode is "require" to ensure production connections are encrypted.
// For local development without TLS, explicitly set SSLMode to "disable".
func BuildDSN(cfg Config) string {
	var user *url.Userinfo
	if cfg.Password != "" {
		user = url.UserPassword(cfg.Username, cfg.Password)
	} else if cfg.Username != "" {
		user = url.User(cfg.Username)
	}

	u := url.URL{
		Scheme: "postgres",
		User:   user,
		Host:   net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
		Path:   "/" + strings.TrimPrefix(cfg.Database, "/"),
	}

	sslMode := strings.ToLower(strings.TrimSpace(cfg.SSLMode))
	if sslMode == "" {
		sslMode = "require"
	}
	q := u.Query()
	q.Set("sslmode", sslMode)
	u.RawQuery = q.Encode()

	return u.String()
}

// RedactedDSN constructs a URL-escaped PostgreSQL connection string with password masked.
// Use this for safe logging and diagnostic outputs.
func RedactedDSN(cfg Config) string {
	var user *url.Userinfo
	if cfg.Password != "" {
		user = url.UserPassword(cfg.Username, cfg.Password)
	} else if cfg.Username != "" {
		user = url.User(cfg.Username)
	}

	u := url.URL{
		Scheme: "postgres",
		User:   user,
		Host:   net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
		Path:   "/" + strings.TrimPrefix(cfg.Database, "/"),
	}

	sslMode := strings.ToLower(strings.TrimSpace(cfg.SSLMode))
	if sslMode == "" {
		sslMode = "require"
	}
	q := u.Query()
	q.Set("sslmode", sslMode)
	u.RawQuery = q.Encode()

	return u.Redacted()
}

// LoadConfig parses environment variables into a Config struct using the provided prefix.
func LoadConfig(prefix string) (Config, error) {
	var cfg Config
	if err := env.ParseWithOptions(&cfg, env.Options{Prefix: prefix}); err != nil {
		return Config{}, fmt.Errorf("failed to parse postgres config with prefix %q: %w", prefix, err)
	}
	return cfg, nil
}

// DefaultConfig parses environment variables using the standard OpenWiFi prefix ("STORAGE_TYPE_POSTGRESQL_").
func DefaultConfig() (Config, error) {
	return LoadConfig(DefaultEnvPrefix)
}
