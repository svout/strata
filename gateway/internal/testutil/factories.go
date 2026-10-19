package testutil

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Row factories for tests that need rows another package owns (a rail test
// needs a payment to settle). They insert directly, so a test doesn't depend
// on that package's API; nothing outside tests may use them.

// NewAccount inserts a GBP account of kind (user, merchant or clearing) and
// returns its id.
func NewAccount(t testing.TB, pool *pgxpool.Pool, kind string) uuid.UUID {
	t.Helper()
	return insertID(t, pool, "INSERT INTO accounts (kind, currency) VALUES ($1, 'GBP') RETURNING id", kind)
}

// NewPayment inserts a GBP payment of amount from payer to payee, in state
// created on the internal rail, and returns its id.
func NewPayment(t testing.TB, pool *pgxpool.Pool, payer, payee uuid.UUID, amount int64) uuid.UUID {
	t.Helper()
	return insertID(t, pool, `
		INSERT INTO payments (payer_account_id, payee_account_id, amount, currency, rail)
		VALUES ($1, $2, $3, 'GBP', 'internal') RETURNING id`,
		pgtype.UUID{Bytes: payer, Valid: true}, pgtype.UUID{Bytes: payee, Valid: true}, amount)
}

func insertID(t testing.TB, pool *pgxpool.Pool, sql string, args ...any) uuid.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := pool.QueryRow(t.Context(), sql, args...).Scan(&id); err != nil {
		t.Fatalf("testutil: insert: %v", err)
	}
	return uuid.UUID(id.Bytes)
}
