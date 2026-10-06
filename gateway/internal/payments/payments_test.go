package payments

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"pgregory.net/rapid"

	"github.com/svout/agentgate/gateway/internal/idem"
	"github.com/svout/agentgate/gateway/internal/testutil"
)

// SPEC-002 subtask 7: create/get, forward-only transitions, and a rapid
// property that a payment's state never moves backwards.

func TestCreateGet_RoundTrip(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	ctx := t.Context()
	svc := New(pool)
	payer, payee := newAccount(ctx, t, pool), newAccount(ctx, t, pool)
	mandate := uuid.New()

	p, err := svc.create(ctx, CreateInput{Amount: 1250, Currency: "GBP", PayerAccountID: payer, PayeeAccountID: payee, MandateID: &mandate})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if p.CreatedAt.Location() != time.UTC {
		t.Errorf("created_at in %v, want UTC", p.CreatedAt.Location())
	}
	if p.ID == uuid.Nil || p.State != Created || p.Rail != RailInternal || p.RailRef != nil ||
		p.Amount != 1250 || p.Currency != "GBP" || p.PayerAccountID != payer || p.PayeeAccountID != payee ||
		p.MandateID == nil || *p.MandateID != mandate || p.CreatedAt.IsZero() || !p.UpdatedAt.Equal(p.CreatedAt) {
		t.Fatalf("created payment = %+v", p)
	}

	got, err := svc.Get(ctx, p.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != p.ID || got.State != p.State || !got.CreatedAt.Equal(p.CreatedAt) || *got.MandateID != mandate {
		t.Fatalf("Get = %+v, want %+v", got, p)
	}

	noMandate, err := svc.create(ctx, CreateInput{Amount: 1, Currency: "GBP", PayerAccountID: payer, PayeeAccountID: payee})
	if err != nil || noMandate.MandateID != nil {
		t.Fatalf("Create without mandate = %+v, %v; want nil MandateID", noMandate, err)
	}

	// Subtask 7 posts nothing: settlement (subtask 10) does.
	var entries int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM ledger_entries").Scan(&entries); err != nil || entries != 0 {
		t.Fatalf("ledger entries = %d (%v), want 0", entries, err)
	}
}

func TestCreate_Rejects(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	ctx := t.Context()
	svc := New(pool)
	payer, payee := newAccount(ctx, t, pool), newAccount(ctx, t, pool)
	valid := CreateInput{Amount: 100, Currency: "GBP", PayerAccountID: payer, PayeeAccountID: payee}

	for name, tc := range map[string]struct {
		edit func(*CreateInput)
		want error
	}{
		"zero amount":     {func(in *CreateInput) { in.Amount = 0 }, ErrInvalidAmount},
		"negative amount": {func(in *CreateInput) { in.Amount = -5 }, ErrInvalidAmount},
		"currency":        {func(in *CreateInput) { in.Currency = "EUR" }, ErrUnsupportedCurrency},
		"no currency":     {func(in *CreateInput) { in.Currency = "" }, ErrUnsupportedCurrency},
		"nil payer":       {func(in *CreateInput) { in.PayerAccountID = uuid.Nil }, ErrMissingAccount},
		"nil payee":       {func(in *CreateInput) { in.PayeeAccountID = uuid.Nil }, ErrMissingAccount},
		"same account":    {func(in *CreateInput) { in.PayeeAccountID = payer }, ErrSameAccount},
		"unknown payer":   {func(in *CreateInput) { in.PayerAccountID = uuid.New() }, ErrAccountNotFound},
		"unknown payee":   {func(in *CreateInput) { in.PayeeAccountID = uuid.New() }, ErrAccountNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			in := valid
			tc.edit(&in)
			if _, err := svc.create(ctx, in); !errors.Is(err, tc.want) {
				t.Fatalf("Create = %v, want %v", err, tc.want)
			}
		})
	}

	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM payments").Scan(&n); err != nil || n != 0 {
		t.Fatalf("payments rows = %d (%v), want 0 after rejected creates", n, err)
	}
}

func TestGet_NotFound(t *testing.T) {
	t.Parallel()
	svc := New(testutil.Postgres(t))
	if _, err := svc.Get(t.Context(), uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get unknown = %v, want ErrNotFound", err)
	}
}

// Every legal edge succeeds and moves updated_at; every other call from each
// state fails with ErrInvalidTransition and leaves the row unchanged.
func TestTransitions_ForwardOnly(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	ctx := t.Context()
	svc := New(pool)
	payer, payee := newAccount(ctx, t, pool), newAccount(ctx, t, pool)

	// at returns a fresh payment in state s, reached by legal edges.
	at := func(t *testing.T, s State) Payment {
		t.Helper()
		p, err := svc.create(ctx, CreateInput{Amount: 10, Currency: "GBP", PayerAccountID: payer, PayeeAccountID: payee})
		for _, step := range pathTo[s] {
			if err == nil {
				p, err = apply(ctx, svc, p.ID, step)
			}
		}
		if err != nil {
			t.Fatalf("reach %s: %v", s, err)
		}
		return p
	}

	for _, start := range []State{Created, Settling, Settled, Failed} {
		for _, to := range []State{Settling, Settled, Failed} {
			legal := allowed(start, to)
			t.Run(string(start)+"->"+string(to), func(t *testing.T) {
				before := at(t, start)
				got, err := apply(ctx, svc, before.ID, to)
				after, getErr := svc.Get(ctx, before.ID)
				if getErr != nil {
					t.Fatalf("Get: %v", getErr)
				}
				if legal {
					if err != nil || got.State != to || after.State != to || !after.UpdatedAt.After(before.UpdatedAt) {
						t.Fatalf("legal edge: err=%v state=%s stored=%s updated_at %v -> %v", err, got.State, after.State, before.UpdatedAt, after.UpdatedAt)
					}
					return
				}
				if !errors.Is(err, ErrInvalidTransition) {
					t.Fatalf("illegal edge: err = %v, want ErrInvalidTransition", err)
				}
				if after.State != before.State || !after.UpdatedAt.Equal(before.UpdatedAt) || !equalPtr(after.RailRef, before.RailRef) {
					t.Fatalf("illegal edge changed the row: %+v -> %+v", before, after)
				}
			})
		}
	}

	t.Run("rail_ref", func(t *testing.T) {
		p := at(t, Settling)
		if p.RailRef == nil || *p.RailRef != "rail-"+p.ID.String() {
			t.Fatalf("rail_ref = %v, want the value passed to MarkSettling", p.RailRef)
		}
		if _, err := svc.MarkSettling(ctx, at(t, Created).ID, ""); !errors.Is(err, ErrMissingRailRef) {
			t.Fatalf("empty rail ref: %v, want ErrMissingRailRef", err)
		}
	})
	t.Run("unknown id", func(t *testing.T) {
		for _, to := range []State{Settling, Settled, Failed} {
			if _, err := apply(ctx, svc, uuid.New(), to); !errors.Is(err, ErrNotFound) {
				t.Fatalf("%s on unknown id: %v, want ErrNotFound", to, err)
			}
		}
	})
}

// Two transitions racing on one settling payment: exactly one wins, and the
// stored state is the winner's.
func TestTransitions_ConcurrentOneWins(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	ctx := t.Context()
	svc := New(pool)
	payer, payee := newAccount(ctx, t, pool), newAccount(ctx, t, pool)

	for range 20 {
		p, err := svc.create(ctx, CreateInput{Amount: 10, Currency: "GBP", PayerAccountID: payer, PayeeAccountID: payee})
		if err == nil {
			_, err = svc.MarkSettling(ctx, p.ID, "rail-ref")
		}
		if err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		results := make([]error, 2)
		for i, to := range []State{Settled, Failed} {
			wg.Go(func() { _, results[i] = apply(ctx, svc, p.ID, to) })
		}
		wg.Wait()

		wins := 0
		var winner State
		for i, err := range results {
			switch {
			case err == nil:
				wins++
				winner = []State{Settled, Failed}[i]
			case !errors.Is(err, ErrInvalidTransition):
				t.Fatalf("loser error = %v, want ErrInvalidTransition", err)
			}
		}
		got, err := svc.Get(ctx, p.ID)
		if wins != 1 || err != nil || got.State != winner {
			t.Fatalf("wins = %d, stored %s (%v), winner %s", wins, got.State, err, winner)
		}
	}
}

// Property: under any sequence of transition calls, the stored state's rank
// never decreases, terminal states never change, and each call succeeds
// exactly when the model says the edge is legal.
func TestPropertyTransitionsNeverGoBackwards(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	ctx := t.Context()
	svc := New(pool)
	payer, payee := newAccount(ctx, t, pool), newAccount(ctx, t, pool)

	rapid.Check(t, func(rt *rapid.T) {
		p, err := svc.create(ctx, CreateInput{Amount: 1, Currency: "GBP", PayerAccountID: payer, PayeeAccountID: payee})
		if err != nil {
			rt.Fatalf("Create: %v", err)
		}
		state := Created
		steps := rapid.SliceOfN(rapid.SampledFrom([]State{Settling, Settled, Failed}), 1, 8).Draw(rt, "steps")
		for _, to := range steps {
			_, err := apply(ctx, svc, p.ID, to)
			legal := allowed(state, to)
			if legal != (err == nil) || (!legal && !errors.Is(err, ErrInvalidTransition)) {
				rt.Fatalf("%s -> %s: err = %v, legal = %v", state, to, err, legal)
			}
			got, err := svc.Get(ctx, p.ID)
			if err != nil {
				rt.Fatalf("Get: %v", err)
			}
			if rank[got.State] < rank[state] || (terminal(state) && got.State != state) {
				rt.Fatalf("state went backwards: %s -> %s", state, got.State)
			}
			if legal {
				state = to
			}
			if got.State != state {
				rt.Fatalf("stored %s, model %s", got.State, state)
			}
		}
	})
}

// Property: under any sequence of creates with reused keys and bodies, a key
// is bound to its first successful request: the same request replays the same
// payment, a different one is ErrMismatch, and the number of payments equals
// the number of distinct keys used successfully.
func TestPropertyKeyReuseNeverCreatesSecondPayment(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	ctx := t.Context()
	svc := New(pool)
	payer, payee := newAccount(ctx, t, pool), newAccount(ctx, t, pool)
	var run int

	rapid.Check(t, func(rt *rapid.T) {
		run++
		type bound struct {
			amount int64
			id     uuid.UUID
		}
		model := map[string]bound{}
		// Count every payment, not only those a key points to: a stray
		// insert on the replay path must fail the property too.
		before := countPayments(ctx, rt, pool)
		steps := rapid.IntRange(1, 12).Draw(rt, "steps")
		for range steps {
			key := idem.Key{Value: fmt.Sprintf("run%d-k%d", run, rapid.IntRange(0, 2).Draw(rt, "key"))}
			amount := rapid.Int64Range(1, 3).Draw(rt, "amount")
			hash := idem.Hash(idem.Request{Method: "POST", Path: "/v1/payments", Body: []byte(fmt.Sprint(amount))})
			res, err := svc.CreateIdempotent(ctx, key, hash, CreateInput{Amount: amount, Currency: "GBP", PayerAccountID: payer, PayeeAccountID: payee})

			b, seen := model[key.Value]
			switch {
			case !seen:
				if err != nil || res.Replayed {
					rt.Fatalf("new key %s: %+v, %v", key.Value, res, err)
				}
				model[key.Value] = bound{amount, res.PaymentID}
			case b.amount == amount:
				if err != nil || !res.Replayed || res.PaymentID != b.id {
					rt.Fatalf("reused key %s, same request: %+v, %v; want replay of %s", key.Value, res, err, b.id)
				}
			default:
				if !errors.Is(err, idem.ErrMismatch) {
					rt.Fatalf("reused key %s, other request: %v, want ErrMismatch", key.Value, err)
				}
			}
		}
		after := countPayments(ctx, rt, pool)
		if after-before != len(model) {
			rt.Fatalf("payments created in this run = %d, distinct keys = %d", after-before, len(model))
		}
	})
}

// --- helpers ----------------------------------------------------------------

func countPayments(ctx context.Context, t interface{ Fatal(...any) }, pool *pgxpool.Pool) int {
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM payments").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

var rank = map[State]int{Created: 0, Settling: 1, Settled: 2, Failed: 2}

// pathTo lists the legal steps from created to each state.
var pathTo = map[State][]State{Created: nil, Settling: {Settling}, Settled: {Settling, Settled}, Failed: {Failed}}

func terminal(s State) bool { return s == Settled || s == Failed }

// allowed is the test's own model of the state machine, written out rather
// than read from the package's from table.
func allowed(start, to State) bool {
	switch to {
	case Settling:
		return start == Created
	case Settled:
		return start == Settling
	case Failed:
		return start == Created || start == Settling
	}
	return false
}

func apply(ctx context.Context, svc *Service, id uuid.UUID, to State) (Payment, error) {
	switch to {
	case Settling:
		return svc.MarkSettling(ctx, id, "rail-"+id.String())
	case Settled:
		return svc.MarkSettled(ctx, id)
	default:
		return svc.MarkFailed(ctx, id)
	}
}

func equalPtr(a, b *string) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

// newAccount inserts a user account directly: payments never reads accounts
// (ADR-117), so the fixture does.
func newAccount(ctx context.Context, t testing.TB, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := pool.QueryRow(ctx, "INSERT INTO accounts (kind, currency) VALUES ('user', 'GBP') RETURNING id").Scan(&id); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	return uuid.UUID(id.Bytes)
}
