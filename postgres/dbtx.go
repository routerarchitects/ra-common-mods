package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DBTX is the minimal query and execution interface satisfied by *pgxpool.Pool, *pgxpool.Conn, and pgx.Tx.
// Any repository method accepting DBTX can run either standalone against the pool
// or atomically within an ongoing transaction without requiring code duplication.
type DBTX interface {
	Exec(ctx context.Context, sql string, arguments ...any) (commandTag pgconn.CommandTag, err error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ExtendedDBTX extends DBTX with high-throughput batch and bulk copy operations.
// Repositories only need to accept ExtendedDBTX if they specifically perform batching or copying.
type ExtendedDBTX interface {
	DBTX
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
	CopyFrom(ctx context.Context, tableName pgx.Identifier, columnNames []string, rowSrc pgx.CopyFromSource) (int64, error)
}
