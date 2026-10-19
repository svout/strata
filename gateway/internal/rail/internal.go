package rail

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/svout/strata/gateway/internal/ledger"
	"github.com/svout/strata/gateway/internal/store"
)

// InternalName is the internal rail's name. It equals payments.RailInternal;
// subtask 10 makes payments use this constant so the rail owns it.
const InternalName = "internal"

// Internal settles a payment by posting it to the ledger: the payer's account
// is debited and the payee's credited, in one SERIALIZABLE transaction of its
// own. It never declines and is never pending.
//
// The posting carries the rail reference, so it happens at most once per
// reference: ledger.Posted, read before the transaction, is the fast path,
// and the ledger's unique constraint is the guarantee (ADR-120). It can't
// share the transaction that claims the Idempotency-Key: the claim commits
// before the rail is called, and a transaction must not stay open across a
// rail call.
type Internal struct {
	pool *pgxpool.Pool
}

// NewInternal returns the internal rail on pool.
func NewInternal(pool *pgxpool.Pool) *Internal {
	return &Internal{pool: pool}
}

// Name implements Adapter.
func (*Internal) Name() string { return InternalName }

// Settle implements Adapter. A repeated reference returns Settled without
// posting again, whatever amount the repeat carries: the reference names the
// operation.
func (r *Internal) Settle(ctx context.Context, req Request) (Result, error) {
	if err := validate(req); err != nil {
		return Result{}, err
	}
	// The fast path reads on the pool, outside the transaction (ADR-120).
	// Inside it, the read takes SERIALIZABLE predicate locks on
	// ledger_entries, so settles of *different* payments conflicted and were
	// retried (~0.8 rollbacks per settle under 16 concurrent settles; 0 with
	// the read outside). The read can now be stale, which is safe: a racing
	// writer that got there first makes PostRef return ErrAlreadyPosted from
	// the unique constraint (directly, or after one retried serialization
	// failure), treated as Settled below.
	posted, err := ledger.Posted(ctx, r.pool, req.Ref)
	if err != nil {
		return Result{}, err
	}
	if posted {
		return Result{Outcome: Settled}, nil
	}
	err = store.Serializable(ctx, r.pool, func(tx pgx.Tx) error {
		return ledger.PostRef(ctx, tx, req.PaymentID, req.Ref, []ledger.Entry{
			{AccountID: req.Payer, Direction: ledger.Debit, Amount: req.Amount},
			{AccountID: req.Payee, Direction: ledger.Credit, Amount: req.Amount},
		})
	})
	if err != nil && !errors.Is(err, ledger.ErrAlreadyPosted) {
		return Result{}, err
	}
	return Result{Outcome: Settled}, nil
}

// Status implements Adapter: Settled once the posting exists, ErrUnknownRef
// before.
func (r *Internal) Status(ctx context.Context, ref string) (Result, error) {
	posted, err := ledger.Posted(ctx, r.pool, ref)
	if err != nil {
		return Result{}, err
	}
	if !posted {
		return Result{}, ErrUnknownRef
	}
	return Result{Outcome: Settled}, nil
}
