package rail

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// All of this package's SQL for its own table (ADR-117 amendment, rule 1).
// It touches fake_rail_operations only (migration 00006); the internal
// rail's SQL is ledger's (ledger.PostRef, ledger.Posted).
//
// Every statement runs on the pool, outside any explicit transaction, so
// each is its own READ COMMITTED transaction. That matters for
// insertFakeOperation: see there.

// ErrFakeStore means the fake PSP could not read or write its own table
// (database unreachable, pool closed, table missing). For a caller it is an
// unknown outcome, like any transport failure of a real PSP (ADR-120): never
// ErrInvalidRequest, and from Status never ErrUnknownRef, so recovery doesn't
// re-settle a reference it couldn't look up.
//
// Its text is fixed. Error text is logged verbatim (root CLAUDE.md), and a
// driver error can name the host, user and database; the cause is reachable
// only through errors.Unwrap / errors.As.
var ErrFakeStore = errors.New("rail: fake PSP store unavailable")

// fakeStoreError is ErrFakeStore carrying the driver error as its cause, as
// ledger's lookupError does for ErrClearingLookup. Not fmt.Errorf("%w: %w"):
// that would put the cause's text inside Error().
type fakeStoreError struct{ cause error }

func (e *fakeStoreError) Error() string        { return ErrFakeStore.Error() }
func (e *fakeStoreError) Unwrap() error        { return e.cause }
func (e *fakeStoreError) Is(target error) bool { return target == ErrFakeStore }

// storeErr maps a database error. A context that has ended is reported as
// itself (context.Canceled / DeadlineExceeded, fixed text, and what the
// railtest contract and payments' timeouts expect to see), not as a store
// failure.
func storeErr(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return &fakeStoreError{cause: err}
}

// errVanished is the cause when a row that just won a conflict can't be read
// back. Only a DELETE between the two statements could do that, and nothing
// in the gateway deletes from this table.
var errVanished = errors.New("rail: fake operation missing after insert conflict")

// getFakeOperation reads ref's recorded outcome; found is false if the fake
// has no record of it.
func getFakeOperation(ctx context.Context, pool *pgxpool.Pool, ref string) (res Result, found bool, err error) {
	var outcome string
	var reason *string
	err = pool.QueryRow(ctx, `SELECT outcome, reason FROM fake_rail_operations WHERE ref = $1`, ref).Scan(&outcome, &reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, storeErr(ctx, err)
	}
	res = Result{Outcome: Outcome(outcome)}
	if reason != nil {
		res.Reason = *reason
	}
	return res, true, nil
}

// insertFakeOperation records res for ref unless ref already has a record,
// and returns the record that stands: res if this call inserted it, the
// earlier one if another writer got there first (another Fake, i.e. another
// gateway process; the in-process sharing in Fake.Settle covers one
// process). First writer wins, and the loser moves no money: moves is set
// only by the INSERT that succeeds.
//
// Two statements, not one. A single statement (INSERT ... ON CONFLICT DO
// NOTHING RETURNING, UNION a SELECT of the existing row) reads with the
// snapshot taken when it started: if the conflicting row was committed by a
// concurrent transaction after that, ON CONFLICT waits for it and does
// nothing, but the SELECT can't see it, and the statement returns no row.
// The second statement here gets a fresh snapshot, which includes it.
func insertFakeOperation(ctx context.Context, pool *pgxpool.Pool, ref string, res Result) (Result, error) {
	moves := 0
	if res.Outcome == Settled {
		moves = 1
	}
	tag, err := pool.Exec(ctx, `
		INSERT INTO fake_rail_operations (ref, outcome, reason, moves)
		VALUES ($1, $2, NULLIF($3, ''), $4)
		ON CONFLICT (ref) DO NOTHING`,
		ref, string(res.Outcome), res.Reason, moves)
	if err != nil {
		return Result{}, storeErr(ctx, err)
	}
	if tag.RowsAffected() == 1 {
		return res, nil
	}
	stored, found, err := getFakeOperation(ctx, pool, ref)
	if err != nil {
		return Result{}, err
	}
	if !found {
		return Result{}, &fakeStoreError{cause: errVanished}
	}
	return stored, nil
}

// resolveFakeOperation finishes ref if, and only if, it is still pending:
// the WHERE clause is the guard, so two concurrent Resolves can't both
// apply, and a final outcome is never overwritten. moves goes up by one when
// the money moves (Settled). It returns ErrUnknownRef or ErrNotPending when
// nothing was updated.
func resolveFakeOperation(ctx context.Context, pool *pgxpool.Pool, ref string, res Result) error {
	moved := 0
	if res.Outcome == Settled {
		moved = 1
	}
	tag, err := pool.Exec(ctx, `
		UPDATE fake_rail_operations
		SET outcome = $2, reason = NULLIF($3, ''), moves = moves + $4, updated_at = now()
		WHERE ref = $1 AND outcome = 'pending'`,
		ref, string(res.Outcome), res.Reason, moved)
	if err != nil {
		return storeErr(ctx, err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	// Nothing updated: tell "no such reference" from "already final".
	_, found, err := getFakeOperation(ctx, pool, ref)
	switch {
	case err != nil:
		return err
	case !found:
		return ErrUnknownRef
	}
	return ErrNotPending
}

// fakeMoves reads how many times money moved for ref (0 if unknown).
func fakeMoves(ctx context.Context, pool *pgxpool.Pool, ref string) (int, error) {
	var n int
	err := pool.QueryRow(ctx, `SELECT moves FROM fake_rail_operations WHERE ref = $1`, ref).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, storeErr(ctx, err)
	}
	return n, nil
}
