package ledger

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/svout/strata/gateway/internal/store"
	"github.com/svout/strata/gateway/internal/testutil"
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

// PostRef numbers its legs and records the reference; Posted sees it; a
// second posting with the reference writes nothing (ADR-120).
func TestPostRefPostsOnce(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	ctx := testContext(t)
	f := newFixture(ctx, t, pool)
	const ref = "pay_ref_once"

	if posted, err := Posted(ctx, pool, ref); err != nil || posted {
		t.Fatalf("Posted before = %v, %v; want false", posted, err)
	}
	postRef := func() error {
		return store.Serializable(ctx, pool, func(tx pgx.Tx) error { return PostRef(ctx, tx, f.payment, ref, f.balanced(500)) })
	}
	if err := postRef(); err != nil {
		t.Fatalf("PostRef: %v", err)
	}
	if posted, err := Posted(ctx, pool, ref); err != nil || !posted {
		t.Fatalf("Posted after = %v, %v; want true", posted, err)
	}
	if posted, err := Posted(ctx, pool, "pay_ref_other"); err != nil || posted {
		t.Fatalf("Posted(other ref) = %v, %v; want false", posted, err)
	}
	var legs []int
	rows, err := pool.Query(ctx, "SELECT leg FROM ledger_entries WHERE rail_ref = $1 ORDER BY leg", ref)
	if err == nil {
		legs, err = pgx.CollectRows(rows, pgx.RowTo[int])
	}
	if err != nil || len(legs) != 2 || legs[0] != 1 || legs[1] != 2 {
		t.Fatalf("legs = %v (%v), want [1 2]", legs, err)
	}

	if err := postRef(); !errors.Is(err, ErrAlreadyPosted) {
		t.Fatalf("second PostRef = %v, want ErrAlreadyPosted", err)
	}
	if n := countEntries(ctx, t, pool, f.payment); n != 2 {
		t.Fatalf("entries = %d after a repeated PostRef, want 2", n)
	}
	// Post without a reference is unaffected (refunds), and leaves both
	// columns NULL.
	if err := store.Serializable(ctx, pool, func(tx pgx.Tx) error { return Post(ctx, tx, f.payment, f.balanced(500)) }); err != nil {
		t.Fatalf("Post after PostRef: %v", err)
	}
	var unref int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM ledger_entries WHERE payment_id = $1 AND rail_ref IS NULL AND leg IS NULL", pgUUID(f.payment)).Scan(&unref); err != nil || unref != 2 {
		t.Fatalf("unreferenced legs = %d (%v), want 2", unref, err)
	}
}

// The database is the guarantee, not Posted: concurrent PostRef calls with
// one reference and no Posted check still leave exactly one posting.
func TestPostRefConstraintHoldsUnderRace(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	ctx := testContext(t)
	f := newFixture(ctx, t, pool)
	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			errs[i] = store.Serializable(ctx, pool, func(tx pgx.Tx) error { return PostRef(ctx, tx, f.payment, "pay_ref_race", f.balanced(500)) })
		})
	}
	close(start)
	wg.Wait()
	ok := 0
	for i, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrAlreadyPosted):
			// A loser sees the constraint: 23505 at once, or one 40001 that
			// store.Serializable retries into 23505. Without a Posted read
			// there are no predicate locks to keep conflicting on, so
			// ErrRetryLimit here would be a regression.
		default:
			t.Errorf("PostRef %d: %v", i, err)
		}
	}
	if ok != 1 {
		t.Errorf("%d PostRef calls succeeded, want 1", ok)
	}
	if got := countEntries(ctx, t, pool, f.payment); got != 2 {
		t.Fatalf("entries = %d, want 2 (one posting)", got)
	}
}

func TestPostRefRejectsBadReference(t *testing.T) {
	t.Parallel()
	for _, ref := range []string{"", strings.Repeat("r", 256)} {
		// Validation runs before any I/O, so a nil tx is never touched.
		if err := PostRef(t.Context(), nil, idP, ref, []Entry{{idA, Debit, 1}, {idB, Credit, 1}}); !errors.Is(err, ErrMissingRef) || !errors.Is(err, ErrInvalidPosting) {
			t.Errorf("PostRef(%d-char ref) = %v, want ErrMissingRef", len(ref), err)
		}
	}
}

// Migration 00005 seeds one clearing account per rail; ClearingAccount finds
// each by its 'rail:<name>' owner and fails, naming the rail, for a rail that
// has none.
func TestClearingAccount(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	ctx := t.Context()

	internal, err := ClearingAccount(ctx, pool, "internal")
	if err != nil || internal == (ClearingAccountID{}) {
		t.Fatalf("ClearingAccount(internal) = %v, %v; want the seeded account", internal, err)
	}
	fake, err := ClearingAccount(ctx, pool, "fake")
	if err != nil || fake == (ClearingAccountID{}) || fake == internal {
		t.Fatalf("ClearingAccount(fake) = %v, %v; want a second, distinct account (internal is %v)", fake, err, internal)
	}

	// A non-clearing account with a rail-shaped owner doesn't count: the
	// lookup is by kind as well as owner.
	if _, err := pool.Exec(ctx, "INSERT INTO accounts (owner, kind, currency) VALUES ('rail:stripe', 'merchant', 'GBP')"); err != nil {
		t.Fatalf("insert merchant account: %v", err)
	}
	// A rail whose seeded account is gone fails the same way (what a missing
	// migration or a hand-deleted row looks like to the service at startup).
	if _, err := pool.Exec(ctx, "DELETE FROM accounts WHERE owner = 'rail:fake'"); err != nil {
		t.Fatalf("delete fake clearing account: %v", err)
	}
	for _, railName := range []string{"stripe", "fake"} {
		_, err := ClearingAccount(ctx, pool, railName)
		if !errors.Is(err, ErrNoClearingAccount) {
			t.Errorf("ClearingAccount(%s) = %v, want ErrNoClearingAccount", railName, err)
			continue
		}
		// The error is what an operator sees when the service refuses to
		// start, so it must say which rail and which account is missing.
		if msg := err.Error(); !strings.Contains(msg, `"`+railName+`"`) || !strings.Contains(msg, `"rail:`+railName+`"`) {
			t.Errorf("error %q doesn't name rail %q and owner rail:%s", msg, railName, railName)
		}
		// A missing account is a configuration error, never the transient
		// kind a caller would retry.
		if errors.Is(err, ErrClearingLookup) {
			t.Errorf("ClearingAccount(%s) = %v, also matches ErrClearingLookup", railName, err)
		}
	}
}

// failingRow is a pgx.Row whose Scan fails with err.
type failingRow struct{ err error }

func (r failingRow) Scan(...any) error { return r.err }

// failingQuerier is a Querier whose every query fails with err.
type failingQuerier struct{ err error }

func (q failingQuerier) QueryRow(context.Context, string, ...any) pgx.Row { return failingRow(q) }

// When the lookup itself fails, ClearingAccount returns ErrClearingLookup,
// not ErrNoClearingAccount, so a caller at startup retries instead of exiting;
// and its text carries none of the driver's error (which names host, user
// and database for a connection failure), while errors.Is/As still reach it.
func TestClearingAccountLookupFailure(t *testing.T) {
	t.Parallel()
	// Shaped like a pgconn connect error: the detail that must not be printed.
	cause := errors.New(`failed to connect to host=db.internal.example user=strata database=strata: dial error`)

	_, err := ClearingAccount(t.Context(), failingQuerier{cause}, "internal")
	if !errors.Is(err, ErrClearingLookup) {
		t.Fatalf("ClearingAccount on a failing database = %v, want ErrClearingLookup", err)
	}
	if errors.Is(err, ErrNoClearingAccount) {
		t.Errorf("ClearingAccount on a failing database = %v, also matches ErrNoClearingAccount (a config error)", err)
	}
	if !errors.Is(err, cause) {
		t.Errorf("ClearingAccount's error doesn't unwrap to the database error")
	}
	if msg := err.Error(); msg != ErrClearingLookup.Error() {
		t.Errorf("error text = %q, want only %q (no driver detail)", msg, ErrClearingLookup.Error())
	}
}

// The case a fresh `make dev` stack hits before `make migrate`: the database
// answers but has no accounts table. That is the transient kind too.
func TestClearingAccountBeforeMigration(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	ctx := t.Context()
	if _, err := pool.Exec(ctx, "DROP TABLE accounts CASCADE"); err != nil {
		t.Fatalf("drop accounts: %v", err)
	}
	_, err := ClearingAccount(ctx, pool, "internal")
	if !errors.Is(err, ErrClearingLookup) || errors.Is(err, ErrNoClearingAccount) {
		t.Fatalf("ClearingAccount without an accounts table = %v, want ErrClearingLookup only", err)
	}
	// PostgreSQL's text here is `relation "accounts" does not exist`.
	if strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error text %q carries the database's message", err.Error())
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
