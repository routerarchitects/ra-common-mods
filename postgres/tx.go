package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/routerarchitects/ra-common-mods/apperror"
)

type txContextKey struct {
	db *Database
}

type activeTxDatabasesKey struct{}

// ContextWithTx injects an active pgx.Tx transaction handle associated with db into the context.
func ContextWithTx(ctx context.Context, db *Database, tx pgx.Tx) context.Context {
	if ctx == nil || db == nil || tx == nil {
		return ctx
	}
	ctx = context.WithValue(ctx, txContextKey{db: db}, tx)

	var activeDBs map[*Database]struct{}
	if existing, ok := ctx.Value(activeTxDatabasesKey{}).(map[*Database]struct{}); ok {
		activeDBs = make(map[*Database]struct{}, len(existing)+1)
		for k, v := range existing {
			activeDBs[k] = v
		}
	} else {
		activeDBs = make(map[*Database]struct{}, 1)
	}
	activeDBs[db] = struct{}{}
	return context.WithValue(ctx, activeTxDatabasesKey{}, activeDBs)
}

// TxFromContext retrieves an active pgx.Tx transaction handle from context associated with this database instance.
// Returns false if no transaction is active for this database or if the context has been contaminated with
// transactions belonging to multiple database instances.
func (db *Database) TxFromContext(ctx context.Context) (pgx.Tx, bool) {
	if ctx == nil || db == nil {
		return nil, false
	}
	// If context has multiple database transactions injected, single-database transaction isolation is violated
	if activeDBs, ok := ctx.Value(activeTxDatabasesKey{}).(map[*Database]struct{}); ok && len(activeDBs) > 1 {
		return nil, false
	}
	tx, ok := ctx.Value(txContextKey{db: db}).(pgx.Tx)
	return tx, ok && tx != nil
}

// DB returns the active pgx.Tx from ctx if one is present for this database instance;
// otherwise it returns db.Pool.
// If the context contains active transactions belonging to other database instances, or multiple databases,
// it returns an explicit error to prevent accidental cross-database or non-transactional fallback execution.
func (db *Database) DB(ctx context.Context) (DBTX, error) {
	if db == nil {
		return nil, apperror.New(apperror.CodeInternal, "database instance is nil")
	}

	// Guard against cross-database transaction contamination:
	// If ctx contains active transactions for other database instances, or multiple database instances,
	// reject to prevent accidental non-transactional fallback or uncoordinated cross-database transaction execution.
	if ctx != nil {
		if activeDBs, ok := ctx.Value(activeTxDatabasesKey{}).(map[*Database]struct{}); ok && len(activeDBs) > 0 {
			if len(activeDBs) > 1 {
				return nil, apperror.New(apperror.CodeConflict, "cross-database transaction conflict: context contains transactions for multiple database instances")
			}
			if _, hasSelf := activeDBs[db]; !hasSelf {
				return nil, apperror.New(apperror.CodeConflict, "context contains a transaction belonging to a different database instance")
			}
		}
	}

	if tx, ok := db.TxFromContext(ctx); ok {
		return tx, nil
	}

	if db.Pool == nil {
		return nil, apperror.New(apperror.CodeInternal, "database pool is not initialized")
	}

	return db.Pool, nil
}

// WithTx executes fn within an atomic PostgreSQL transaction.
// If fn returns nil, the transaction is committed.
// If fn returns an error or panics, the transaction is safely rolled back using a detached context
// to ensure connection cleanup even if ctx has already been cancelled.
func (db *Database) WithTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return db.WithTxOptions(ctx, pgx.TxOptions{}, fn)
}

// WithTxOptions executes fn within a transaction using customized pgx.TxOptions
// (e.g., custom isolation levels or read-only access).
func (db *Database) WithTxOptions(ctx context.Context, opts pgx.TxOptions, fn func(tx pgx.Tx) error) (err error) {
	if db == nil || db.Pool == nil {
		return apperror.New(apperror.CodeInternal, "database pool is not initialized")
	}

	tx, err := db.Pool.BeginTx(ctx, opts)
	if err != nil {
		return apperror.Wrap(apperror.CodeInternal, "failed to begin database transaction", err)
	}

	defer func() {
		if p := recover(); p != nil {
			// Detached context ensures rollback succeeds even if ctx was cancelled
			rollbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			rbErr := tx.Rollback(rollbackCtx)
			cancel()
			if rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
				if db.log != nil {
					db.log.ErrorContext(context.Background(), "failed to rollback transaction during panic", "error", rbErr)
				}
			}
			panic(p) // Re-throw panic after rollback to preserve caller diagnostics
		} else if err != nil {
			rollbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			rbErr := tx.Rollback(rollbackCtx)
			cancel()
			if rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
				if db.log != nil {
					db.log.ErrorContext(ctx, "failed to rollback transaction", "error", rbErr)
				}
				err = errors.Join(err, apperror.Wrap(apperror.CodeInternal, "failed to rollback transaction", rbErr))
			}
		} else {
			if commitErr := tx.Commit(ctx); commitErr != nil {
				err = apperror.Wrap(apperror.CodeInternal, "failed to commit database transaction", commitErr)
				rollbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = tx.Rollback(rollbackCtx)
				cancel()
			}
		}
	}()

	err = fn(tx)
	return err
}

// WithinTx wraps fn in a context-propagated transaction.
// If an active transaction already exists for this database in ctx, fn directly participates
// in the existing atomic transaction without creating savepoints or sub-transactions.
//
// Distributed Transactions Notice:
// This package currently supports single-database atomic transactions only and does not
// support distributed two-phase commit (2PC). If the context already carries an active
// transaction from a different database instance, WithinTx will immediately abort and return
// a conflict error rather than executing uncoordinated operations. Distributed 2PC
// transactions across multiple database instances can be implemented in a future release if required.
//
// Note on PostgreSQL transaction behavior:
// Because nested transactions participate directly in the single parent transaction without SAVEPOINTs,
// any SQL execution error inside fn will leave the underlying PostgreSQL transaction in an aborted state
// (SQLSTATE 25P02: current transaction is aborted, commands ignored until end of transaction block).
// Catching or ignoring a Go error from a failed SQL statement will NOT restore the transaction.
// If any nested operation fails, the transaction must be rolled back by the outer transaction owner.
func (db *Database) WithinTx(ctx context.Context, fn func(txCtx context.Context) error) error {
	return db.WithinTxOptions(ctx, pgx.TxOptions{}, fn)
}

// WithinTxOptions executes WithinTx with customized pgx.TxOptions.
// If called within an existing transaction on the same database, specifying incompatible options
// (IsoLevel, AccessMode, or DeferrableMode) returns a descriptive conflict error.
// If called with a context containing an active transaction for a different database instance,
// it returns a cross-database conflict error without executing fn.
func (db *Database) WithinTxOptions(ctx context.Context, opts pgx.TxOptions, fn func(txCtx context.Context) error) error {
	if db == nil {
		return apperror.New(apperror.CodeInternal, "database instance is nil")
	}

	// 1. Guard against cross-database transaction contamination:
	// If context carries an active transaction for another database or multiple databases, abort immediately.
	// This module currently supports single-database transactions only (no distributed 2PC).
	if ctx != nil {
		if activeDBs, ok := ctx.Value(activeTxDatabasesKey{}).(map[*Database]struct{}); ok && len(activeDBs) > 0 {
			if len(activeDBs) > 1 {
				return apperror.New(apperror.CodeConflict, "cross-database transaction conflict: context contains transactions for multiple database instances; distributed transactions (2PC) are not supported")
			}
			if _, hasSelf := activeDBs[db]; !hasSelf {
				return apperror.New(apperror.CodeConflict, "cross-database transaction detected: context already has an active transaction for another database; distributed transactions (2PC) are not supported")
			}
		}
	}

	// 2. Check if a transaction for this database already exists in ctx (same-database nesting)
	if existingTx, ok := db.TxFromContext(ctx); ok && existingTx != nil {
		// Reject incompatible options attempted on an existing transaction
		if opts.IsoLevel != "" || opts.AccessMode != "" || opts.DeferrableMode != "" {
			return apperror.New(apperror.CodeConflict, fmt.Sprintf(
				"cannot apply transaction options (isoLevel=%q, accessMode=%q, deferrableMode=%q) to an existing transaction",
				opts.IsoLevel, opts.AccessMode, opts.DeferrableMode,
			))
		}
		// Directly participate in existing atomic transaction
		return fn(ctx)
	}

	// 3. Start top-level transaction for this database and inject into context
	if db.Pool == nil {
		return apperror.New(apperror.CodeInternal, "database pool is not initialized")
	}

	return db.WithTxOptions(ctx, opts, func(tx pgx.Tx) error {
		txCtx := ContextWithTx(ctx, db, tx)
		return fn(txCtx)
	})
}

// WithRetryTx executes fn in a transaction, automatically retrying if a transient
// serialization failure (PostgreSQL SQLSTATE 40001) or deadlock (PostgreSQL SQLSTATE 40P01) occurs.
// Note: fn may be invoked multiple times and must not perform irreversible external side-effects.
func (db *Database) WithRetryTx(ctx context.Context, maxRetries int, fn func(tx pgx.Tx) error) error {
	if maxRetries < 0 {
		maxRetries = 0
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		lastErr = db.WithTx(ctx, fn)
		if lastErr == nil {
			return nil
		}

		var pgErr *pgconn.PgError
		if errors.As(lastErr, &pgErr) && (pgErr.Code == "40001" || pgErr.Code == "40P01") {
			// Do not sleep after final attempt
			if attempt == maxRetries {
				return lastErr
			}

			// Transient error; back off and retry if context is still valid
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration((attempt+1)*50) * time.Millisecond):
				continue
			}
		}

		// Non-retryable error
		return lastErr
	}

	return lastErr
}
