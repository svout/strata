package testutil

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Ledger checks for tests. They read ledger_entries directly, as the row
// factories write rows directly, so a property in any package can check
// money conservation without going through the ledger's API; nothing
// outside tests may use them.

// LedgerSums checks ledger_entries directly: unbalanced is how many postings
// (entries sharing a payment and a rail reference) don't sum to zero, and
// total is the signed sum of every entry. Entries posted without a reference
// (ledger.Post) form one group per payment, so there a payment's postings
// are checked together. With payments given it looks only at theirs; with
// none it checks the whole table, and that includes a caller passing an
// empty list (stricter, never weaker). It returns an error rather than
// taking a testing.TB so a rapid property can fail through its own *rapid.T
// and keep the shrunk case.
//
// Cost: the "no ids or ANY(ids)" filter may keep the planner off the
// payment_id index, so a call can scan the whole table. Acceptable at the
// sizes the properties reach today; if the suite gets slow, look here first.
func LedgerSums(ctx context.Context, pool *pgxpool.Pool, payments ...uuid.UUID) (unbalanced int, total int64, err error) {
	ids := make([]pgtype.UUID, len(payments))
	for i, id := range payments {
		ids[i] = pgtype.UUID{Bytes: id, Valid: true}
	}
	err = pool.QueryRow(ctx, `
		WITH scoped AS (
			SELECT payment_id, rail_ref, CASE direction WHEN 'credit' THEN amount ELSE -amount END AS signed
			FROM ledger_entries
			WHERE cardinality($1::uuid[]) = 0 OR payment_id = ANY($1))
		SELECT
			(SELECT count(*) FROM (
				SELECT 1 FROM scoped GROUP BY payment_id, rail_ref HAVING sum(signed) <> 0) u),
			(SELECT COALESCE(sum(signed), 0)::bigint FROM scoped)`, ids).Scan(&unbalanced, &total)
	return unbalanced, total, err
}
