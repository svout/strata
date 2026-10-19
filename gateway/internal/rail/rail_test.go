package rail_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/svout/strata/gateway/internal/ledger"
	"github.com/svout/strata/gateway/internal/rail"
	"github.com/svout/strata/gateway/internal/rail/railtest"
	"github.com/svout/strata/gateway/internal/testutil"
)

// AC6: the internal-ledger rail passes the contract. One database serves the
// whole suite; each request has its own accounts and payment. It never
// declines, is never pending, and a lost response can't be arranged
// in-process, so those tests skip. Unreachable is the rail on a closed pool:
// the database (its "PSP") can't be reached at all.
func TestInternal_Contract(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	railtest.Run(t, railtest.Harness{
		New:     func(*testing.T) rail.Adapter { return rail.NewInternal(pool) },
		Request: func(t *testing.T) rail.Request { return newPaymentRequest(t, pool) },
		Effects: func(t *testing.T, ref string) int {
			// Count postings by payment, not by reference, so a double post
			// that bypassed the reference would still be seen.
			id, err := uuid.Parse(strings.TrimPrefix(ref, "pay_"))
			if err != nil {
				t.Fatalf("ref %q: %v", ref, err)
			}
			var legs int
			if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM ledger_entries WHERE payment_id = $1", pgtype.UUID{Bytes: id, Valid: true}).Scan(&legs); err != nil {
				t.Fatal(err)
			}
			return legs / 2 // every internal posting has two legs
		},
		Unreachable: func(t *testing.T, _ rail.Adapter) (rail.Adapter, rail.Request) {
			closed, err := pgxpool.NewWithConfig(t.Context(), pool.Config())
			if err != nil {
				t.Fatal(err)
			}
			closed.Close()
			return rail.NewInternal(closed), newPaymentRequest(t, pool)
		},
	})
}

// The internal rail moves the amount from payer to payee.
func TestInternal_Balances(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	req := newPaymentRequest(t, pool)
	r := rail.NewInternal(pool)
	for range 2 { // the repeat moves nothing
		if _, err := r.Settle(t.Context(), req); err != nil {
			t.Fatal(err)
		}
	}
	for acct, want := range map[uuid.UUID]int64{req.Payer: -req.Amount, req.Payee: req.Amount} {
		if got, err := ledger.Balance(t.Context(), pool, acct); err != nil || got != want {
			t.Errorf("balance = %d (%v), want %d", got, err, want)
		}
	}
}

// Review should 4: settles of *different* payments must not conflict. With
// ledger.Posted read inside the SERIALIZABLE transaction its predicate locks
// made them conflict (0.78-0.89 rolled-back transactions per settle); read
// before the transaction it was 0.00. Settle rounds of 16 different payments
// at once on a fresh (small) ledger, require every settle to succeed, and
// fail if more than 1 in 10 settles was rolled back (each rollback is one
// retry) — far above the 0 measured, far below the ~0.8 regression.
func TestInternal_ConcurrentDifferentPayments(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	const rounds, n = 10, 16
	reqs := make([][]rail.Request, rounds)
	for i := range reqs {
		for range n {
			reqs[i] = append(reqs[i], newPaymentRequest(t, pool))
		}
	}
	var db string
	if err := pool.QueryRow(t.Context(), "SELECT current_database()").Scan(&db); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond) // let the setup's statistics land
	before := xactCounts(t, pool, db)

	r := rail.NewInternal(pool)
	var failed []error
	for _, round := range reqs {
		errs := make([]error, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i, req := range round {
			wg.Go(func() {
				<-start
				_, errs[i] = r.Settle(t.Context(), req)
			})
		}
		close(start)
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				failed = append(failed, err)
			}
		}
	}

	// Closing the pool flushes its backends' statistics; read them on a
	// fresh pool, by database name.
	cfg := pool.Config()
	pool.Close()
	stats, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stats.Close()
	after := xactCounts(t, stats, db)
	settles := rounds * n
	rollbacks := after[1] - before[1]
	t.Logf("%d settles of different payments, %d at a time: %d failed, %d rolled-back transactions (%.2f per settle), %d committed",
		settles, n, len(failed), rollbacks, float64(rollbacks)/float64(settles), after[0]-before[0])
	if len(failed) > 0 {
		t.Fatalf("%d of %d settles failed, first: %v", len(failed), settles, failed[0])
	}
	if rollbacks*10 > int64(settles) {
		t.Fatalf("%d rolled-back transactions for %d settles of different payments, want at most %d", rollbacks, settles, settles/10)
	}
}

// AC6: the fake PSP passes the contract, including declines, pending and a
// lost response. It has no transport to cut, so the unreachable test skips;
// a Step's Err is its failure before acting (TestFake_ErrorBeforeApplying).
func TestFake_Contract(t *testing.T) {
	t.Parallel()
	var current *rail.Fake // subtests run one at a time
	fake := func(a rail.Adapter) *rail.Fake { return a.(*rail.Fake) }
	railtest.Run(t, railtest.Harness{
		New: func(*testing.T) rail.Adapter {
			current = rail.NewFake()
			return current
		},
		Request: func(*testing.T) rail.Request { return fakeRequest() },
		Effects: func(_ *testing.T, ref string) int { return current.Moves(ref) },
		Decline: func(_ *testing.T, a rail.Adapter) rail.Request {
			fake(a).Push(rail.Step{Outcome: rail.Declined, Reason: "insufficient_funds"})
			return fakeRequest()
		},
		Pend: func(_ *testing.T, a rail.Adapter) rail.Request {
			fake(a).Push(rail.Step{Outcome: rail.Pending})
			return fakeRequest()
		},
		Resolve: func(t *testing.T, a rail.Adapter, ref string, outcome rail.Outcome) {
			if err := fake(a).Resolve(ref, outcome, ""); err != nil {
				t.Fatal(err)
			}
		},
		LoseResponse: func(_ *testing.T, a rail.Adapter) rail.Request {
			fake(a).Push(rail.Step{LoseResponse: true})
			return fakeRequest()
		},
	})
}

// A delay shorter than the deadline settles; a longer one returns the
// context's error and applies nothing, so a retry with the same reference
// settles once.
func TestFake_DelayAndTimeout(t *testing.T) {
	t.Parallel()
	f := rail.NewFake(rail.Step{Delay: 5 * time.Millisecond}, rail.Step{Delay: time.Second})
	ctx := func() context.Context {
		c, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		t.Cleanup(cancel)
		return c
	}
	if res, err := f.Settle(ctx(), fakeRequest()); err != nil || res.Outcome != rail.Settled {
		t.Fatalf("short delay = %+v, %v; want Settled", res, err)
	}
	slow := fakeRequest()
	if _, err := f.Settle(ctx(), slow); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("long delay = %v, want DeadlineExceeded", err)
	}
	if _, err := f.Status(t.Context(), slow.Ref); !errors.Is(err, rail.ErrUnknownRef) {
		t.Fatalf("Status after timeout = %v, want ErrUnknownRef (nothing applied)", err)
	}
	if res, err := f.Settle(t.Context(), slow); err != nil || res.Outcome != rail.Settled || f.Moves(slow.Ref) != 1 {
		t.Fatalf("retry = %+v, %v; want Settled once", res, err)
	}
}

// A failure before the PSP acted applies nothing.
func TestFake_ErrorBeforeApplying(t *testing.T) {
	t.Parallel()
	boom := errors.New("connection reset")
	f := rail.NewFake(rail.Step{Err: boom})
	req := fakeRequest()
	if _, err := f.Settle(t.Context(), req); !errors.Is(err, boom) {
		t.Fatalf("Settle = %v, want the scripted error", err)
	}
	if _, err := f.Status(t.Context(), req.Ref); !errors.Is(err, rail.ErrUnknownRef) {
		t.Fatalf("Status = %v, want ErrUnknownRef", err)
	}
}

// Resolve only finishes a pending operation, with a final outcome, and a
// decline always carries a reason.
func TestFake_Resolve(t *testing.T) {
	t.Parallel()
	f := rail.NewFake(rail.Step{Outcome: rail.Pending}, rail.Step{Outcome: rail.Declined})
	pending, declined := fakeRequest(), fakeRequest()
	for _, req := range []rail.Request{pending, declined} {
		if _, err := f.Settle(t.Context(), req); err != nil {
			t.Fatal(err)
		}
	}
	if res, _ := f.Status(t.Context(), declined.Ref); res.Reason != rail.DefaultDeclineReason {
		t.Fatalf("decline without a scripted reason = %+v, want reason %q", res, rail.DefaultDeclineReason)
	}
	if err := f.Resolve(pending.Ref, rail.Pending, ""); !errors.Is(err, rail.ErrInvalidRequest) {
		t.Fatalf("Resolve to Pending = %v, want ErrInvalidRequest", err)
	}
	if err := f.Resolve(declined.Ref, rail.Settled, ""); !errors.Is(err, rail.ErrNotPending) {
		t.Fatalf("Resolve of a final outcome = %v, want ErrNotPending", err)
	}
	if err := f.Resolve("pay_unknown", rail.Settled, ""); !errors.Is(err, rail.ErrUnknownRef) {
		t.Fatalf("Resolve of an unknown ref = %v, want ErrUnknownRef", err)
	}
}

// The script plays out exactly: one step per reference that reaches the
// fake, in order, and a repeat returns the recorded outcome without a step.
func TestFake_ScriptPlaysInOrder(t *testing.T) {
	t.Parallel()
	reset := errors.New("reset")
	f := rail.NewFake(
		rail.Step{Outcome: rail.Declined, Reason: "insufficient_funds"},
		rail.Step{LoseResponse: true},
		rail.Step{Outcome: rail.Pending},
		rail.Step{Err: reset},
	)
	reqs := make([]rail.Request, 6)
	for i := range reqs {
		reqs[i] = fakeRequest()
	}
	declined := rail.Result{Outcome: rail.Declined, Reason: "insufficient_funds"}
	settled := rail.Result{Outcome: rail.Settled}
	for i, want := range []struct {
		req rail.Request
		res rail.Result
		err error
	}{
		{reqs[0], declined, nil},
		{reqs[1], rail.Result{}, rail.ErrResponseLost},
		{reqs[2], rail.Result{Outcome: rail.Pending}, nil},
		{reqs[3], rail.Result{}, reset},
		{reqs[4], settled, nil}, // the script has run out
		{reqs[5], settled, nil},
		{reqs[0], declined, nil}, // repeats: recorded, no step taken
		{reqs[1], settled, nil},
		{reqs[3], settled, nil}, // nothing was recorded, so a new step: default
	} {
		res, err := f.Settle(t.Context(), want.req)
		if res != want.res || !errors.Is(err, want.err) || (want.err == nil && err != nil) {
			t.Errorf("call %d = %+v, %v; want %+v, %v", i, res, err, want.res, want.err)
		}
	}
}

// Concurrent calls for one reference share one step, so the script doesn't
// shift: every caller sees the scripted outcome and the next reference gets
// the next step (review should 1, for the subtask 11 chaos runs).
func TestFake_ConcurrentCallsShareOneStep(t *testing.T) {
	t.Parallel()
	f := rail.NewFake(
		rail.Step{Delay: 20 * time.Millisecond, Outcome: rail.Declined, Reason: "first"},
		rail.Step{Outcome: rail.Declined, Reason: "second"},
	)
	req := fakeRequest()
	const n = 16
	results := make([]rail.Result, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			results[i], errs[i] = f.Settle(t.Context(), req)
		})
	}
	close(start)
	wg.Wait()
	first := rail.Result{Outcome: rail.Declined, Reason: "first"}
	for i := range n {
		if errs[i] != nil || results[i] != first {
			t.Fatalf("call %d = %+v, %v; want %+v", i, results[i], errs[i], first)
		}
	}
	if res, err := f.Settle(t.Context(), fakeRequest()); err != nil || res.Reason != "second" {
		t.Fatalf("next reference = %+v, %v; want the second step", res, err)
	}
}

func TestRef_Deterministic(t *testing.T) {
	id := uuid.New()
	again := uuid.MustParse(id.String())
	if rail.Ref(id) != rail.Ref(again) || rail.Ref(id) == rail.Ref(uuid.New()) || !strings.HasSuffix(rail.Ref(id), id.String()) {
		t.Fatalf("Ref(%s) = %q", id, rail.Ref(id))
	}
}

// fakeRequest is a valid request with a new payment; the fake never looks
// the accounts up.
func fakeRequest() rail.Request {
	id := uuid.New()
	return rail.Request{PaymentID: id, Ref: rail.Ref(id), Amount: 1250, Currency: "GBP", Payer: uuid.New(), Payee: uuid.New()}
}

// newPaymentRequest creates two accounts and a payment between them, and
// returns the request that settles it.
func newPaymentRequest(t *testing.T, pool *pgxpool.Pool) rail.Request {
	t.Helper()
	payer, payee := testutil.NewAccount(t, pool, "user"), testutil.NewAccount(t, pool, "merchant")
	payment := testutil.NewPayment(t, pool, payer, payee, 1250)
	return rail.Request{PaymentID: payment, Ref: rail.Ref(payment), Amount: 1250, Currency: "GBP", Payer: payer, Payee: payee}
}

// xactCounts reads database db's committed and rolled-back transaction
// counts, polling until two reads 200 ms apart agree (statistics arrive
// asynchronously), for at most 5 s.
func xactCounts(t *testing.T, pool *pgxpool.Pool, db string) [2]int64 {
	t.Helper()
	read := func() [2]int64 {
		var c [2]int64
		if err := pool.QueryRow(context.WithoutCancel(t.Context()), "SELECT xact_commit, xact_rollback FROM pg_stat_database WHERE datname = $1", db).Scan(&c[0], &c[1]); err != nil {
			t.Fatal(err)
		}
		return c
	}
	prev := read()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		time.Sleep(200 * time.Millisecond)
		cur := read()
		if cur[1] == prev[1] && cur[0]-prev[0] <= 1 {
			return cur
		}
		prev = cur
	}
	return prev
}
