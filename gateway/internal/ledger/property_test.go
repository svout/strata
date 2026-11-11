package ledger

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"pgregory.net/rapid"

	"github.com/svout/strata/gateway/internal/store"
	"github.com/svout/strata/gateway/internal/testutil"
)

// Property tests for SPEC-002 AC1 (money conservation) and AC2 (no partial
// postings). rapid runs each property, and each AC2 fault kind, 100 times by
// default (20 under -short; see -rapid.checks), shrinking any failure to a
// minimal case.

// --- AC1: money conservation ------------------------------------------------

// opsPerCheck operations per rapid check; 100 checks x 100 = AC1's 10,000.
const (
	opsPerCheck = 100
	minTotalOps = 10_000
)

type opKind int

const (
	opCreate opKind = iota // insert a payments row; no entries
	opSettle               // post payer -> payee, with an optional fee leg to clearing
	opFail                 // a created payment fails; no entries
	opRefund               // post the exact reverse of a settled payment's legs
)

func (k opKind) String() string {
	return [...]string{"create", "settle", "fail", "refund"}[k]
}

// op is one drawn operation. settle, fail and refund act on an existing
// payment in the right state, chosen by pick; when there is none, the op
// creates a payment instead, so every drawn op does something.
type op struct {
	kind   opKind
	amount int64 // payment amount, for create
	fee    int64 // fee for settle, reduced modulo the payment amount; 0 = no fee leg
	pick   int
}

var opGen = rapid.Custom(func(t *rapid.T) op {
	o := op{
		kind:   rapid.SampledFrom([]opKind{opCreate, opSettle, opFail, opRefund}).Draw(t, "kind"),
		amount: rapid.Int64Range(1, 1_000_000).Draw(t, "amount"),
		pick:   rapid.IntRange(0, 1<<16).Draw(t, "pick"),
	}
	if rapid.Bool().Draw(t, "withFee") {
		o.fee = rapid.Int64Range(1, 1_000_000).Draw(t, "fee")
	}
	return o
})

type paymentState int

const (
	stateCreated paymentState = iota
	stateSettled
	stateFailed
	stateRefunded
)

// modelPayment is the test's own record of a payment and the legs posted
// when it settled (a refund posts their reverse).
type modelPayment struct {
	id     uuid.UUID
	amount int64
	state  paymentState
	legs   []Entry
}

func TestPropertyMoneyConservation(t *testing.T) {
	t.Parallel()
	// One database for every check: each check makes its own accounts, so
	// checks don't interfere, and the global check below sees all of them.
	pool := testutil.Postgres(t)
	ctx := t.Context()
	var ops atomic.Int64

	rapid.Check(t, func(rt *rapid.T) {
		user := newAccount(ctx, t, pool, "user")
		merchant := newAccount(ctx, t, pool, "merchant")
		clearing := newAccount(ctx, t, pool, "clearing")
		want := map[uuid.UUID]int64{user: 0, merchant: 0, clearing: 0}
		var payments []*modelPayment

		post := func(p *modelPayment, legs []Entry) {
			err := store.Serializable(ctx, pool, func(tx pgx.Tx) error {
				return Post(ctx, tx, p.id, legs)
			})
			if err != nil {
				rt.Fatalf("post for payment %v: %v", p.id, err)
			}
			for _, l := range legs {
				if l.Direction == Credit {
					want[l.AccountID] += l.Amount
				} else {
					want[l.AccountID] -= l.Amount
				}
			}
		}
		// inState returns a payment in state s chosen by pick, or nil.
		inState := func(s paymentState, pick int) *modelPayment {
			var match []*modelPayment
			for _, p := range payments {
				if p.state == s {
					match = append(match, p)
				}
			}
			if len(match) == 0 {
				return nil
			}
			return match[pick%len(match)]
		}

		for _, o := range rapid.SliceOfN(opGen, opsPerCheck, opsPerCheck).Draw(rt, "ops") {
			var p *modelPayment
			switch o.kind {
			case opSettle, opFail:
				p = inState(stateCreated, o.pick)
			case opRefund:
				p = inState(stateSettled, o.pick)
			}
			kind := o.kind
			if kind != opCreate && p == nil {
				kind = opCreate
			}

			switch kind {
			case opCreate:
				id := newPayment(ctx, t, pool, user, merchant, o.amount)
				payments = append(payments, &modelPayment{id: id, amount: o.amount})
			case opSettle:
				fee := o.fee % p.amount
				legs := []Entry{{user, Debit, p.amount}, {merchant, Credit, p.amount - fee}}
				if fee > 0 {
					legs = append(legs, Entry{clearing, Credit, fee})
				}
				post(p, legs)
				p.legs, p.state = legs, stateSettled
			case opFail:
				p.state = stateFailed
			case opRefund:
				// Refunds post on the settled payment's own payment_id.
				reverse := make([]Entry, len(p.legs))
				for i, l := range p.legs {
					reverse[i] = l
					reverse[i].Direction = Debit
					if l.Direction == Debit {
						reverse[i].Direction = Credit
					}
				}
				post(p, reverse)
				p.state = stateRefunded
			}
			ops.Add(1)
		}

		for _, p := range payments {
			if s := paymentSum(ctx, t, pool, p.id); s != 0 {
				rt.Errorf("payment %v (state %d): signed sum = %d, want 0", p.id, p.state, s)
			}
			if p.state == stateCreated || p.state == stateFailed {
				if n := countEntries(ctx, t, pool, p.id); n != 0 {
					rt.Errorf("payment %v (state %d): %d entries, want 0", p.id, p.state, n)
				}
			}
		}
		var total int64
		for id, w := range want {
			got, err := Balance(ctx, pool, id)
			if err != nil {
				rt.Fatalf("Balance: %v", err)
			}
			if got != w {
				rt.Errorf("Balance(%v) = %d, want %d", id, got, w)
			}
			total += got
		}
		if total != 0 {
			rt.Errorf("sum of balances = %d, want 0", total)
		}
	})

	// Across everything every check wrote: no posting is out of balance,
	// and all entries together sum to zero.
	unbalanced, total, err := testutil.LedgerSums(ctx, pool)
	if err != nil {
		t.Fatalf("global check: %v", err)
	}
	if unbalanced != 0 {
		t.Errorf("%d postings with a non-zero signed sum, want 0", unbalanced)
	}
	if total != 0 {
		t.Errorf("signed sum of all entries = %d, want 0", total)
	}

	n := ops.Load()
	t.Logf("ran %d operations", n)
	if !testing.Short() && n < minTotalOps {
		t.Errorf("ran %d operations, want at least %d (AC1)", n, minTotalOps)
	}
}

// --- AC2: no partial postings -----------------------------------------------

// errInjected is the failure faultTx injects.
var errInjected = errors.New("injected failure")

// faultTx wraps a real transaction and counts the QueryRow and Exec calls
// made through it (Post makes two: the isolation check, then the INSERT).
// Call failAt returns errInjected without reaching the server; before call
// cancelAt, cancel is called, so that call and everything after it see a
// cancelled context. 0 disables either.
type faultTx struct {
	pgx.Tx
	failAt, cancelAt int
	cancel           context.CancelFunc
	calls            int
}

func (f *faultTx) next() (fail bool) {
	f.calls++
	if f.calls == f.cancelAt {
		f.cancel()
	}
	return f.calls == f.failAt
}

func (f *faultTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if f.next() {
		return errRow{errInjected}
	}
	return f.Tx.QueryRow(ctx, sql, args...)
}

func (f *faultTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if f.next() {
		return pgconn.CommandTag{}, errInjected
	}
	return f.Tx.Exec(ctx, sql, args...)
}

type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

// The fault points AC2 covers.
type fault int

const (
	faultIsolationQuery fault = iota // the isolation-level QueryRow fails
	faultInsertSend                  // the INSERT Exec fails before it is sent
	faultInsertRow                   // the INSERT fails on row k, after earlier rows went in
	faultAfterPost                   // fn fails after a good Post
	faultKillBackend                 // the backend dies between Post and COMMIT
	faultCancel                      // the context is cancelled at a drawn call
)

func (f fault) String() string {
	return [...]string{"isolation query", "insert send", "insert row", "after post", "kill backend", "cancel"}[f]
}

// failRowTrigger makes the k-th row inserted into ledger_entries in a
// transaction raise, where k is the transaction-local setting test.fail_row
// (unset: the trigger does nothing). It exists only in AC2's own database.
const failRowTrigger = `
CREATE FUNCTION test_fail_row() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
	k    int := nullif(current_setting('test.fail_row', true), '')::int;
	seen int;
BEGIN
	IF k IS NULL THEN
		RETURN NEW;
	END IF;
	seen := coalesce(nullif(current_setting('test.seen_rows', true), ''), '0')::int + 1;
	PERFORM set_config('test.seen_rows', seen::text, true);
	IF seen = k THEN
		RAISE EXCEPTION 'injected failure at row %', k;
	END IF;
	RETURN NEW;
END $$;
CREATE TRIGGER test_fail_row BEFORE INSERT ON ledger_entries
	FOR EACH ROW EXECUTE FUNCTION test_fail_row();`

// balancedLegs draws a balanced posting of 2-6 legs over accounts, with at
// least one debit and one credit and random positive amounts.
func balancedLegs(t *rapid.T, accounts []uuid.UUID) []Entry {
	n := rapid.IntRange(2, 6).Draw(t, "legs")
	debits := rapid.IntRange(1, n-1).Draw(t, "debits")
	account := rapid.SampledFrom(accounts)
	amount := rapid.Int64Range(1, 1_000_000)

	legs := make([]Entry, n)
	var debitSum, creditSum int64
	for i := range n - 1 {
		legs[i] = Entry{account.Draw(t, "account"), Credit, amount.Draw(t, "amount")}
		if i < debits {
			legs[i].Direction = Debit
			debitSum += legs[i].Amount
		} else {
			creditSum += legs[i].Amount
		}
	}
	// The last leg balances the posting. Index n-1 >= debits, so it is a
	// credit; if the credits so far already reach the debits, top up the
	// first debit so the last credit is still positive.
	last := debitSum - creditSum
	if last <= 0 {
		legs[0].Amount += 1 - last
		last = 1
	}
	legs[n-1] = Entry{account.Draw(t, "account"), Credit, last}
	return legs
}

func TestPropertyNoPartialPostings(t *testing.T) {
	t.Parallel()
	// Its own database, because of the trigger.
	pool := testutil.Postgres(t)
	ctx := t.Context()
	if _, err := pool.Exec(ctx, failRowTrigger); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	// Each fault kind is its own subtest with its own rapid.Check, so every
	// run (including -short) exercises all of them; drawing the fault would
	// leave a kind untested on some runs. The subtests share the database:
	// each check makes its own accounts and payment, and the trigger's
	// settings are transaction-local.
	for _, f := range []fault{
		faultIsolationQuery, faultInsertSend, faultInsertRow, faultAfterPost, faultKillBackend, faultCancel,
	} {
		t.Run(f.String(), func(t *testing.T) {
			t.Parallel()
			rapid.Check(t, func(rt *rapid.T) {
				accounts := []uuid.UUID{
					newAccount(ctx, t, pool, "user"),
					newAccount(ctx, t, pool, "merchant"),
					newAccount(ctx, t, pool, "clearing"),
					newAccount(ctx, t, pool, "user"),
				}
				legs := balancedLegs(rt, accounts)
				payment := newPayment(ctx, t, pool, accounts[0], accounts[1], 1)
				before := balances(ctx, t, pool, accounts)

				cctx, cancel := context.WithCancel(ctx)
				defer cancel()
				var fn func(tx pgx.Tx) error
				var wantErr error
				switch f {
				case faultIsolationQuery, faultInsertSend:
					failAt := 1
					if f == faultInsertSend {
						failAt = 2
					}
					wantErr = errInjected
					fn = func(tx pgx.Tx) error {
						return Post(cctx, &faultTx{Tx: tx, failAt: failAt}, payment, legs)
					}
				case faultInsertRow:
					k := rapid.IntRange(1, len(legs)).Draw(rt, "failRow")
					fn = func(tx pgx.Tx) error {
						if _, err := tx.Exec(cctx, "SELECT set_config('test.fail_row', $1, true)", strconv.Itoa(k)); err != nil {
							return err
						}
						err := Post(cctx, tx, payment, legs)
						if err == nil || !strings.Contains(err.Error(), "injected failure at row") {
							rt.Errorf("Post = %v, want the trigger's failure at row %d", err, k)
						}
						return err
					}
				case faultAfterPost:
					wantErr = errInjected
					fn = func(tx pgx.Tx) error {
						if err := Post(cctx, tx, payment, legs); err != nil {
							return err
						}
						return errInjected
					}
				case faultKillBackend:
					fn = func(tx pgx.Tx) error {
						if err := Post(cctx, tx, payment, legs); err != nil {
							return err
						}
						// Terminate this transaction's backend from another
						// connection and wait (up to 5 s) for it to exit, so the
						// COMMIT that follows finds the connection gone.
						pid := tx.Conn().PgConn().PID()
						var ok bool
						if err := pool.QueryRow(ctx, "SELECT pg_terminate_backend($1, 5000)", pid).Scan(&ok); err != nil || !ok {
							rt.Fatalf("pg_terminate_backend(%d) = %v, %v", pid, ok, err)
						}
						return nil
					}
				case faultCancel:
					// Calls 1 and 2 are Post's; 3 means after Post returns, before
					// COMMIT.
					at := rapid.IntRange(1, 3).Draw(rt, "cancelAt")
					wantErr = context.Canceled
					fn = func(tx pgx.Tx) error {
						if err := Post(cctx, &faultTx{Tx: tx, cancelAt: at, cancel: cancel}, payment, legs); err != nil {
							return err
						}
						if at == 3 {
							cancel()
						}
						return nil
					}
				}

				err := store.Serializable(cctx, pool, fn)
				if err == nil {
					rt.Fatalf("fault %v: Serializable = nil, want an error", f)
				}
				if wantErr != nil && !errors.Is(err, wantErr) {
					rt.Errorf("fault %v: err = %v, want %v", f, err, wantErr)
				}
				if n := countEntries(ctx, t, pool, payment); n != 0 {
					rt.Errorf("fault %v: %d entries after the failed posting, want 0", f, n)
				}
				after := balances(ctx, t, pool, accounts)
				for i := range accounts {
					if after[i] != before[i] {
						rt.Errorf("fault %v: balance of account %d = %d, want %d (unchanged)", f, i, after[i], before[i])
					}
				}

				// Control: the same posting without the fault goes in whole, so the
				// zero above is down to the fault, not to a posting that could never
				// succeed.
				err = store.Serializable(ctx, pool, func(tx pgx.Tx) error {
					return Post(ctx, tx, payment, legs)
				})
				if err != nil {
					rt.Fatalf("control post: %v", err)
				}
				if n := countEntries(ctx, t, pool, payment); n != len(legs) {
					rt.Errorf("control post: %d entries, want %d", n, len(legs))
				}
			})
		})
	}
}

func balances(ctx context.Context, t *testing.T, pool *pgxpool.Pool, accounts []uuid.UUID) []int64 {
	t.Helper()
	out := make([]int64, len(accounts))
	for i, id := range accounts {
		b, err := Balance(ctx, pool, id)
		if err != nil {
			t.Fatalf("Balance: %v", err)
		}
		out[i] = b
	}
	return out
}
