package postgres

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/routerarchitects/ra-common-mods/apperror"
)

// Database encapsulates the PostgreSQL pgxpool connection pool, configuration, and logger.
type Database struct {
	Pool *pgxpool.Pool
	log  *slog.Logger
	cfg  Config
}

// Connect establishes a connection pool to PostgreSQL and returns a Database wrapper.
// It validates the provided configuration before parsing DSN or establishing connections.
func Connect(ctx context.Context, cfg Config, log *slog.Logger) (*Database, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	dsn := BuildDSN(cfg)

	poolConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, apperror.Wrap(apperror.CodeInternal, "failed to parse postgres dsn config", err)
	}

	// Apply connection pool sizing and lifetime configurations
	if cfg.MaxConns > 0 {
		poolConfig.MaxConns = cfg.MaxConns
	} else {
		poolConfig.MaxConns = 25
	}

	poolConfig.MinConns = cfg.MinConns

	if cfg.MaxConnLifetime > 0 {
		poolConfig.MaxConnLifetime = cfg.MaxConnLifetime
	} else {
		poolConfig.MaxConnLifetime = 30 * time.Minute
	}

	if cfg.MaxConnIdleTime > 0 {
		poolConfig.MaxConnIdleTime = cfg.MaxConnIdleTime
	} else {
		poolConfig.MaxConnIdleTime = 10 * time.Minute
	}

	healthCheckTimeout := cfg.HealthCheckTimeout
	if healthCheckTimeout <= 0 {
		healthCheckTimeout = 5 * time.Second
	}

	if log == nil {
		log = slog.Default()
	}

	log.InfoContext(ctx, "connecting to postgresql database",
		"host", cfg.Host,
		"port", cfg.Port,
		"database", cfg.Database,
		"sslmode", cfg.SSLMode,
		"max_conns", poolConfig.MaxConns,
		"min_conns", poolConfig.MinConns,
		"max_conn_lifetime", poolConfig.MaxConnLifetime,
		"max_conn_idle_time", poolConfig.MaxConnIdleTime,
	)

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, apperror.Wrap(apperror.CodeInternal, "failed to establish postgres connection pool", err)
	}

	// Verify the connection pool health
	pingCtx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, apperror.Wrap(apperror.CodeInternal, "failed to ping database", err)
	}

	log.InfoContext(ctx, "successfully connected to postgresql database")

	return &Database{
		Pool: pool,
		log:  log,
		cfg:  cfg,
	}, nil
}

// Ping verifies database connectivity within the provided context.
func (db *Database) Ping(ctx context.Context) error {
	if db == nil || db.Pool == nil {
		return apperror.New(apperror.CodeInternal, "database pool is not initialized")
	}
	return db.Pool.Ping(ctx)
}

// Close closes the connection pool.
func (db *Database) Close() {
	if db != nil && db.Pool != nil {
		if db.log != nil {
			db.log.Info("closing database connection pool")
		}
		db.Pool.Close()
	}
}

// Logger returns the associated structured logger.
func (db *Database) Logger() *slog.Logger {
	if db == nil || db.log == nil {
		return slog.Default()
	}
	return db.log
}

// Config returns a copy of the configuration used to establish the database connection.
func (db *Database) Config() Config {
	if db == nil {
		return Config{}
	}
	return db.cfg
}
