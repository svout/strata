package payments

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// All of this package's SQL (ADR-117). It touches the payments table only.

// querier is what these functions need; *pgxpool.Pool and pgx.Tx both
// satisfy it, so settlement (subtask 10) can run them inside
// store.Serializable.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

const columns = `id, state, amount, currency, payer_account_id, payee_account_id,
	mandate_id, rail, rail_ref, failure_reason, created_at, updated_at`

// sqlStateFKViolation is PostgreSQL's foreign_key_violation. On insert, the
// only foreign keys are the two account ids. P2's migration adds one on
// mandate_id; this mapping must then tell them apart (pgErr.ConstraintName).
const sqlStateFKViolation = "23503"

func insertPayment(ctx context.Context, q querier, in CreateInput, railName string) (Payment, error) {
	p, err := scanPayment(q.QueryRow(ctx, `
		INSERT INTO payments (payer_account_id, payee_account_id, amount, currency, mandate_id, rail)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING `+columns,
		pgUUID(in.PayerAccountID), pgUUID(in.PayeeAccountID), in.Amount, in.Currency,
		pgNullUUID(in.MandateID), railName))
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == sqlStateFKViolation {
		return Payment{}, ErrAccountNotFound
	}
	if err != nil {
		return Payment{}, fmt.Errorf("payments: insert: %w", err)
	}
	return p, nil
}

func selectPayment(ctx context.Context, q querier, id uuid.UUID) (Payment, error) {
	p, err := scanPayment(q.QueryRow(ctx, `SELECT `+columns+` FROM payments WHERE id = $1`, pgUUID(id)))
	if errors.Is(err, pgx.ErrNoRows) {
		return Payment{}, ErrNotFound
	}
	if err != nil {
		return Payment{}, fmt.Errorf("payments: select: %w", err)
	}
	return p, nil
}

// transition moves payment id to state to, if its current state is one that
// may lead there (see from), and sets updated_at; railRef, when non-nil,
// replaces rail_ref, and reason is written as failure_reason (nil unless to
// is Failed; the schema's CHECK pairs the two). If no row matched, a second
// read tells "no such payment" from "wrong state".
func transition(ctx context.Context, q querier, id uuid.UUID, to State, railRef *string, reason *FailureReason) (Payment, error) {
	allowed := make([]string, len(from[to]))
	for i, s := range from[to] {
		allowed[i] = string(s)
	}
	p, err := scanPayment(q.QueryRow(ctx, `
		UPDATE payments
		SET state = $2, updated_at = now(), rail_ref = COALESCE($4, rail_ref), failure_reason = $5
		WHERE id = $1 AND state = ANY($3)
		RETURNING `+columns,
		pgUUID(id), string(to), allowed, railRef, (*string)(reason)))
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err := selectPayment(ctx, q, id); err != nil {
			return Payment{}, err
		}
		return Payment{}, ErrInvalidTransition
	}
	if err != nil {
		return Payment{}, fmt.Errorf("payments: update state: %w", err)
	}
	return p, nil
}

// settlingCursor is where a Recover pass has got to: the last payment it
// read. The zero value starts from the beginning.
type settlingCursor struct {
	updatedAt time.Time
	id        uuid.UUID
}

// listSettlingSQL is the Recover backlog query, served entirely by the
// partial index payments_settling_idx (rail, updated_at, id) WHERE state =
// 'settling' (migration 00004): rail is the equality prefix, the age cutoff
// and the keyset are ranges on the next two columns, and the ORDER BY is the
// index order. A settling payment's updated_at is the moment it became
// settling and can't change while it stays settling (any transition takes it
// out of the index), so the key is stable for as long as the row is listed.
const listSettlingSQL = `
	SELECT ` + columns + ` FROM payments
	WHERE state = 'settling' AND rail = $1
	  AND updated_at < now() - make_interval(secs => $2)
	  AND (updated_at, id) > ($3, $4)
	ORDER BY updated_at, id LIMIT $5`

// listSettling returns up to limit payments on railName that have been
// settling for longer than grace, after cursor, oldest first: a keyset page,
// so recovery walks the backlog without OFFSET and without holding a
// transaction open. The grace period leaves a payment whose first settle may
// still be running to that settle, instead of calling the rail twice. The
// cutoff is re-read from the clock on each page, and a payment that enters
// the backlog behind the cursor during a pass waits for the next pass.
func listSettling(ctx context.Context, q querier, railName string, grace time.Duration, after settlingCursor, limit int) ([]Payment, error) {
	rows, err := q.Query(ctx, listSettlingSQL,
		railName, grace.Seconds(), after.updatedAt, pgUUID(after.id), limit)
	if err != nil {
		return nil, fmt.Errorf("payments: list settling: %w", err)
	}
	ps, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Payment, error) { return scanPayment(row) })
	if err != nil {
		return nil, fmt.Errorf("payments: list settling: %w", err)
	}
	return ps, nil
}

func scanPayment(row pgx.Row) (Payment, error) {
	var (
		p                   Payment
		id, payer, payee, m pgtype.UUID
		state               string
		reason              *string
	)
	err := row.Scan(&id, &state, &p.Amount, &p.Currency, &payer, &payee,
		&m, &p.Rail, &p.RailRef, &reason, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return Payment{}, err
	}
	p.ID, p.PayerAccountID, p.PayeeAccountID = uuid.UUID(id.Bytes), uuid.UUID(payer.Bytes), uuid.UUID(payee.Bytes)
	p.State = State(state)
	if reason != nil {
		r := FailureReason(*reason)
		p.FailureReason = &r
	}
	// pgx scans timestamptz in time.Local; UTC keeps the wire format stable.
	p.CreatedAt, p.UpdatedAt = p.CreatedAt.UTC(), p.UpdatedAt.UTC()
	if m.Valid {
		mandate := uuid.UUID(m.Bytes)
		p.MandateID = &mandate
	}
	return p, nil
}

// pgUUID converts an id to pgx's UUID type at the SQL boundary (ADR-117), as
// ledger does; kept per package rather than shared, being two lines.
func pgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

func pgNullUUID(id *uuid.UUID) pgtype.UUID {
	if id == nil {
		return pgtype.UUID{}
	}
	return pgUUID(*id)
}
