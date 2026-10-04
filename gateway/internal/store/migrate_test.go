// External test package: testutil imports store (to migrate test databases),
// so an in-package test importing testutil would be an import cycle.
package store_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/svout/agentgate/gateway/internal/store"
	"github.com/svout/agentgate/gateway/internal/testutil"
)

var schemaTables = []string{"accounts", "payments", "ledger_entries", "idempotency_keys"}

// PostgreSQL SQLSTATE codes the schema's constraints raise.
const (
	notNullViolation    = "23502"
	foreignKeyViolation = "23503"
	uniqueViolation     = "23505"
	checkViolation      = "23514"
)

func TestUpAppliesSchemaV1(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t) // already migrated with store.Up
	ctx := testContext(t)

	var version int64
	if err := pool.QueryRow(ctx, "SELECT max(version_id) FROM goose_db_version").Scan(&version); err != nil {
		t.Fatalf("read goose version: %v", err)
	}
	if version != 1 {
		t.Errorf("schema version = %d, want 1", version)
	}
	for _, table := range schemaTables {
		if !tableExists(ctx, t, pool, table) {
			t.Errorf("table %s missing after Up", table)
		}
	}

	// Up is idempotent: a second run has nothing to apply and must not fail.
	if err := store.Up(ctx, sqlDB(t, pool)); err != nil {
		t.Errorf("second Up: %v", err)
	}
}

func TestDownRemovesSchemaAndUpRestoresIt(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	ctx := testContext(t)
	db := sqlDB(t, pool)

	if err := store.Down(ctx, db); err != nil {
		t.Fatalf("Down: %v", err)
	}
	for _, table := range schemaTables {
		if tableExists(ctx, t, pool, table) {
			t.Errorf("table %s still exists after Down", table)
		}
	}

	if err := store.Up(ctx, db); err != nil {
		t.Fatalf("Up after Down: %v", err)
	}
	for _, table := range schemaTables {
		if !tableExists(ctx, t, pool, table) {
			t.Errorf("table %s missing after Up following Down", table)
		}
	}
}

// TestSchemaConstraints inserts one valid row per table, then checks that each
// kind of bad row is rejected by the database itself with the expected
// SQLSTATE, independent of any Go validation added later.
func TestSchemaConstraints(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	ctx := testContext(t)

	// Valid baseline rows. A clearing account has no owner (NULL).
	payer := insertID(ctx, t, pool, `INSERT INTO accounts (owner, kind, currency) VALUES ('user-1', 'user', 'GBP') RETURNING id::text`)
	payee := insertID(ctx, t, pool, `INSERT INTO accounts (owner, kind, currency) VALUES ('merchant-1', 'merchant', 'GBP') RETURNING id::text`)
	insertID(ctx, t, pool, `INSERT INTO accounts (kind, currency) VALUES ('clearing', 'GBP') RETURNING id::text`)
	payment := insertID(ctx, t, pool,
		`INSERT INTO payments (payer_account_id, payee_account_id, amount, currency, rail)
		 VALUES ($1, $2, 1250, 'GBP', 'internal') RETURNING id::text`, payer, payee)
	mustExec(ctx, t, pool,
		`INSERT INTO ledger_entries (payment_id, account_id, direction, amount) VALUES ($1, $2, 'debit', 1250), ($1, $3, 'credit', 1250)`,
		payment, payer, payee)
	mustExec(ctx, t, pool,
		`INSERT INTO idempotency_keys (key, request_hash, status) VALUES ('key-1', $1, 'in_progress')`, hash(32))
	// The same key under another scope is a different record (P2 per-agent scoping).
	mustExec(ctx, t, pool,
		`INSERT INTO idempotency_keys (scope, key, request_hash, status) VALUES ('agent-a', 'key-1', $1, 'in_progress')`, hash(32))
	// A completed key carries the response to replay.
	mustExec(ctx, t, pool,
		`INSERT INTO idempotency_keys (key, request_hash, status, response_code, response_body) VALUES ('key-done', $1, 'completed', 201, $2)`,
		hash(32), []byte(`{"id":"p1"}`))

	var state string
	if err := pool.QueryRow(ctx, `SELECT state FROM payments WHERE id = $1`, payment).Scan(&state); err != nil {
		t.Fatalf("read payment state: %v", err)
	}
	if state != "created" {
		t.Errorf("default payment state = %q, want created", state)
	}

	const missing = "00000000-0000-0000-0000-000000000000" // no row has this id

	tests := []struct {
		name     string
		sql      string
		args     []any
		wantCode string
	}{
		// accounts
		{"account bad kind", `INSERT INTO accounts (kind, currency) VALUES ('bank', 'GBP')`, nil, checkViolation},
		{"account non-GBP", `INSERT INTO accounts (kind, currency) VALUES ('user', 'USD')`, nil, checkViolation},
		{"account no kind", `INSERT INTO accounts (currency) VALUES ('GBP')`, nil, notNullViolation},

		// payments
		{"payment zero amount", `INSERT INTO payments (payer_account_id, payee_account_id, amount, currency, rail) VALUES ($1, $2, 0, 'GBP', 'internal')`, []any{payer, payee}, checkViolation},
		{"payment negative amount", `INSERT INTO payments (payer_account_id, payee_account_id, amount, currency, rail) VALUES ($1, $2, -5, 'GBP', 'internal')`, []any{payer, payee}, checkViolation},
		{"payment non-GBP", `INSERT INTO payments (payer_account_id, payee_account_id, amount, currency, rail) VALUES ($1, $2, 100, 'EUR', 'internal')`, []any{payer, payee}, checkViolation},
		{"payment bad state", `INSERT INTO payments (payer_account_id, payee_account_id, amount, currency, rail, state) VALUES ($1, $2, 100, 'GBP', 'internal', 'pending')`, []any{payer, payee}, checkViolation},
		{"payment no rail", `INSERT INTO payments (payer_account_id, payee_account_id, amount, currency) VALUES ($1, $2, 100, 'GBP')`, []any{payer, payee}, notNullViolation},
		{"payment dangling payer", `INSERT INTO payments (payer_account_id, payee_account_id, amount, currency, rail) VALUES ($1, $2, 100, 'GBP', 'internal')`, []any{missing, payee}, foreignKeyViolation},
		{"payment to itself", `INSERT INTO payments (payer_account_id, payee_account_id, amount, currency, rail) VALUES ($1, $1, 100, 'GBP', 'internal')`, []any{payer}, checkViolation},
		{"payment dangling payee", `INSERT INTO payments (payer_account_id, payee_account_id, amount, currency, rail) VALUES ($1, $2, 100, 'GBP', 'internal')`, []any{payer, missing}, foreignKeyViolation},

		// ledger_entries
		{"entry bad direction", `INSERT INTO ledger_entries (payment_id, account_id, direction, amount) VALUES ($1, $2, 'up', 100)`, []any{payment, payer}, checkViolation},
		{"entry zero amount", `INSERT INTO ledger_entries (payment_id, account_id, direction, amount) VALUES ($1, $2, 'debit', 0)`, []any{payment, payer}, checkViolation},
		{"entry no payment", `INSERT INTO ledger_entries (account_id, direction, amount) VALUES ($1, 'debit', 100)`, []any{payer}, notNullViolation},
		{"entry no direction", `INSERT INTO ledger_entries (payment_id, account_id, amount) VALUES ($1, $2, 100)`, []any{payment, payer}, notNullViolation},
		{"entry dangling payment", `INSERT INTO ledger_entries (payment_id, account_id, direction, amount) VALUES ($1, $2, 'debit', 100)`, []any{missing, payer}, foreignKeyViolation},
		{"entry dangling account", `INSERT INTO ledger_entries (payment_id, account_id, direction, amount) VALUES ($1, $2, 'debit', 100)`, []any{payment, missing}, foreignKeyViolation},

		// idempotency_keys
		{"idempotency duplicate scope+key", `INSERT INTO idempotency_keys (key, request_hash, status) VALUES ('key-1', $1, 'in_progress')`, []any{hash(32)}, uniqueViolation},
		{"idempotency empty key", `INSERT INTO idempotency_keys (key, request_hash, status) VALUES ('', $1, 'in_progress')`, []any{hash(32)}, checkViolation},
		{"idempotency key too long", `INSERT INTO idempotency_keys (key, request_hash, status) VALUES ($1, $2, 'in_progress')`, []any{strings.Repeat("k", 256), hash(32)}, checkViolation},
		{"idempotency short hash", `INSERT INTO idempotency_keys (key, request_hash, status) VALUES ('key-2', $1, 'in_progress')`, []any{hash(31)}, checkViolation},
		{"idempotency bad status", `INSERT INTO idempotency_keys (key, request_hash, status) VALUES ('key-3', $1, 'done')`, []any{hash(32)}, checkViolation},
		{"idempotency completed without response", `INSERT INTO idempotency_keys (key, request_hash, status) VALUES ('key-4', $1, 'completed')`, []any{hash(32)}, checkViolation},
		{"idempotency in_progress with response", `INSERT INTO idempotency_keys (key, request_hash, status, response_code) VALUES ('key-5', $1, 'in_progress', 201)`, []any{hash(32)}, checkViolation},
		{"idempotency response code 99", `INSERT INTO idempotency_keys (key, request_hash, status, response_code) VALUES ('key-6', $1, 'completed', 99)`, []any{hash(32)}, checkViolation},
		{"idempotency response code 600", `INSERT INTO idempotency_keys (key, request_hash, status, response_code) VALUES ('key-7', $1, 'completed', 600)`, []any{hash(32)}, checkViolation},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := pool.Exec(ctx, tt.sql, tt.args...)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) {
				t.Fatalf("got err %v, want PostgreSQL error %s", err, tt.wantCode)
			}
			if pgErr.Code != tt.wantCode {
				t.Errorf("SQLSTATE = %s (%s), want %s", pgErr.Code, pgErr.Message, tt.wantCode)
			}
		})
	}

	// A 255-character key is the upper bound and must be accepted.
	mustExec(ctx, t, pool,
		`INSERT INTO idempotency_keys (key, request_hash, status) VALUES ($1, $2, 'in_progress')`,
		strings.Repeat("k", 255), hash(32))
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// sqlDB wraps pool for goose, which works over database/sql.
func sqlDB(t *testing.T, pool *pgxpool.Pool) *sql.DB {
	t.Helper()
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func tableExists(ctx context.Context, t *testing.T, pool *pgxpool.Pool, table string) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('public.' || $1) IS NOT NULL", table).Scan(&exists); err != nil {
		t.Fatalf("check table %s: %v", table, err)
	}
	return exists
}

func insertID(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sql string, args ...any) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
		t.Fatalf("insert valid row: %v", err)
	}
	return id
}

func mustExec(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("exec valid statement: %v", err)
	}
}

// hash returns n bytes standing in for a SHA-256 digest.
func hash(n int) []byte { return make([]byte, n) }
