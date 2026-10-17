// Package ledger is the only writer of ledger_entries. It posts double-entry
// legs for a payment and reads account balances.
//
// Invariant (SPEC-002, root CLAUDE.md): every posting sums to zero, i.e. its
// debits equal its credits. The schema cannot express this across rows, so
// Post checks it before writing anything, and requires a SERIALIZABLE
// transaction so that concurrent postings behave as if they ran one at a
// time. Callers get one through store.Serializable, which also retries
// serialization failures.
package ledger

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// Direction is the side of a leg. The amount is always positive; the
// direction carries the sign (see the schema's conventions).
type Direction string

const (
	Debit  Direction = "debit"
	Credit Direction = "credit"
)

// Entry is one leg of a posting: Amount minor units (pence) moved on
// AccountID, on the Direction side.
type Entry struct {
	AccountID uuid.UUID
	Direction Direction
	Amount    int64
}

// ErrInvalidPosting is wrapped by every error Post returns for a posting that
// is wrong in itself (as opposed to a database failure), so a caller can map
// all of them to one client-error code with errors.Is. Each specific error
// below also matches with errors.Is.
var ErrInvalidPosting = errors.New("ledger: invalid posting")

var (
	ErrTooFewLegs       = fmt.Errorf("%w: a posting needs at least two legs", ErrInvalidPosting)
	ErrNonPositive      = fmt.Errorf("%w: amount must be greater than zero", ErrInvalidPosting)
	ErrInvalidDirection = fmt.Errorf("%w: direction must be debit or credit", ErrInvalidPosting)
	ErrUnbalanced       = fmt.Errorf("%w: debits do not equal credits", ErrInvalidPosting)
	ErrOverflow         = fmt.Errorf("%w: total exceeds int64", ErrInvalidPosting)
	ErrMissingID        = fmt.Errorf("%w: payment or account id is the nil UUID", ErrInvalidPosting)
	ErrMissingRef       = fmt.Errorf("%w: rail reference must be 1-255 characters", ErrInvalidPosting)
)

// ErrAlreadyPosted means a posting with this rail reference exists: PostRef
// wrote nothing. For a caller settling a rail operation it means the work is
// already done (ADR-120).
var ErrAlreadyPosted = errors.New("ledger: a posting with this rail reference already exists")

// railRefLegKey is migration 00003's UNIQUE (rail_ref, leg) constraint.
const railRefLegKey = "ledger_entries_rail_ref_leg_key"

const sqlstateUniqueViolation = "23505"

// ErrNotSerializable means Post was handed a transaction below SERIALIZABLE.
// That is a programming error in the caller, not a bad request: use
// store.Serializable.
var ErrNotSerializable = errors.New("ledger: transaction is not SERIALIZABLE")

// Post writes entries as the legs of one posting for paymentID, inside tx.
//
// It validates the whole posting before touching the database (at least two
// legs, every amount > 0, a known direction, non-null ids, and total debits
// equal to total credits without int64 overflow), then checks that tx is
// SERIALIZABLE, then inserts every leg in a single statement. If any step
// fails nothing is written by Post; the caller's transaction decides what
// happens to anything else it did.
//
// Post does not stop the same payment being posted twice: each call adds its
// legs. A posting that must happen once (settling a rail operation) uses
// PostRef.
func Post(ctx context.Context, tx pgx.Tx, paymentID uuid.UUID, entries []Entry) error {
	return post(ctx, tx, paymentID, nil, entries)
}

// PostRef is Post for the posting that settles the rail operation railRef. It
// happens at most once per railRef, enforced by the database (UNIQUE
// (rail_ref, leg), migration 00003): if a posting with railRef exists,
// PostRef writes nothing and returns ErrAlreadyPosted, however the writers
// race. Check Posted first in the same transaction as the fast path; under
// SERIALIZABLE that also turns a race into a retried serialization failure
// rather than ErrAlreadyPosted (ADR-120).
func PostRef(ctx context.Context, tx pgx.Tx, paymentID uuid.UUID, railRef string, entries []Entry) error {
	if railRef == "" || len([]rune(railRef)) > 255 {
		return ErrMissingRef
	}
	return post(ctx, tx, paymentID, &railRef, entries)
}

// Posted reports whether a posting with railRef exists.
func Posted(ctx context.Context, q Querier, railRef string) (bool, error) {
	var posted bool
	if err := q.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM ledger_entries WHERE rail_ref = $1)", railRef).Scan(&posted); err != nil {
		return false, fmt.Errorf("ledger: posted: %w", err)
	}
	return posted, nil
}

func post(ctx context.Context, tx pgx.Tx, paymentID uuid.UUID, railRef *string, entries []Entry) error {
	if err := validate(paymentID, entries); err != nil {
		return err
	}

	// pgx.Tx does not expose the options it was begun with, so ask the
	// server. One extra round trip per posting; it turns a silent loss of the
	// isolation guarantee into an error.
	var isolation string
	if err := tx.QueryRow(ctx, "SELECT current_setting('transaction_isolation')").Scan(&isolation); err != nil {
		return fmt.Errorf("ledger: read isolation level: %w", err)
	}
	if isolation != "serializable" {
		return fmt.Errorf("%w (got %q)", ErrNotSerializable, isolation)
	}

	// One INSERT for all legs: unnest turns the parallel arrays back into
	// rows, so the posting costs one round trip however many legs it has.
	// With a rail reference, legs are numbered from 1 in entry order.
	accounts := make([]pgtype.UUID, len(entries))
	directions := make([]string, len(entries))
	amounts := make([]int64, len(entries))
	for i, e := range entries {
		accounts[i], directions[i], amounts[i] = pgUUID(e.AccountID), string(e.Direction), e.Amount
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO ledger_entries (payment_id, account_id, direction, amount, rail_ref, leg)
		SELECT $1, a, d, m, $5::text, CASE WHEN $5::text IS NULL THEN NULL ELSE n::smallint END
		FROM unnest($2::uuid[], $3::text[], $4::bigint[]) WITH ORDINALITY AS t (a, d, m, n)`,
		pgUUID(paymentID), accounts, directions, amounts, railRef)
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == sqlstateUniqueViolation && pgErr.ConstraintName == railRefLegKey {
		return ErrAlreadyPosted
	}
	if err != nil {
		return fmt.Errorf("ledger: insert entries: %w", err)
	}
	return nil
}

// validate checks a posting without any I/O.
func validate(paymentID uuid.UUID, entries []Entry) error {
	if paymentID == uuid.Nil {
		return ErrMissingID
	}
	if len(entries) < 2 {
		return ErrTooFewLegs
	}
	var debits, credits int64
	for i, e := range entries {
		if e.AccountID == uuid.Nil {
			return fmt.Errorf("leg %d: %w", i, ErrMissingID)
		}
		if e.Amount <= 0 {
			return fmt.Errorf("leg %d: %w", i, ErrNonPositive)
		}
		// Amounts are positive, so a sum can only overflow upwards.
		var sum *int64
		switch e.Direction {
		case Debit:
			sum = &debits
		case Credit:
			sum = &credits
		default:
			return fmt.Errorf("leg %d: %w", i, ErrInvalidDirection)
		}
		if *sum > math.MaxInt64-e.Amount {
			return fmt.Errorf("leg %d: %w", i, ErrOverflow)
		}
		*sum += e.Amount
	}
	if debits != credits {
		return ErrUnbalanced
	}
	return nil
}

// Querier is what Balance and Posted need; *pgxpool.Pool, *pgx.Conn and pgx.Tx all
// satisfy it.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Balance returns credits minus debits on accountID, in minor units; 0 for an
// account with no entries (or no such account).
//
// The sign convention is the same for every account kind. The schema's kinds
// (user, merchant, clearing) say who owns an account, not whether it is an
// asset or a liability, so there is no debit-normal kind to flip.
func Balance(ctx context.Context, q Querier, accountID uuid.UUID) (int64, error) {
	// sum(bigint) is numeric, so it cannot overflow mid-sum; the ::bigint
	// cast then fails loudly (22003) rather than wrapping if the result does
	// not fit.
	var balance int64
	err := q.QueryRow(ctx, `
		SELECT COALESCE(sum(CASE direction WHEN 'credit' THEN amount ELSE -amount END), 0)::bigint
		FROM ledger_entries
		WHERE account_id = $1`, pgUUID(accountID)).Scan(&balance)
	if err != nil {
		return 0, fmt.Errorf("ledger: balance: %w", err)
	}
	return balance, nil
}

// pgUUID converts an id to pgx's own UUID type at the SQL boundary, so the
// exported API speaks google/uuid without registering a codec with pgx.
func pgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}
