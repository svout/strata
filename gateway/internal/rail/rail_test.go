package rail_test

import (
	"context"
	"errors"
	"strings"
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
// whole suite; each request has its own accounts and payment.
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

// AC6: the fake PSP passes the contract, including declines.
func TestFake_Contract(t *testing.T) {
	t.Parallel()
	var current *rail.Fake // subtests run one at a time
	railtest.Run(t, railtest.Harness{
		New: func(*testing.T) rail.Adapter {
			current = rail.NewFake()
			return current
		},
		Request: func(*testing.T) rail.Request { return fakeRequest() },
		Effects: func(_ *testing.T, ref string) int {
			if current.Moved(ref) {
				return 1
			}
			return 0
		},
		Decline: func(_ *testing.T, a rail.Adapter) rail.Request {
			a.(*rail.Fake).Push(rail.Step{Outcome: rail.Declined, Reason: "insufficient_funds"})
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
	if res, err := f.Settle(t.Context(), slow); err != nil || res.Outcome != rail.Settled || !f.Moved(slow.Ref) {
		t.Fatalf("retry = %+v, %v; want Settled", res, err)
	}
}

// Unknown is not failure (ADR-120): the response is lost after the money
// moved, Settle errors, and Status says Settled; a retry moves nothing more.
func TestFake_LostResponseIsResolvedByStatus(t *testing.T) {
	t.Parallel()
	f := rail.NewFake(rail.Step{LoseResponse: true})
	req := fakeRequest()
	if _, err := f.Settle(t.Context(), req); !errors.Is(err, rail.ErrResponseLost) {
		t.Fatalf("Settle = %v, want ErrResponseLost", err)
	}
	if res, err := f.Status(t.Context(), req.Ref); err != nil || res.Outcome != rail.Settled {
		t.Fatalf("Status = %+v, %v; want Settled", res, err)
	}
	if res, err := f.Settle(t.Context(), req); err != nil || res.Outcome != rail.Settled {
		t.Fatalf("retry = %+v, %v; want the recorded Settled", res, err)
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

// Pending stays pending (and repeats return it) until Resolve, as an
// asynchronous PSP's webhook would; only a pending operation resolves.
func TestFake_PendingThenResolved(t *testing.T) {
	t.Parallel()
	f := rail.NewFake(rail.Step{Outcome: rail.Pending}, rail.Step{Outcome: rail.Pending})
	settles, declines := fakeRequest(), fakeRequest()
	for _, req := range []rail.Request{settles, declines} {
		if res, err := f.Settle(t.Context(), req); err != nil || res.Outcome != rail.Pending {
			t.Fatalf("Settle = %+v, %v; want Pending", res, err)
		}
	}
	if res, _ := f.Settle(t.Context(), settles); res.Outcome != rail.Pending || f.Moved(settles.Ref) {
		t.Fatalf("repeat while pending = %+v, want Pending and no money moved", res)
	}
	if err := f.Resolve(settles.Ref, rail.Settled, ""); err != nil {
		t.Fatal(err)
	}
	if err := f.Resolve(declines.Ref, rail.Declined, "do_not_honour"); err != nil {
		t.Fatal(err)
	}
	if res, err := f.Status(t.Context(), settles.Ref); err != nil || res.Outcome != rail.Settled || !f.Moved(settles.Ref) {
		t.Fatalf("Status = %+v, %v; want Settled", res, err)
	}
	if res, err := f.Status(t.Context(), declines.Ref); err != nil || res != (rail.Result{Outcome: rail.Declined, Reason: "do_not_honour"}) {
		t.Fatalf("Status = %+v, %v; want Declined do_not_honour", res, err)
	}
	if err := f.Resolve(settles.Ref, rail.Declined, "late"); !errors.Is(err, rail.ErrNotPending) {
		t.Fatalf("Resolve of a final outcome = %v, want ErrNotPending", err)
	}
	if err := f.Resolve("pay_unknown", rail.Settled, ""); !errors.Is(err, rail.ErrUnknownRef) {
		t.Fatalf("Resolve of an unknown ref = %v, want ErrUnknownRef", err)
	}
}

// Same script, same calls, same results.
func TestFake_Deterministic(t *testing.T) {
	t.Parallel()
	script := []rail.Step{
		{Outcome: rail.Declined, Reason: "insufficient_funds"},
		{LoseResponse: true},
		{Outcome: rail.Pending},
		{Err: errors.New("reset")},
	}
	reqs := make([]rail.Request, 6)
	for i := range reqs {
		reqs[i] = fakeRequest()
	}
	run := func() []string {
		f := rail.NewFake(script...)
		var out []string
		for _, req := range append(reqs, reqs[0], reqs[1]) {
			res, err := f.Settle(t.Context(), req)
			out = append(out, string(res.Outcome)+"/"+res.Reason+"/"+errText(err))
		}
		return out
	}
	if a, b := run(), run(); strings.Join(a, " ") != strings.Join(b, " ") {
		t.Fatalf("two runs differ:\n%v\n%v", a, b)
	}
}

func TestRef_Deterministic(t *testing.T) {
	id := uuid.New()
	again := uuid.MustParse(id.String())
	if rail.Ref(id) != rail.Ref(again) || rail.Ref(id) == rail.Ref(uuid.New()) || !strings.HasSuffix(rail.Ref(id), id.String()) {
		t.Fatalf("Ref(%s) = %q", id, rail.Ref(id))
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// fakeRequest is a valid request with a new payment; the fake never looks
// the accounts up.
func fakeRequest() rail.Request {
	id := uuid.New()
	return rail.Request{PaymentID: id, Ref: rail.Ref(id), Amount: 1250, Currency: rail.Currency, Payer: uuid.New(), Payee: uuid.New()}
}

// newPaymentRequest creates two accounts and a payment between them, and
// returns the request that settles it.
func newPaymentRequest(t *testing.T, pool *pgxpool.Pool) rail.Request {
	t.Helper()
	ctx := t.Context()
	newID := func(sql string, args ...any) uuid.UUID {
		var id pgtype.UUID
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("insert: %v", err)
		}
		return uuid.UUID(id.Bytes)
	}
	const account = "INSERT INTO accounts (kind, currency) VALUES ('user', 'GBP') RETURNING id"
	payer, payee := newID(account), newID(account)
	pg := func(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }
	payment := newID(`INSERT INTO payments (payer_account_id, payee_account_id, amount, currency, rail)
		VALUES ($1, $2, 1250, 'GBP', 'internal') RETURNING id`, pg(payer), pg(payee))
	return rail.Request{PaymentID: payment, Ref: rail.Ref(payment), Amount: 1250, Currency: rail.Currency, Payer: payer, Payee: payee}
}
