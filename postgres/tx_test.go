package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/routerarchitects/ra-common-mods/postgres"
)

type mockTx struct {
	pgx.Tx
}

func TestTx_NilPoolErrors(t *testing.T) {
	database := &postgres.Database{Pool: nil}

	err := database.WithTx(context.Background(), func(tx pgx.Tx) error {
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "database pool is not initialized") {
		t.Fatalf("expected nil pool error on WithTx, got: %v", err)
	}

	err = database.WithinTx(context.Background(), func(ctx context.Context) error {
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "database pool is not initialized") {
		t.Fatalf("expected nil pool error on WithinTx, got: %v", err)
	}

	err = database.WithRetryTx(context.Background(), 2, func(tx pgx.Tx) error {
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "database pool is not initialized") {
		t.Fatalf("expected nil pool error on WithRetryTx, got: %v", err)
	}
}

func TestTx_ContextPropagation_SingleDB(t *testing.T) {
	db := &postgres.Database{Pool: nil}
	ctx := context.Background()

	// Initial context has no tx
	tx, ok := db.TxFromContext(ctx)
	if ok || tx != nil {
		t.Fatal("expected no tx in empty context")
	}

	// TxFromContext on nil context
	tx, ok = db.TxFromContext(nil)
	if ok || tx != nil {
		t.Fatal("expected no tx in nil context")
	}

	// Inject mock tx into context for this db
	mock := &mockTx{}
	txCtx := postgres.ContextWithTx(ctx, db, mock)

	retrievedTx, ok := db.TxFromContext(txCtx)
	if !ok || retrievedTx != mock {
		t.Fatal("expected retrieved tx to match injected mock tx")
	}

	// db.DB(txCtx) returns the active mock tx
	dbtx, err := db.DB(txCtx)
	if err != nil {
		t.Fatalf("unexpected error from db.DB: %v", err)
	}
	if dbtx != mock {
		t.Fatal("expected db.DB to return active mock tx")
	}
}

func TestTx_MultiDatabase_Isolation(t *testing.T) {
	dbA := &postgres.Database{Pool: nil}
	dbB := &postgres.Database{Pool: nil}

	ctx := context.Background()
	mockA := &mockTx{}

	// 1. Transaction started on database A and retrieved through A
	txCtxA := postgres.ContextWithTx(ctx, dbA, mockA)
	txA, ok := dbA.TxFromContext(txCtxA)
	if !ok || txA != mockA {
		t.Fatalf("expected dbA to retrieve its own tx, got ok=%v, tx=%v", ok, txA)
	}
	dbtxA, err := dbA.DB(txCtxA)
	if err != nil || dbtxA != mockA {
		t.Fatalf("expected dbA.DB to return mockA, got err=%v, dbtx=%v", err, dbtxA)
	}

	// 2. Transaction started on A and accessed through B (rejected with conflict)
	txB, ok := dbB.TxFromContext(txCtxA)
	if ok || txB != nil {
		t.Fatalf("expected dbB to NOT retrieve dbA's tx, got ok=%v, tx=%v", ok, txB)
	}
	_, err = dbB.DB(txCtxA)
	if err == nil {
		t.Fatal("expected dbB.DB to reject cross-database tx context, got nil")
	}
	if !strings.Contains(err.Error(), "different database instance") {
		t.Fatalf("expected cross-database error, got: %v", err)
	}

	// 3. dbB.WithinTx(txCtxA) must detect cross-database transaction and reject immediately without executing callback
	callbackExecuted := false
	err = dbB.WithinTx(txCtxA, func(c context.Context) error {
		callbackExecuted = true
		return nil
	})
	if err == nil {
		t.Fatal("expected dbB.WithinTx to reject cross-database tx context, got nil")
	}
	if !strings.Contains(err.Error(), "cross-database transaction detected") {
		t.Fatalf("expected cross-database conflict error, got: %v", err)
	}
	if callbackExecuted {
		t.Fatal("expected callback NOT to execute when cross-database transaction is detected")
	}

	// 4. dbA.WithinTx(txCtxA) same-database nesting must succeed
	sameDBExecuted := false
	err = dbA.WithinTx(txCtxA, func(nestedCtx context.Context) error {
		sameDBExecuted = true
		tx, ok := dbA.TxFromContext(nestedCtx)
		if !ok || tx != mockA {
			t.Fatal("nested call failed to receive existing mock tx")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error on same-database nesting: %v", err)
	}
	if !sameDBExecuted {
		t.Fatal("expected same-database nested callback to execute")
	}

	// 5. Repository operations without active transactions
	cleanCtx := context.Background()
	txNone, okNone := dbA.TxFromContext(cleanCtx)
	if okNone || txNone != nil {
		t.Fatalf("expected no tx on clean context, got ok=%v, tx=%v", okNone, txNone)
	}
}

func TestTx_MultiDatabase_MultipleInjectionsRejected(t *testing.T) {
	dbA := &postgres.Database{Pool: nil}
	dbB := &postgres.Database{Pool: nil}

	ctx := context.Background()
	mockA := &mockTx{}
	mockB := &mockTx{}

	// Inject dbA then dbB into the same context to simulate uncoordinated multi-DB injection
	txCtx := postgres.ContextWithTx(ctx, dbA, mockA)
	txCtx = postgres.ContextWithTx(txCtx, dbB, mockB)

	// Neither dbA nor dbB should return a valid tx from the contaminated context
	if tx, ok := dbA.TxFromContext(txCtx); ok || tx != nil {
		t.Fatalf("expected dbA.TxFromContext to return false on contaminated multi-DB context, got ok=%v, tx=%v", ok, tx)
	}
	if tx, ok := dbB.TxFromContext(txCtx); ok || tx != nil {
		t.Fatalf("expected dbB.TxFromContext to return false on contaminated multi-DB context, got ok=%v, tx=%v", ok, tx)
	}

	// dbA.DB and dbB.DB must both reject with conflict error
	if _, err := dbA.DB(txCtx); err == nil || !strings.Contains(err.Error(), "multiple database instances") {
		t.Fatalf("expected dbA.DB to reject contaminated multi-DB context, got err=%v", err)
	}
	if _, err := dbB.DB(txCtx); err == nil || !strings.Contains(err.Error(), "multiple database instances") {
		t.Fatalf("expected dbB.DB to reject contaminated multi-DB context, got err=%v", err)
	}

	// dbA.WithinTx and dbB.WithinTx must both reject without executing callbacks
	fnExecuted := false
	if err := dbA.WithinTx(txCtx, func(c context.Context) error {
		fnExecuted = true
		return nil
	}); err == nil || !strings.Contains(err.Error(), "multiple database instances") {
		t.Fatalf("expected dbA.WithinTx to reject contaminated context, got err=%v", err)
	}
	if fnExecuted {
		t.Fatal("expected callback NOT to execute on contaminated context for dbA")
	}

	if err := dbB.WithinTx(txCtx, func(c context.Context) error {
		fnExecuted = true
		return nil
	}); err == nil || !strings.Contains(err.Error(), "multiple database instances") {
		t.Fatalf("expected dbB.WithinTx to reject contaminated context, got err=%v", err)
	}
	if fnExecuted {
		t.Fatal("expected callback NOT to execute on contaminated context for dbB")
	}
}

func TestTx_NestedTransaction_ErrorPropagation(t *testing.T) {
	db := &postgres.Database{Pool: nil}
	ctx := context.Background()
	mock := &mockTx{}

	txCtx := postgres.ContextWithTx(ctx, db, mock)

	expectedErr := errors.New("business rule failed in nested function")

	// Outer WithinTx participates with existing mock tx; inner function returns error
	err := db.WithinTx(txCtx, func(nestedCtx1 context.Context) error {
		// Call nested WithinTx simulating a sub-service call
		nestedErr := db.WithinTx(nestedCtx1, func(nestedCtx2 context.Context) error {
			// Sub-operation performs work then returns a business error
			return expectedErr
		})
		if nestedErr != nil {
			// Propagate error to outer transaction owner to ensure atomic rollback
			return nestedErr
		}
		return nil
	})

	if !errors.Is(err, expectedErr) {
		t.Fatalf("expected outer WithinTx to propagate nested error %v, got %v", expectedErr, err)
	}
}

func TestTx_WithinTx_IncompatibleOptionsRejection(t *testing.T) {
	db := &postgres.Database{Pool: nil}
	mock := &mockTx{}
	txCtx := postgres.ContextWithTx(context.Background(), db, mock)

	testCases := []struct {
		name string
		opts pgx.TxOptions
	}{
		{
			name: "Incompatible IsoLevel",
			opts: pgx.TxOptions{IsoLevel: pgx.Serializable},
		},
		{
			name: "Incompatible AccessMode",
			opts: pgx.TxOptions{AccessMode: pgx.ReadOnly},
		},
		{
			name: "Incompatible DeferrableMode",
			opts: pgx.TxOptions{DeferrableMode: pgx.Deferrable},
		},
		{
			name: "All incompatible options combined",
			opts: pgx.TxOptions{
				IsoLevel:       pgx.RepeatableRead,
				AccessMode:     pgx.ReadOnly,
				DeferrableMode: pgx.Deferrable,
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			executed := false
			err := db.WithinTxOptions(txCtx, tc.opts, func(c context.Context) error {
				executed = true
				return nil
			})
			if err == nil {
				t.Fatalf("expected error applying %s to existing tx, got nil", tc.name)
			}
			if !strings.Contains(err.Error(), "cannot apply transaction options") {
				t.Fatalf("expected descriptive incompatible options error, got: %v", err)
			}
			if executed {
				t.Fatal("expected callback NOT to be executed when incompatible options are provided")
			}
		})
	}
}

func TestTx_WithinTx_ReusesExistingTransaction(t *testing.T) {
	db := &postgres.Database{Pool: nil}
	mock := &mockTx{}
	txCtx := postgres.ContextWithTx(context.Background(), db, mock)

	// Calling WithinTx when already in tx should directly participate without error
	called := false
	err := db.WithinTx(txCtx, func(nestedCtx context.Context) error {
		called = true
		tx, ok := db.TxFromContext(nestedCtx)
		if !ok || tx != mock {
			t.Fatal("nested call failed to receive existing mock tx")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error from nested WithinTx: %v", err)
	}
	if !called {
		t.Fatal("expected nested WithinTx callback to be invoked")
	}
}

func TestTx_WithRetryTx_NonRetryableError(t *testing.T) {
	db := &postgres.Database{Pool: nil}

	// Nil pool error is non-retryable; should return immediately without retry delay
	start := time.Now()
	err := db.WithRetryTx(context.Background(), 5, func(tx pgx.Tx) error {
		return nil
	})
	duration := time.Since(start)

	if err == nil {
		t.Fatal("expected nil pool error, got nil")
	}
	if duration > 100*time.Millisecond {
		t.Errorf("WithRetryTx took %v; expected immediate return on non-retryable error", duration)
	}
}

func TestTx_WithRetryTx_ContextCancelled(t *testing.T) {
	db := &postgres.Database{Pool: nil}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	err := db.WithRetryTx(ctx, 3, func(tx pgx.Tx) error {
		return &pgconn.PgError{Code: "40001"} // Transient error
	})
	if err == nil {
		t.Fatal("expected error on cancelled context, got nil")
	}
	if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "database pool is not initialized") {
		t.Fatalf("expected context.Canceled or pool error, got: %v", err)
	}
}
