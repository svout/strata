package payments

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"pgregory.net/rapid"

	"github.com/svout/strata/gateway/internal/httpx"
	"github.com/svout/strata/gateway/internal/idem"
	"github.com/svout/strata/gateway/internal/ledger"
	"github.com/svout/strata/gateway/internal/rail"
	"github.com/svout/strata/gateway/internal/testutil"
)

// SPEC-002 subtask 10a-i: the settlement outbox and recovery (AC4). The
// process-kill version of these tests is subtask 11 (chaos).

// The internal rail settles a new payment before CreateIdempotent returns;
// the stored 201 still says settling, and a replay returns it byte for byte
// without calling the rail again.
func TestSettle_InternalRail(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	r := &hookRail{Adapter: internalRail(t, pool)}
	svc, _ := newSettleService(t, pool, r)
	payer, payee := newAccount(t.Context(), t, pool), newAccount(t.Context(), t, pool)

	first := createFor(t, svc, "k", payer, payee)
	var body Payment
	if err := json.Unmarshal(first.Body, &body); err != nil || body.State != Settling || body.RailRef == nil || *body.RailRef != rail.Ref(first.PaymentID) {
		t.Fatalf("201 body = %+v (%v), want settling with rail_ref %s", body, err, rail.Ref(first.PaymentID))
	}
	assertState(t, svc, first.PaymentID, Settled, nil)

	again := createFor(t, svc, "k", payer, payee)
	if !again.Replayed || !bytes.Equal(again.Body, first.Body) {
		t.Fatalf("replay = %+v, want the stored body", again)
	}
	if n := r.settles.Load(); n != 1 {
		t.Fatalf("rail Settle called %d times, want 1 (a replay never settles)", n)
	}
	// ADR-123: the payer's posting (payer -> clearing) and the rail's
	// (clearing -> payee), so the clearing account nets to zero.
	if n := ledgerEntries(t, pool, first.PaymentID); n != 4 {
		t.Fatalf("ledger entries = %d, want 4", n)
	}
	assertBalances(t, pool, map[uuid.UUID]int64{payer: -1250, payee: 1250, uuid.UUID(clearingFor(t, pool, rail.InternalName)): 0})
}

// AC4 on the fake PSP: each way a settle can end, and what recovery then
// does. Effects are counted with Fake.Moves, so a double settle shows as 2.
func TestSettle_FakeOutcomesAndRecovery(t *testing.T) {
	t.Parallel()
	declined := FailureDeclined
	for name, tc := range map[string]struct {
		step         rail.Step
		afterCreate  State
		resolve      rail.Outcome // finish a pending operation before recovery
		afterRecover State
		reason       *FailureReason
		moves        int
	}{
		"settled":  {step: rail.Step{}, afterCreate: Settled, afterRecover: Settled, moves: 1},
		"declined": {step: rail.Step{Outcome: rail.Declined}, afterCreate: Failed, afterRecover: Failed, reason: &declined},
		// Failure before the PSP acted: Status doesn't know the reference,
		// so recovery settles it with the same one.
		"error before the rail acted": {step: rail.Step{Err: errors.New("connection reset")}, afterCreate: Settling, afterRecover: Settled, moves: 1},
		// The rail acted but the answer was lost: Status says Settled, and
		// recovery records it without settling again.
		"lost response": {step: rail.Step{LoseResponse: true}, afterCreate: Settling, afterRecover: Settled, moves: 1},
		// Slower than settleTimeout: nothing applied, recovered as above.
		"timeout":               {step: rail.Step{Delay: time.Second}, afterCreate: Settling, afterRecover: Settled, moves: 1},
		"pending then settled":  {step: rail.Step{Outcome: rail.Pending}, afterCreate: Settling, resolve: rail.Settled, afterRecover: Settled, moves: 1},
		"pending then declined": {step: rail.Step{Outcome: rail.Pending}, afterCreate: Settling, resolve: rail.Declined, afterRecover: Failed, reason: &declined},
		"still pending":         {step: rail.Step{Outcome: rail.Pending}, afterCreate: Settling, afterRecover: Settling},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pool := testutil.Postgres(t)
			fake := rail.NewFake(tc.step)
			svc, _ := newSettleService(t, pool, fake)
			svc.settleTimeout = 100 * time.Millisecond
			res := createFor(t, svc, "k", newAccount(t.Context(), t, pool), newAccount(t.Context(), t, pool))
			var reason *FailureReason
			if tc.afterCreate == Failed {
				reason = tc.reason
			}
			assertState(t, svc, res.PaymentID, tc.afterCreate, reason)

			if tc.resolve != "" {
				if err := fake.Resolve(rail.Ref(res.PaymentID), tc.resolve, ""); err != nil {
					t.Fatal(err)
				}
			}
			// A new Service, as after a restart; the fake is the PSP and
			// outlives it.
			restarted, _ := newSettleService(t, pool, fake)
			if _, err := restarted.Recover(t.Context()); err != nil {
				t.Fatalf("Recover: %v", err)
			}
			assertState(t, svc, res.PaymentID, tc.afterRecover, tc.reason)
			if n := fake.Moves(rail.Ref(res.PaymentID)); n != tc.moves {
				t.Fatalf("money moved %d times, want %d", n, tc.moves)
			}
			// ADR-123 on an external rail: the gateway's ledger holds only
			// the payer's side, into the rail's clearing account, once per
			// settled payment and never for a failed or pending one.
			wantLegs, wantClearing := 0, int64(0)
			if tc.afterRecover == Settled {
				wantLegs, wantClearing = 2, 1250
			}
			if n := payerEntries(t, pool, res.PaymentID); n != wantLegs {
				t.Fatalf("payer posting legs = %d, want %d", n, wantLegs)
			}
			assertBalances(t, pool, map[uuid.UUID]int64{uuid.UUID(clearingFor(t, pool, rail.FakeName)): wantClearing})
		})
	}
}

// Decision 1 (Ivan, 2026-10-19): a request the rail rejects as invalid fails
// the payment with its own reason, is logged at Error with the payment id as
// text (AC8), and is never sent again, by recovery or anyone else.
func TestSettle_InvalidRequestFailsOnceAndIsNotRetried(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	r := &hookRail{Adapter: rail.NewFake(rail.Step{Err: rail.ErrInvalidRequest})}
	svc, logs := newSettleService(t, pool, r)
	res := createFor(t, svc, "k", newAccount(t.Context(), t, pool), newAccount(t.Context(), t, pool))
	invalid := FailureInvalidRequest
	assertState(t, svc, res.PaymentID, Failed, &invalid)

	st, err := svc.Recover(t.Context())
	if err != nil || st != (RecoverStats{}) {
		t.Fatalf("Recover = %+v, %v; want nothing to do", st, err)
	}
	if n := r.settles.Load(); n != 1 {
		t.Fatalf("rail Settle called %d times, want 1", n)
	}
	var line map[string]any
	for l := range strings.Lines(logs.String()) {
		if strings.Contains(l, "rejected the settle request") {
			if err := json.Unmarshal([]byte(l), &line); err != nil {
				t.Fatal(err)
			}
		}
	}
	if line["level"] != "ERROR" || line["payment_id"] != res.PaymentID.String() {
		t.Fatalf("invalid-request log line = %v, want level ERROR and payment_id %s\nlogs:\n%s", line, res.PaymentID, logs)
	}
}

// A client that goes away while the rail is working can't strand the
// payment: the rail call and the write after it don't use the request's
// context.
func TestSettle_ClientCancelDoesNotStrandPayment(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	ctx, cancel := context.WithCancel(t.Context())
	r := &hookRail{Adapter: internalRail(t, pool), onSettle: func(rail.Request) { cancel() }}
	svc, _ := newSettleService(t, pool, r)
	payer, payee := newAccount(t.Context(), t, pool), newAccount(t.Context(), t, pool)
	key, err := idem.ParseKey(idem.AnonymousCaller, "k")
	if err != nil {
		t.Fatal(err)
	}
	res, err := svc.CreateIdempotent(ctx, key, sha256.Sum256([]byte("k")), CreateInput{Amount: 1250, Currency: Currency, PayerAccountID: payer, PayeeAccountID: payee})
	if err != nil {
		t.Fatalf("CreateIdempotent: %v", err)
	}
	if ctx.Err() == nil {
		t.Fatal("the hook didn't cancel the request context")
	}
	assertState(t, svc, res.PaymentID, Settled, nil)
	if n := ledgerEntries(t, pool, res.PaymentID); n != 4 {
		t.Fatalf("ledger entries = %d, want 4", n)
	}
}

// Concurrent recovery passes (two gateway replicas starting at once) settle
// every stuck payment exactly once. The pages are small so the keyset walk
// crosses several pages while other passes change the backlog under it.
func TestRecover_ConcurrentPassesSettleOnce(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	// Every Settle fails before the rail is reached, as if the process died
	// after the outbox commit: all payments are left settling.
	down, _ := newSettleService(t, pool, &hookRail{Adapter: internalRail(t, pool), fail: errors.New("connection refused")})
	payer, payee := newAccount(t.Context(), t, pool), newAccount(t.Context(), t, pool)
	const n, passes = 10, 4
	ids := make([]uuid.UUID, n)
	for i := range ids {
		ids[i] = createFor(t, down, "k"+string(rune('a'+i)), payer, payee).PaymentID
		assertState(t, down, ids[i], Settling, nil)
	}

	var wg sync.WaitGroup
	errs := make([]error, passes)
	start := make(chan struct{})
	for i := range passes {
		svc, _ := newSettleService(t, pool, internalRail(t, pool))
		svc.recoverPage = 3
		wg.Go(func() {
			<-start
			_, errs[i] = svc.Recover(t.Context())
		})
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	for _, id := range ids {
		assertState(t, down, id, Settled, nil)
		// One payer posting and one rail posting, though four passes
		// raced to record it.
		if got := ledgerEntries(t, pool, id); got != 4 {
			t.Fatalf("payment %s: ledger entries = %d, want 4 (two postings)", id, got)
		}
	}
}

// Recovery only touches payments on its own rail, and needs a rail.
func TestRecover_OwnRailOnly(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	fakeSvc, _ := newSettleService(t, pool, rail.NewFake(rail.Step{Outcome: rail.Pending}))
	res := createFor(t, fakeSvc, "k", newAccount(t.Context(), t, pool), newAccount(t.Context(), t, pool))

	internal, _ := newSettleService(t, pool, internalRail(t, pool))
	if st, err := internal.Recover(t.Context()); err != nil || st != (RecoverStats{}) {
		t.Fatalf("internal Recover = %+v, %v; want nothing to do", st, err)
	}
	assertState(t, fakeSvc, res.PaymentID, Settling, nil)
	if _, err := New(pool).Recover(t.Context()); !errors.Is(err, ErrNoRail) {
		t.Fatalf("Recover without a rail = %v, want ErrNoRail", err)
	}
}

// MarkFailed rejects a reason that isn't one of the two before the
// database sees it (review should 3): no raw CHECK violation, and the
// payment is unchanged.
func TestMarkFailed_RejectsUnknownReason(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	svc := New(pool)
	p, err := svc.create(t.Context(), CreateInput{Amount: 1250, Currency: Currency,
		PayerAccountID: newAccount(t.Context(), t, pool), PayeeAccountID: newAccount(t.Context(), t, pool)})
	if err != nil {
		t.Fatal(err)
	}
	for _, reason := range []FailureReason{"", "timeout", "Declined"} {
		if _, err := svc.MarkFailed(t.Context(), p.ID, reason); !errors.Is(err, ErrInvalidFailureReason) {
			t.Errorf("MarkFailed(%q) = %v, want ErrInvalidFailureReason", reason, err)
		}
	}
	assertState(t, svc, p.ID, Created, nil)
}

// When another writer finishes the payment while the rail is answering, the
// first outcome recorded wins, and a contradicting answer is logged at Error
// rather than overwriting it: settled against failed, and declined against
// invalid_request, which must stay distinguishable (review nit).
func TestSettle_FirstWriterWins(t *testing.T) {
	t.Parallel()
	declined := FailureDeclined
	for name, tc := range map[string]struct {
		r *hookRail
	}{
		"rail says settled":         {&hookRail{answer: &rail.Result{Outcome: rail.Settled}}},
		"rail says invalid request": {&hookRail{fail: rail.ErrInvalidRequest}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pool := testutil.Postgres(t)
			tc.r.Adapter = rail.NewFake()
			svc, logs := newSettleService(t, pool, tc.r)
			// The other writer: the payment is failed (declined) while the
			// rail call is in progress.
			tc.r.onSettle = func(req rail.Request) {
				if _, err := svc.MarkFailed(t.Context(), req.PaymentID, FailureDeclined); err != nil {
					t.Errorf("MarkFailed: %v", err)
				}
			}
			res := createFor(t, svc, "k", newAccount(t.Context(), t, pool), newAccount(t.Context(), t, pool))
			assertState(t, svc, res.PaymentID, Failed, &declined)
			// The losing settle's transaction rolled back whole: no payer
			// posting for a failed payment.
			if n := payerEntries(t, pool, res.PaymentID); n != 0 {
				t.Fatalf("payer posting legs = %d, want 0", n)
			}
			if !strings.Contains(logs.String(), `"level":"ERROR","msg":"payment already finished differently`) {
				t.Fatalf("no Error line for the contradicting answer:\n%s", logs)
			}
		})
	}
}

// Transition first, then the payer posting (Ivan, 2026-10-19): a second
// settle of a payment already settled loses on the state machine
// (ErrInvalidTransition, the same outcome, so done), never on the ledger's
// unique constraint. Posting first would end this as ErrAlreadyPosted: a Warn
// and outcomeUnknown for a payment that is settled correctly.
func TestFinish_SecondSettleLosesOnTransitionNotLedger(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	svc, logs := newSettleService(t, pool, rail.NewFake())
	res := createFor(t, svc, "k", newAccount(t.Context(), t, pool), newAccount(t.Context(), t, pool))
	assertState(t, svc, res.PaymentID, Settled, nil)
	p, err := svc.Get(t.Context(), res.PaymentID)
	if err != nil {
		t.Fatal(err)
	}

	if got := svc.finish(t.Context(), p, Settled, ""); got != outcomeSettled {
		t.Fatalf("second finish = %v, want outcomeSettled", got)
	}
	if strings.Contains(logs.String(), `"level":"WARN"`) {
		t.Fatalf("second finish logged a Warn:\n%s", logs)
	}
	if n := payerEntries(t, pool, res.PaymentID); n != 2 {
		t.Fatalf("payer posting legs = %d, want 2", n)
	}
}

// AC1, atomicity of markSettled, first direction: the posting fails after
// the transition succeeded. The transition must roll back with it: still
// settling, no payer legs, one Warn. Catches a transition run on the pool
// instead of the transaction (settled with no posting).
//
// Two ways to fail, because they fail differently. A clearing id that names
// no account hits the foreign key on the server, which aborts the
// transaction, so its commit would roll back even if markSettled dropped
// PostRef's error. uuid.Nil is refused by ledger's validation before any SQL
// runs, leaving the transaction healthy, so only returning PostRef's error
// stops the commit; that case catches a swallowed error.
func TestMarkSettled_PostingFailsAfterTransition(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		clearing uuid.UUID
	}{
		{"server-side: foreign key", uuid.New()},
		{"client-side: validation", uuid.Nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pool := testutil.Postgres(t)
			svc, logs := newSettleService(t, pool, rail.NewFake())
			svc.clearing = ledger.ClearingAccountID(tc.clearing)
			res := createFor(t, svc, "k", newAccount(t.Context(), t, pool), newAccount(t.Context(), t, pool))

			assertState(t, svc, res.PaymentID, Settling, nil)
			if n := payerEntries(t, pool, res.PaymentID); n != 0 {
				t.Fatalf("payer posting legs = %d, want 0", n)
			}
			if n := strings.Count(logs.String(), `"level":"WARN","msg":"could not record the rail's answer`); n != 1 {
				t.Fatalf("Warn lines = %d, want 1:\n%s", n, logs)
			}
		})
	}
}

// AC1, atomicity of markSettled, second direction: the posting succeeded and
// then the transition doesn't commit. A deferred constraint trigger (this
// test's database only) refuses the commit of any payment reaching settled,
// after both statements have run. Neither may persist: still settling, no
// payer legs. Catches a posting run on the pool instead of the transaction
// (payer legs for a payment that never settled). TestSettle_FirstWriterWins
// can't: there the transition fails first, so the posting never runs.
func TestMarkSettled_CommitFailsAfterPosting(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	if _, err := pool.Exec(t.Context(), `
		CREATE FUNCTION test_refuse_settled() RETURNS trigger LANGUAGE plpgsql
			AS $$ BEGIN RAISE EXCEPTION 'test: commit of settled refused'; END $$;
		CREATE CONSTRAINT TRIGGER test_refuse_settled AFTER UPDATE ON payments
			DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.state = 'settled')
			EXECUTE FUNCTION test_refuse_settled();`); err != nil {
		t.Fatal(err)
	}
	svc, logs := newSettleService(t, pool, rail.NewFake())
	res := createFor(t, svc, "k", newAccount(t.Context(), t, pool), newAccount(t.Context(), t, pool))

	assertState(t, svc, res.PaymentID, Settling, nil)
	if n := payerEntries(t, pool, res.PaymentID); n != 0 {
		t.Fatalf("payer posting legs = %d, want 0", n)
	}
	assertBalances(t, pool, map[uuid.UUID]int64{uuid.UUID(clearingFor(t, pool, rail.FakeName)): 0})
	if n := strings.Count(logs.String(), `"level":"WARN","msg":"could not record the rail's answer`); n != 1 {
		t.Fatalf("Warn lines = %d, want 1:\n%s", n, logs)
	}
}

// The central invariant of 10a-ii-a2, as a property: whatever mix of rail
// outcomes, recoveries, repeated answers, other writers and failed postings
// happens, a payment is settled if and only if its payer posting exists
// (exactly two legs), and the fake rail's clearing account holds exactly the
// settled amount. The fixed tests above prove atomicity today; this keeps
// proving it as later phases add code that touches settlement.
func TestProperty_SettledIffPayerPosting(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	fake := rail.NewFake()
	svc, _ := newSettleService(t, pool, fake)
	// broken posts to a clearing account that doesn't exist: every
	// settlement it records fails at the foreign key and must roll back.
	broken, _ := newSettleService(t, pool, fake)
	broken.clearing = ledger.ClearingAccountID(uuid.New())
	clearing := uuid.UUID(clearingFor(t, pool, rail.FakeName))
	payer, payee := newAccount(t.Context(), t, pool), newAccount(t.Context(), t, pool)
	services := []*Service{svc, broken}
	steps := []rail.Step{{}, {Outcome: rail.Declined}, {Outcome: rail.Pending}, {Err: errors.New("connection reset")}, {LoseResponse: true}}
	var keys int

	rapid.Check(t, func(rt *rapid.T) {
		var ids []uuid.UUID // this run's payments
		pick := func(rt *rapid.T) uuid.UUID {
			if len(ids) == 0 {
				rt.Skip("no payments yet")
			}
			return rapid.SampledFrom(ids).Draw(rt, "payment")
		}
		rt.Repeat(map[string]func(*rapid.T){
			"create": func(rt *rapid.T) {
				fake.Push(rapid.SampledFrom(steps).Draw(rt, "step"))
				s := rapid.SampledFrom(services).Draw(rt, "service")
				keys++
				ids = append(ids, createFor(t, s, fmt.Sprintf("p%d", keys), payer, payee).PaymentID)
			},
			"resolve pending": func(rt *rapid.T) {
				// Errors (not pending) are expected and ignored.
				_ = fake.Resolve(rail.Ref(pick(rt)), rapid.SampledFrom([]rail.Outcome{rail.Settled, rail.Declined}).Draw(rt, "outcome"), "")
			},
			"recover": func(rt *rapid.T) {
				if _, err := rapid.SampledFrom(services).Draw(rt, "service").Recover(t.Context()); err != nil {
					rt.Fatalf("Recover: %v", err)
				}
			},
			// A repeated or contradicting answer from a stale snapshot, as a
			// second request or a concurrent Recover would deliver it.
			"finish again": func(rt *rapid.T) {
				p, err := svc.Get(t.Context(), pick(rt))
				if err != nil {
					rt.Fatal(err)
				}
				to := rapid.SampledFrom([]State{Settled, Failed}).Draw(rt, "to")
				rapid.SampledFrom(services).Draw(rt, "service").finish(t.Context(), p, to, FailureDeclined)
			},
			"": func(rt *rapid.T) {
				for _, id := range ids {
					p, err := svc.Get(t.Context(), id)
					if err != nil {
						rt.Fatal(err)
					}
					legs := payerEntries(t, pool, id)
					if (p.State == Settled) != (legs == 2) || (p.State != Settled && legs != 0) {
						rt.Fatalf("payment %s is %s with %d payer legs; want settled iff 2", id, p.State, legs)
					}
				}
			},
		})
		// Cumulative across runs: the clearing account holds 1250 per
		// settled payment on the fake rail, ever.
		var total int
		if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM payments WHERE state = 'settled'").Scan(&total); err != nil {
			rt.Fatal(err)
		}
		if got, err := ledger.Balance(t.Context(), pool, clearing); err != nil || got != int64(total)*1250 {
			rt.Fatalf("clearing:fake balance = %d (%v), want %d for %d settled payments", got, err, int64(total)*1250, total)
		}
	})
}

// A result outside the three outcomes is not a decision: the payment stays
// settling and the line is logged at Error.
func TestSettle_UnknownOutcomeLeavesSettling(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	svc, logs := newSettleService(t, pool, &hookRail{Adapter: rail.NewFake(), answer: &rail.Result{Outcome: "refunded"}})
	res := createFor(t, svc, "k", newAccount(t.Context(), t, pool), newAccount(t.Context(), t, pool))
	assertState(t, svc, res.PaymentID, Settling, nil)
	if !strings.Contains(logs.String(), `"level":"ERROR","msg":"rail returned an unknown outcome`) {
		t.Fatalf("no Error line for the unknown outcome:\n%s", logs)
	}
}

// Review should 5: a Recover pass leaves alone a payment whose first settle
// is still running (it is younger than the grace period), so the rail is
// called once. Without the grace, the pass races the live call; the rail's
// per-reference idempotency still moves the money once (ADR-120).
func TestRecover_GraceLeavesInFlightSettleAlone(t *testing.T) {
	t.Parallel()
	for name, grace := range map[string]time.Duration{"default grace": defaultRecoverGrace, "no grace": 0} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pool := testutil.Postgres(t)
			fake := rail.NewFake(rail.Step{Delay: 300 * time.Millisecond})
			started := make(chan struct{})
			live := &hookRail{Adapter: fake, onSettle: func(rail.Request) { close(started) }}
			svc, _ := newSettleService(t, pool, live)
			payer, payee := newAccount(t.Context(), t, pool), newAccount(t.Context(), t, pool)
			done := make(chan CreateResult)
			go func() { done <- createFor(t, svc, "k", payer, payee) }()
			<-started // committed as settling; the rail call is in flight

			pass := &hookRail{Adapter: fake}
			recoverer, _ := newSettleService(t, pool, pass)
			recoverer.recoverGrace = grace
			if _, err := recoverer.Recover(t.Context()); err != nil {
				t.Fatalf("Recover: %v", err)
			}
			res := <-done
			assertState(t, svc, res.PaymentID, Settled, nil)
			if n := fake.Moves(rail.Ref(res.PaymentID)); n != 1 {
				t.Fatalf("money moved %d times, want 1", n)
			}
			calls := pass.settles.Load() + pass.statuses.Load()
			if grace > 0 && calls != 0 {
				t.Fatalf("Recover called the rail %d times inside the grace period, want 0", calls)
			}
			if grace == 0 && calls == 0 {
				t.Fatal("without a grace period Recover should have raced the live call")
			}
		})
	}
}

// Recover stops as soon as its context ends, mid-page: no further rail
// calls, and the rest of the backlog stays settling for the next pass.
func TestRecover_StopsWhenCancelledMidPage(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	down, _ := newSettleService(t, pool, &hookRail{Adapter: internalRail(t, pool), fail: errors.New("connection refused")})
	payer, payee := newAccount(t.Context(), t, pool), newAccount(t.Context(), t, pool)
	const n = 5
	for i := range n {
		createFor(t, down, "k"+string(rune('a'+i)), payer, payee)
	}
	ctx, cancel := context.WithCancel(t.Context())
	pass := &hookRail{Adapter: internalRail(t, pool), onStatus: cancel}
	svc, _ := newSettleService(t, pool, pass)
	if _, err := svc.Recover(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Recover = %v, want context.Canceled", err)
	}
	if got := pass.statuses.Load() + pass.settles.Load(); got != 1 {
		t.Fatalf("rail called %d times after the cancel, want 1 (the call that cancelled)", got)
	}
	var settling int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM payments WHERE state = 'settling'").Scan(&settling); err != nil || settling != n {
		t.Fatalf("settling = %d (%v), want %d", settling, err, n)
	}
}

// Review should 1: the backlog query is answerable from the partial index
// alone (rail, updated_at, id) WHERE state = 'settling'. Seq scans are
// disabled so the planner must use an index if one fits; a missing or
// mis-ordered index shows up as a Sort or a scan of another index.
func TestListSettling_UsesSettlingIndex(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	if _, err := tx.Exec(t.Context(), "SET LOCAL enable_seqscan = off"); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(t.Context(), "EXPLAIN "+listSettlingSQL,
		rail.InternalName, 15.0, time.Time{}, pgtype.UUID{Valid: true}, 100)
	if err != nil {
		t.Fatal(err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	plan := strings.Join(lines, "\n")
	if !strings.Contains(plan, "payments_settling_idx") || strings.Contains(plan, "Sort") {
		t.Fatalf("plan doesn't read the settling index in order:\n%s", plan)
	}
}

// Review should 2: the Redis in-flight lock is released before the rail
// call. A duplicate arriving while the first request waits on a slow rail
// doesn't wait for the rail: it finds the completed key and replays the 201
// at once, and the replay never calls the rail.
func TestCreate_LockReleasedBeforeRailCall(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	fake := rail.NewFake(rail.Step{Delay: time.Second})
	started := make(chan struct{})
	r := &hookRail{Adapter: fake, onSettle: func(rail.Request) { close(started) }}
	var logs syncBuffer
	log := httpx.NewLogger(&logs, slog.LevelDebug)
	svc := New(pool, WithRail(r, clearingFor(t, pool, r.Name())), WithLogger(log), WithLocker(idem.NewRedisLocker(testutil.Redis(t), log)))
	payer, payee := newAccount(t.Context(), t, pool), newAccount(t.Context(), t, pool)
	done := make(chan CreateResult)
	go func() { done <- createFor(t, svc, "k", payer, payee) }()
	<-started

	begin := time.Now()
	dup := createFor(t, svc, "k", payer, payee)
	if waited := time.Since(begin); !dup.Replayed || waited > 500*time.Millisecond {
		t.Fatalf("duplicate = replayed %v after %v; want a replay without waiting for the 1 s rail call", dup.Replayed, waited)
	}
	first := <-done
	if n := r.settles.Load(); n != 1 {
		t.Fatalf("rail Settle called %d times, want 1", n)
	}
	assertState(t, svc, first.PaymentID, Settled, nil)
}

// hookRail wraps a rail to count calls, run a hook at the start of each, or
// answer Settle itself: with fail (as if the rail wasn't reached) or answer
// (a canned result) instead of calling the rail.
type hookRail struct {
	rail.Adapter
	onSettle func(rail.Request)
	onStatus func()
	fail     error
	answer   *rail.Result

	settles, statuses atomic.Int32
}

func (h *hookRail) Settle(ctx context.Context, req rail.Request) (rail.Result, error) {
	h.settles.Add(1)
	if h.onSettle != nil {
		h.onSettle(req)
	}
	switch {
	case h.fail != nil:
		return rail.Result{}, h.fail
	case h.answer != nil:
		return *h.answer, nil
	}
	return h.Adapter.Settle(ctx, req)
}

func (h *hookRail) Status(ctx context.Context, ref string) (rail.Result, error) {
	h.statuses.Add(1)
	if h.onStatus != nil {
		h.onStatus()
	}
	return h.Adapter.Status(ctx, ref)
}

// newSettleService is a Service settling on r, logging (redacted, as in
// production) to the returned buffer. Recover has no grace period, so tests
// can recover a payment the moment it is left settling;
// TestRecover_GraceLeavesInFlightSettleAlone tests the grace.
func newSettleService(t testing.TB, pool *pgxpool.Pool, r rail.Adapter) (*Service, *syncBuffer) {
	t.Helper()
	var buf syncBuffer
	svc := New(pool, WithRail(r, clearingFor(t, pool, r.Name())), WithLogger(httpx.NewLogger(&buf, slog.LevelDebug)))
	svc.recoverGrace = 0
	return svc, &buf
}

// createFor creates a 12.50 GBP payment with Idempotency-Key key.
func createFor(t *testing.T, svc *Service, key string, payer, payee uuid.UUID) CreateResult {
	t.Helper()
	k, err := idem.ParseKey(idem.AnonymousCaller, key)
	if err != nil {
		t.Fatal(err)
	}
	res, err := svc.CreateIdempotent(t.Context(), k, sha256.Sum256([]byte(key)), CreateInput{Amount: 1250, Currency: Currency, PayerAccountID: payer, PayeeAccountID: payee})
	if err != nil {
		t.Fatalf("CreateIdempotent: %v", err)
	}
	return res
}

func assertState(t *testing.T, svc *Service, id uuid.UUID, want State, reason *FailureReason) {
	t.Helper()
	p, err := svc.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if p.State != want || (p.FailureReason == nil) != (reason == nil) || (reason != nil && *p.FailureReason != *reason) {
		t.Fatalf("payment = %s (reason %v), want %s (reason %v)", p.State, deref(p.FailureReason), want, deref(reason))
	}
}

func deref(r *FailureReason) any {
	if r == nil {
		return nil
	}
	return *r
}

func ledgerEntries(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM ledger_entries WHERE payment_id = $1", pgtype.UUID{Bytes: id, Valid: true}).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// payerEntries counts the legs of payment id's payer posting (PayerRef).
func payerEntries(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM ledger_entries WHERE rail_ref = $1", PayerRef(id)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func assertBalances(t *testing.T, pool *pgxpool.Pool, want map[uuid.UUID]int64) {
	t.Helper()
	for acct, w := range want {
		if got, err := ledger.Balance(t.Context(), pool, acct); err != nil || got != w {
			t.Errorf("balance of %s = %d (%v), want %d", acct, got, err, w)
		}
	}
}

// syncBuffer is a bytes.Buffer safe for the concurrent writes of a logger.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
