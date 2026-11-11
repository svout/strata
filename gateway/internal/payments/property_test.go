package payments

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"pgregory.net/rapid"

	"github.com/svout/strata/gateway/internal/idem"
	"github.com/svout/strata/gateway/internal/ledger"
	"github.com/svout/strata/gateway/internal/rail"
	"github.com/svout/strata/gateway/internal/testutil"
)

// SPEC-002 subtask 10c: AC1 (money conservation) through this package, where
// a settled payment is two postings written by two components (ADR-123). The
// ledger-level property (ledger.TestPropertyMoneyConservation) proves Post
// conserves money; this one proves the payment lifecycle does. Refunds are
// covered only there: this package has no refund path in P1.
//
// It also checks, after every operation, that a payment is settled if and
// only if its payer posting exists: balances can't show that, because a
// settled payment with no posting still sums to zero.
//
// Operations run one at a time, so a failure shrinks to a sequence of calls.
// Races are other tests' job (AC3, TestRecover_ConcurrentPassesSettleOnce,
// the AC5 webhook tests, the chaos run in subtask 11).

// Operations per rapid check, and per subtest: two subtests x 100 checks x 50
// = AC1's 10,000.
const (
	conservationOpsPerCheck = 50
	conservationMinOps      = 5_000
)

// fakeStepChoices is every way the fake PSP can answer a first Settle: settled,
// declined, pending, an error before it acted, and a lost response after it
// moved the money.
var fakeStepChoices = []rail.Step{{}, {Outcome: rail.Declined}, {Outcome: rail.Pending}, {Err: errors.New("connection reset")}, {LoseResponse: true}}

type conservationKind int

const (
	conservationCreate     conservationKind = iota // a new payment, on a drawn service
	conservationReplay                             // the same key again; nothing new may happen
	conservationMarkFailed                         // a created payment fails
	conservationResolve                            // a pending fake operation finishes
	conservationHint                               // a rail webhook names a payment (ADR-124)
	conservationRecover                            // a recovery pass
	conservationFinish                             // an answer delivered a second time
)

func (k conservationKind) String() string {
	return [...]string{"create", "replay", "mark failed", "resolve", "hint", "recover", "finish"}[k]
}

// conservationOp is one drawn operation, drawn whole before it is applied so
// that shrinking one operation doesn't change what the others draw.
type conservationOp struct {
	kind    conservationKind
	amount  int64
	service int // reduced modulo the services the operation can use
	pick    int // reduced modulo the payments the operation can act on
	step    rail.Step
	outcome rail.Outcome // for Resolve
	to      State        // for a contradicting answer to a finished payment
}

var conservationOpGen = rapid.Custom(func(t *rapid.T) conservationOp {
	return conservationOp{
		kind: rapid.SampledFrom([]conservationKind{
			conservationCreate, conservationCreate, conservationCreate, conservationReplay, conservationMarkFailed,
			conservationResolve, conservationHint, conservationRecover, conservationFinish,
		}).Draw(t, "kind"),
		amount:  rapid.Int64Range(1, 1_000_000).Draw(t, "amount"),
		service: rapid.IntRange(0, 1<<16).Draw(t, "service"),
		pick:    rapid.IntRange(0, 1<<16).Draw(t, "pick"),
		step:    rapid.SampledFrom(fakeStepChoices).Draw(t, "step"),
		outcome: rapid.SampledFrom([]rail.Outcome{rail.Settled, rail.Declined}).Draw(t, "outcome"),
		to:      rapid.SampledFrom([]State{Settled, Failed}).Draw(t, "to"),
	}
})

// Where a modelled payment was created.
const (
	onInternal = rail.InternalName
	onFake     = rail.FakeName
	onNoRail   = "none"
)

// modelPayment is the test's record of one payment. The final state comes
// from the database; the model holds what the test needs to judge it.
type modelPayment struct {
	id     uuid.UUID
	key    string
	input  CreateInput
	source string // onInternal, onFake or onNoRail
	marked bool   // MarkFailed was called on it
}

// The property runs once per rail, in parallel, each on its own database, to
// keep the package's test time down (measurements, dated, in SPEC-002
// subtask 10c's as-built note; not repeated here, where they would go
// stale). Payments are created on one rail per subtest, but webhooks and
// recovery still come from services on both.
func TestProperty_MoneyConservationThroughPayments(t *testing.T) {
	t.Parallel()
	for _, onRail := range []string{rail.InternalName, rail.FakeName} {
		t.Run(onRail, func(t *testing.T) {
			t.Parallel()
			testMoneyConservation(t, onRail)
		})
	}
}

func testMoneyConservation(t *testing.T, onRail string) {
	// One database for every check of this subtest: each check makes its
	// own payer and payee, and the clearing accounts are compared against
	// a baseline.
	pool := testutil.Postgres(t)
	ctx := t.Context()
	// Replaced at the start of every check (below), so a step pushed by a
	// check that failed before using it can't answer the next check's
	// Settle.
	fake := rail.NewFake(pool)
	internal := internalRail(t, pool)

	goodInternal, _ := newSettleService(t, pool, internal)
	// The broken services record to a clearing account that doesn't exist,
	// so recording settled fails at the foreign key and rolls back. On the
	// internal rail the rail's own posting has already committed by then.
	brokenInternal, _ := newSettleService(t, pool, internal)
	brokenInternal.clearing = ledger.ClearingAccountID(uuid.New())
	unknownInternal, _ := newSettleService(t, pool, &hookRail{Adapter: internal, fail: errors.New("connection reset")})
	invalidInternal, _ := newSettleService(t, pool, &hookRail{Adapter: internal, fail: rail.ErrInvalidRequest})
	goodFake, _ := newSettleService(t, pool, fake)
	brokenFake, _ := newSettleService(t, pool, fake)
	brokenFake.clearing = ledger.ClearingAccountID(uuid.New())

	internals := []*Service{goodInternal, brokenInternal, unknownInternal, invalidInternal}
	fakes := []*Service{goodFake, brokenFake}
	railed := append(append([]*Service{}, internals...), fakes...)
	creators := append([]*Service{New(pool)}, internals...)
	if onRail == rail.FakeName {
		creators = append([]*Service{New(pool)}, fakes...)
	}
	for _, s := range railed {
		// Deadlines are not what this property tests: on a loaded machine
		// the 300 ms lookup deadline would fail a check with no money at
		// stake, and the settle timeout would leave a payment for the drain.
		s.webhookLookupTimeout, s.settleTimeout = time.Minute, time.Minute
	}
	internalClearing := uuid.UUID(clearingFor(t, pool, rail.InternalName))
	fakeClearing := uuid.UUID(clearingFor(t, pool, rail.FakeName))

	balance := func(rt *rapid.T, account uuid.UUID) int64 {
		got, err := ledger.Balance(ctx, pool, account)
		if err != nil {
			rt.Fatalf("Balance: %v", err)
		}
		return got
	}
	// drain finishes everything still settling: it answers every pending
	// fake operation, then runs recovery on the working services, after
	// which nothing may be left. It lists from the database, not the
	// model, so that it also clears what a failed check left behind while
	// rapid shrinks. That is also why the outcome is not a draw: how many
	// payments are left is database state, and a number of draws that
	// depends on it would make rapid's replay of a failing case draw
	// different values from the original. created maps this check's
	// payments to the operation that created them (see drainOutcome).
	drain := func(rt *rapid.T, created map[uuid.UUID]int) {
		pending, err := listSettling(ctx, pool, rail.FakeName, 0, settlingCursor{}, 1000)
		if err != nil {
			rt.Fatalf("listSettling: %v", err)
		}
		for _, p := range pending {
			// A payment missing from created was left by an earlier check
			// that failed; op 0 is as good as any for it.
			outcome := drainOutcome(p.Amount, created[p.ID])
			if err := fake.Resolve(ctx, rail.Ref(p.ID), outcome, ""); err != nil && !errors.Is(err, rail.ErrNotPending) && !errors.Is(err, rail.ErrUnknownRef) {
				rt.Fatalf("Resolve: %v", err)
			}
		}
		for _, s := range []*Service{goodInternal, goodFake} {
			if _, err := s.Recover(ctx); err != nil {
				rt.Fatalf("Recover: %v", err)
			}
		}
		var left int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM payments WHERE state = 'settling'").Scan(&left); err != nil {
			rt.Fatalf("count settling: %v", err)
		}
		if left != 0 {
			rt.Fatalf("%d payments still settling after the drain, want 0", left)
		}
	}

	// newAccount fails through rt, not t, so a failing case still shrinks
	// (the reason LedgerSums returns an error).
	newAccount := func(rt *rapid.T) uuid.UUID {
		var id pgtype.UUID
		if err := pool.QueryRow(ctx, "INSERT INTO accounts (kind, currency) VALUES ('user', 'GBP') RETURNING id").Scan(&id); err != nil {
			rt.Fatalf("insert account: %v", err)
		}
		return uuid.UUID(id.Bytes)
	}

	var keys, ops int
	finals := map[string]int{}
	kinds := map[conservationKind]int{} // operations run, after substitution

	rapid.Check(t, func(rt *rapid.T) {
		fake = rail.NewFake(pool)
		goodFake.rail, brokenFake.rail = fake, fake
		drain(rt, nil)
		payer, payee := newAccount(rt), newAccount(rt)
		want := map[uuid.UUID]int64{
			payer: 0, payee: 0,
			internalClearing: balance(rt, internalClearing),
			fakeClearing:     balance(rt, fakeClearing),
		}
		var payments []*modelPayment
		var ids []uuid.UUID
		created := map[uuid.UUID]int{}

		// choose returns a payment that keep accepts, chosen by pick, or nil.
		choose := func(pick int, keep func(*modelPayment) bool) *modelPayment {
			var match []*modelPayment
			for _, p := range payments {
				if keep(p) {
					match = append(match, p)
				}
			}
			if len(match) == 0 {
				return nil
			}
			return match[pick%len(match)]
		}
		anyPayment := func(*modelPayment) bool { return true }
		onARail := func(p *modelPayment) bool { return p.source != onNoRail }
		failable := func(p *modelPayment) bool { return p.source == onNoRail && !p.marked }
		isFake := func(p *modelPayment) bool { return p.source == onFake }

		submit := func(s *Service, key string, in CreateInput) CreateResult {
			k, err := idem.ParseKey(idem.AnonymousCaller, key)
			if err != nil {
				rt.Fatalf("ParseKey: %v", err)
			}
			res, err := s.CreateIdempotent(ctx, k, sha256.Sum256([]byte(key)), in)
			if err != nil {
				rt.Fatalf("CreateIdempotent: %v", err)
			}
			return res
		}

		for i, o := range rapid.SliceOfN(conservationOpGen, conservationOpsPerCheck, conservationOpsPerCheck).Draw(rt, "ops") {
			// An operation with nothing to act on creates a payment
			// instead, so every drawn operation does something.
			var p *modelPayment
			switch o.kind {
			case conservationReplay, conservationHint:
				p = choose(o.pick, anyPayment)
			case conservationMarkFailed:
				p = choose(o.pick, failable)
			case conservationResolve:
				p = choose(o.pick, isFake)
			case conservationFinish:
				p = choose(o.pick, onARail)
			}
			kind := o.kind
			if kind != conservationRecover && kind != conservationCreate && p == nil {
				kind = conservationCreate
			}

			switch kind {
			case conservationCreate:
				s := creators[o.service%len(creators)]
				source := onNoRail
				if s.rail != nil {
					source = s.rail.Name()
				}
				if source == onFake {
					fake.Push(o.step)
				}
				keys++
				key := fmt.Sprintf("conserve-%d", keys)
				in := CreateInput{Amount: o.amount, Currency: Currency, PayerAccountID: payer, PayeeAccountID: payee}
				res := submit(s, key, in)
				payments = append(payments, &modelPayment{id: res.PaymentID, key: key, input: in, source: source})
				ids = append(ids, res.PaymentID)
				created[res.PaymentID] = i
			case conservationReplay:
				res := submit(creators[o.service%len(creators)], p.key, p.input)
				if !res.Replayed || res.PaymentID != p.id {
					rt.Fatalf("replay of %s: replayed=%v id=%s, want the stored answer for %s", p.key, res.Replayed, res.PaymentID, p.id)
				}
			case conservationMarkFailed:
				// Created payments only: MarkFailed can also fail a settling
				// payment without asking the rail, which after the internal
				// rail's posting would be a failed payment whose money moved.
				// Nothing in the service does that (finish calls it only on a
				// rail's answer).
				if _, err := goodInternal.MarkFailed(ctx, p.id, FailureDeclined); err != nil {
					rt.Fatalf("MarkFailed: %v", err)
				}
				p.marked = true
			case conservationResolve:
				if err := fake.Resolve(ctx, rail.Ref(p.id), o.outcome, ""); err != nil && !errors.Is(err, rail.ErrNotPending) && !errors.Is(err, rail.ErrUnknownRef) {
					rt.Fatalf("Resolve: %v", err)
				}
			case conservationHint:
				// A rail's webhook may name any payment, including one on
				// another rail, one with no rail, or one already finished.
				res, err := railed[o.service%len(railed)].hint(ctx, p.id)
				if err != nil {
					rt.Fatalf("hint: %v", err)
				}
				if res == hintShed {
					rt.Fatalf("hint shed with no other webhook running")
				}
			case conservationRecover:
				if _, err := railed[o.service%len(railed)].Recover(ctx); err != nil {
					rt.Fatalf("Recover: %v", err)
				}
			case conservationFinish:
				services := internals
				if p.source == onFake {
					services = fakes
				}
				s := services[o.service%len(services)]
				cur, err := s.Get(ctx, p.id)
				if err != nil {
					rt.Fatalf("Get: %v", err)
				}
				to, answered := o.to, true
				if cur.State == Settling {
					// A settling payment gets only the answer the rail would
					// give: finish trusts its caller, and an invented answer
					// would be a rail breaking ADR-120, not a payments bug.
					res, err := s.rail.Status(ctx, ref(cur))
					if err != nil && !errors.Is(err, rail.ErrUnknownRef) {
						rt.Fatalf("Status: %v", err)
					}
					switch {
					case err != nil || res.Outcome == rail.Pending:
						answered = false
					case res.Outcome == rail.Settled:
						to = Settled
					default:
						to = Failed
					}
				}
				if answered {
					s.finish(ctx, cur, to, FailureDeclined)
				}
			}
			ops++
			kinds[kind]++
			// Where a failure happened: the operation's index in the check
			// and the payment it acted on, if any.
			at := fmt.Sprintf("op %d (%v)", i, kind)
			if p != nil {
				at += " on " + p.id.String()
			}

			// Every posting balances at every point, including while a
			// payment is half done (the rail has posted, payments hasn't).
			if unbalanced, total, err := testutil.LedgerSums(ctx, pool, ids...); err != nil {
				rt.Fatalf("LedgerSums: %v", err)
			} else if unbalanced != 0 || total != 0 {
				rt.Fatalf("after %s: %d unbalanced postings, signed sum %d; want 0 and 0", at, unbalanced, total)
			}
			// And at every point, not only after the drain: settled iff the
			// payer posting exists. Checking it only at the end would assume
			// the settle transaction is atomic, which is what is under test.
			if n, example, err := payerPostingMismatches(ctx, pool, ids); err != nil {
				rt.Fatalf("payerPostingMismatches: %v", err)
			} else if n != 0 {
				rt.Fatalf("after %s: %d payments (one is %s) settled without their payer posting or posted without settling; want 0", at, n, example)
			}
		}

		drain(rt, created)

		for _, p := range payments {
			got, err := goodInternal.Get(ctx, p.id)
			if err != nil {
				rt.Fatalf("Get: %v", err)
			}
			finals[p.source+"/"+string(got.State)]++
			var entries, payerLegs, railLegs int
			err = pool.QueryRow(ctx, `
				SELECT count(*), count(*) FILTER (WHERE rail_ref = $2), count(*) FILTER (WHERE rail_ref = $3)
				FROM ledger_entries WHERE payment_id = $1`,
				pgtype.UUID{Bytes: p.id, Valid: true}, PayerRef(p.id), rail.Ref(p.id)).Scan(&entries, &payerLegs, &railLegs)
			if err != nil {
				rt.Fatalf("count entries: %v", err)
			}
			moves := 0
			if p.source == onFake {
				if moves, err = fake.Moves(ctx, rail.Ref(p.id)); err != nil {
					rt.Fatalf("Moves: %v", err)
				}
			}

			switch got.State {
			case Settled:
				wantRail, wantMoves := 0, 1
				if p.source == onInternal {
					wantRail, wantMoves = 2, 0
				}
				if payerLegs != 2 || railLegs != wantRail || entries != 2+wantRail || moves != wantMoves {
					rt.Errorf("settled %s payment %s: payer legs %d, rail legs %d, entries %d, fake moves %d; want 2, %d, %d, %d",
						p.source, p.id, payerLegs, railLegs, entries, moves, wantRail, 2+wantRail, wantMoves)
				}
				want[payer] -= p.input.Amount
				if p.source == onInternal {
					want[payee] += p.input.Amount
				} else {
					want[fakeClearing] += p.input.Amount
				}
			case Failed, Created:
				if entries != 0 || moves != 0 {
					rt.Errorf("%s %s payment %s: %d entries, %d fake moves; want none", got.State, p.source, p.id, entries, moves)
				}
				// Only a payment with no rail stays created, and only
				// MarkFailed fails one.
				if p.source == onNoRail && (got.State == Failed) != p.marked {
					rt.Errorf("no-rail payment %s is %s, marked failed = %v", p.id, got.State, p.marked)
				}
				if p.source != onNoRail && got.State == Created {
					rt.Errorf("%s payment %s is still created", p.source, p.id)
				}
			default:
				rt.Errorf("payment %s is %s after the drain", p.id, got.State)
			}
		}
		for account, w := range want {
			if got := balance(rt, account); got != w {
				rt.Errorf("Balance(%v) = %d, want %d", account, got, w)
			}
		}
		// Every account's balance is a sum of entries, so the whole table
		// summing to zero is all balances summing to zero.
		if unbalanced, total, err := testutil.LedgerSums(ctx, pool); err != nil {
			rt.Fatalf("LedgerSums: %v", err)
		} else if unbalanced != 0 || total != 0 {
			rt.Errorf("whole ledger: %d unbalanced postings, signed sum %d; want 0 and 0", unbalanced, total)
		}
	})

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

	t.Logf("ran %d operations; final states: %v", ops, finals)
	if testing.Short() {
		return
	}
	if ops < conservationMinOps {
		t.Errorf("ran %d operations, want at least %d (AC1)", ops, conservationMinOps)
	}
	// Coverage guard: a property that never reaches an outcome proves nothing
	// about it.
	for _, k := range []string{onRail + "/settled", onRail + "/failed", "none/created", "none/failed"} {
		if finals[k] == 0 {
			t.Errorf("no payment ended as %s; the property isn't exercising it", k)
		}
	}
	// And every kind of operation ran: a change to the generator's weights
	// could otherwise stop exercising one while everything still passes.
	// Resolve acts only on fake-rail payments, which the internal subtest
	// never creates, so there it always becomes a create.
	for k := conservationCreate; k <= conservationFinish; k++ {
		if k == conservationResolve && onRail != rail.FakeName {
			continue
		}
		if kinds[k] == 0 {
			t.Errorf("no %v operation ran; the property isn't exercising it", k)
		}
	}
}

// drainOutcome is how the drain answers a pending fake payment created by
// operation op. Both inputs are drawn (the amount) or fixed by the draw (the
// index), so a replayed case drains the same way. Both are there on purpose:
// on the amount alone, outcome and amount would be correlated, and a pairing
// such as "large amount, declined" could never occur however many checks
// ran; the index decorrelates them.
func drainOutcome(amount int64, op int) rail.Outcome {
	if (amount+int64(op))%2 == 0 {
		return rail.Settled
	}
	return rail.Declined
}

// payerPostingMismatches counts the payments in ids that break the invariant
// markSettled exists for (ADR-123): settled without exactly the two legs of
// its payer posting, or not settled with any of them. example is one of
// them, for the failure message.
func payerPostingMismatches(ctx context.Context, pool *pgxpool.Pool, ids []uuid.UUID) (n int, example uuid.UUID, err error) {
	pgIDs := make([]pgtype.UUID, len(ids))
	refs := make([]string, len(ids))
	for i, id := range ids {
		pgIDs[i], refs[i] = pgUUID(id), PayerRef(id)
	}
	var first pgtype.UUID
	err = pool.QueryRow(ctx, `
		SELECT count(*), (array_agg(w.id))[1]
		FROM unnest($1::uuid[], $2::text[]) AS w(id, ref)
		JOIN payments p ON p.id = w.id
		CROSS JOIN LATERAL (
			SELECT count(*) AS legs FROM ledger_entries e
			WHERE e.payment_id = w.id AND e.rail_ref = w.ref) l
		WHERE (p.state = 'settled') <> (l.legs = 2) OR (p.state <> 'settled' AND l.legs <> 0)`,
		pgIDs, refs).Scan(&n, &first)
	return n, uuid.UUID(first.Bytes), err
}
