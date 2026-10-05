package ledger

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/svout/agentgate/gateway/internal/store"
	"github.com/svout/agentgate/gateway/internal/testutil"
)

// Fixed ids for the validation table: validation runs before any I/O, so the
// rows don't need to exist.
var (
	idA = uuid.UUID{1}
	idB = uuid.UUID{2}
	idP = uuid.UUID{3}
)

func TestPostRejectsInvalidPostings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		payment uuid.UUID
		entries []Entry
		want    error
	}{
		{"no legs", idP, nil, ErrTooFewLegs},
		{"single leg", idP, []Entry{{idA, Debit, 100}}, ErrTooFewLegs},
		{"unbalanced", idP, []Entry{{idA, Debit, 100}, {idB, Credit, 99}}, ErrUnbalanced},
		{"all debits", idP, []Entry{{idA, Debit, 100}, {idB, Debit, 100}}, ErrUnbalanced},
		{"zero amount", idP, []Entry{{idA, Debit, 0}, {idB, Credit, 0}}, ErrNonPositive},
		{"negative amount", idP, []Entry{{idA, Debit, -100}, {idB, Credit, -100}}, ErrNonPositive},
		{"bad direction", idP, []Entry{{idA, "DEBIT", 100}, {idB, Credit, 100}}, ErrInvalidDirection},
		{"empty direction", idP, []Entry{{idA, "", 100}, {idB, Credit, 100}}, ErrInvalidDirection},
		{"debit overflow", idP, []Entry{
			{idA, Debit, math.MaxInt64}, {idA, Debit, 1},
			{idB, Credit, math.MaxInt64}, {idB, Credit, 1},
		}, ErrOverflow},
		// With wrapping arithmetic both sides would sum to
		// MaxInt64+MaxInt64+2 = 0 and look balanced; the check must catch
		// the overflow before the sums are compared.
		{"overflow that would wrap to balanced", idP, []Entry{
			{idA, Debit, math.MaxInt64}, {idA, Debit, math.MaxInt64}, {idA, Debit, 2},
			{idB, Credit, math.MaxInt64}, {idB, Credit, math.MaxInt64}, {idB, Credit, 2},
		}, ErrOverflow},
		{"nil payment id", uuid.Nil, []Entry{{idA, Debit, 1}, {idB, Credit, 1}}, ErrMissingID},
		{"nil account id", idP, []Entry{{uuid.Nil, Debit, 1}, {idB, Credit, 1}}, ErrMissingID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// A nil tx is safe here: Post validates before any I/O, and a
			// test that reached the database would panic.
			err := Post(context.Background(), nil, tt.payment, tt.entries)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Post = %v, want %v", err, tt.want)
			}
			if !errors.Is(err, ErrInvalidPosting) {
				t.Errorf("Post = %v, want it to wrap ErrInvalidPosting", err)
			}
		})
	}
}

func TestValidateAcceptsLargeBalancedPosting(t *testing.T) {
	t.Parallel()
	err := validate(idP, []Entry{
		{idA, Debit, math.MaxInt64 - 1}, {idA, Debit, 1},
		{idB, Credit, math.MaxInt64},
	})
	if err != nil {
		t.Fatalf("validate = %v, want nil", err)
	}
}

func TestPostRejectsNonSerializableTx(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	ctx := testContext(t)
	f := newFixture(ctx, t, pool)

	for _, iso := range []pgx.TxIsoLevel{pgx.ReadCommitted, pgx.RepeatableRead} {
		t.Run(string(iso), func(t *testing.T) {
			err := pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{IsoLevel: iso}, func(tx pgx.Tx) error {
				return Post(ctx, tx, f.payment, f.balanced(500))
			})
			if !errors.Is(err, ErrNotSerializable) {
				t.Fatalf("Post = %v, want ErrNotSerializable", err)
			}
			if n := countEntries(ctx, t, pool, f.payment); n != 0 {
				t.Errorf("entries = %d, want 0", n)
			}
		})
	}
}

func TestPostWritesBalancedLegs(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	ctx := testContext(t)
	f := newFixture(ctx, t, pool)
	clearing := newAccount(ctx, t, pool, "clearing")

	// Three legs: the payer pays 1000, of which the payee gets 970 and the
	// platform's clearing account 30.
	entries := []Entry{
		{f.payer, Debit, 1000},
		{f.payee, Credit, 970},
		{clearing, Credit, 30},
	}
	err := store.Serializable(ctx, pool, func(tx pgx.Tx) error {
		return Post(ctx, tx, f.payment, entries)
	})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}

	if n := countEntries(ctx, t, pool, f.payment); n != len(entries) {
		t.Errorf("entries = %d, want %d", n, len(entries))
	}
	if signed := paymentSum(ctx, t, pool, f.payment); signed != 0 {
		t.Errorf("signed sum for payment = %d, want 0", signed)
	}
}

func TestFailedPostingLeavesNoRows(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	ctx := testContext(t)
	f := newFixture(ctx, t, pool)
	later := errors.New("failure after posting")

	tests := []struct {
		name string
		fn   func(tx pgx.Tx) error
		want error
	}{
		{"unbalanced post", func(tx pgx.Tx) error {
			return Post(ctx, tx, f.payment, []Entry{{f.payer, Debit, 100}, {f.payee, Credit, 90}})
		}, ErrUnbalanced},
		// The legs are inserted, then the caller's transaction fails: the
		// rollback must take the legs with it.
		{"failure after a good post", func(tx pgx.Tx) error {
			if err := Post(ctx, tx, f.payment, f.balanced(100)); err != nil {
				return err
			}
			return later
		}, later},
		// A leg referencing a missing account fails inside the INSERT; no
		// other leg of the same statement may survive.
		{"unknown account", func(tx pgx.Tx) error {
			missing := uuid.UUID{0xff}
			return Post(ctx, tx, f.payment, []Entry{{f.payer, Debit, 100}, {missing, Credit, 100}})
		}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := store.Serializable(ctx, pool, tt.fn)
			if err == nil {
				t.Fatal("Serializable = nil, want an error")
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
			if n := countEntries(ctx, t, pool, f.payment); n != 0 {
				t.Errorf("entries = %d, want 0", n)
			}
		})
	}
}

func TestBalanceAcrossPostings(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	ctx := testContext(t)
	user := newAccount(ctx, t, pool, "user")
	merchant := newAccount(ctx, t, pool, "merchant")
	clearing := newAccount(ctx, t, pool, "clearing")
	idle := newAccount(ctx, t, pool, "user")

	// Fund the user from clearing, then two purchases with a fee, then a
	// partial refund: every kind ends with credits − debits.
	postings := [][]Entry{
		{{clearing, Debit, 10_000}, {user, Credit, 10_000}},
		{{user, Debit, 2_500}, {merchant, Credit, 2_400}, {clearing, Credit, 100}},
		{{user, Debit, 1_000}, {merchant, Credit, 1_000}},
		{{merchant, Debit, 400}, {user, Credit, 400}},
	}
	for i, legs := range postings {
		payment := newPayment(ctx, t, pool, user, merchant, 1)
		err := store.Serializable(ctx, pool, func(tx pgx.Tx) error {
			return Post(ctx, tx, payment, legs)
		})
		if err != nil {
			t.Fatalf("posting %d: %v", i, err)
		}
	}

	want := map[string]struct {
		id      uuid.UUID
		balance int64
	}{
		"user":     {user, 10_000 - 2_500 - 1_000 + 400},
		"merchant": {merchant, 2_400 + 1_000 - 400},
		"clearing": {clearing, -10_000 + 100},
		"idle":     {idle, 0},
	}
	var total int64
	for name, w := range want {
		got, err := Balance(ctx, pool, w.id)
		if err != nil {
			t.Fatalf("Balance(%s): %v", name, err)
		}
		if got != w.balance {
			t.Errorf("Balance(%s) = %d, want %d", name, got, w.balance)
		}
		total += got
	}
	if total != 0 {
		t.Errorf("sum of all balances = %d, want 0", total)
	}

	// Balance also works inside a transaction (the Querier interface).
	err := store.Serializable(ctx, pool, func(tx pgx.Tx) error {
		got, err := Balance(ctx, tx, user)
		if err == nil && got != want["user"].balance {
			t.Errorf("Balance(user) in tx = %d, want %d", got, want["user"].balance)
		}
		return err
	})
	if err != nil {
		t.Fatalf("Balance in tx: %v", err)
	}
}

// fixture is a payer, a payee and a payment between them.
type fixture struct {
	payer, payee, payment uuid.UUID
}

func newFixture(ctx context.Context, t *testing.T, pool *pgxpool.Pool) fixture {
	t.Helper()
	payer := newAccount(ctx, t, pool, "user")
	payee := newAccount(ctx, t, pool, "merchant")
	return fixture{payer: payer, payee: payee, payment: newPayment(ctx, t, pool, payer, payee, 500)}
}

// balanced is the simple two-leg posting of amount from payer to payee.
func (f fixture) balanced(amount int64) []Entry {
	return []Entry{{f.payer, Debit, amount}, {f.payee, Credit, amount}}
}

// The helpers below talk to the database directly, so they convert at the SQL
// boundary the same way ledger does (pgUUID, and back with uuid.UUID(id.Bytes)).
func newAccount(ctx context.Context, t *testing.T, pool *pgxpool.Pool, kind string) uuid.UUID {
	t.Helper()
	var id pgtype.UUID
	err := pool.QueryRow(ctx, "INSERT INTO accounts (kind, currency) VALUES ($1, 'GBP') RETURNING id", kind).Scan(&id)
	if err != nil {
		t.Fatalf("insert account: %v", err)
	}
	return uuid.UUID(id.Bytes)
}

func newPayment(ctx context.Context, t *testing.T, pool *pgxpool.Pool, payer, payee uuid.UUID, amount int64) uuid.UUID {
	t.Helper()
	var id pgtype.UUID
	err := pool.QueryRow(ctx, `
		INSERT INTO payments (payer_account_id, payee_account_id, amount, currency, rail)
		VALUES ($1, $2, $3, 'GBP', 'internal') RETURNING id`, pgUUID(payer), pgUUID(payee), amount).Scan(&id)
	if err != nil {
		t.Fatalf("insert payment: %v", err)
	}
	return uuid.UUID(id.Bytes)
}

func countEntries(ctx context.Context, t *testing.T, pool *pgxpool.Pool, payment uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM ledger_entries WHERE payment_id = $1", pgUUID(payment)).Scan(&n); err != nil {
		t.Fatalf("count entries: %v", err)
	}
	return n
}

// paymentSum is the signed sum (credits − debits) of payment's entries: 0 for
// a balanced payment, and for one with no entries.
func paymentSum(ctx context.Context, t *testing.T, pool *pgxpool.Pool, payment uuid.UUID) int64 {
	t.Helper()
	var signed int64
	err := pool.QueryRow(ctx, `
		SELECT COALESCE(sum(CASE direction WHEN 'credit' THEN amount ELSE -amount END), 0)::bigint
		FROM ledger_entries WHERE payment_id = $1`, pgUUID(payment)).Scan(&signed)
	if err != nil {
		t.Fatalf("signed sum: %v", err)
	}
	return signed
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}
