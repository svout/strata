// Package payments owns the payments table: it creates payments, reads them
// and moves them forward through their lifecycle
// (created -> settling -> settled | failed).
//
// It is the data layer for that table only (ADR-117): all of its SQL is in
// store.go, it never reads or writes another package's tables (an unknown
// account surfaces as the foreign-key violation), and its exported API uses
// plain types, with the pool handle in New as the only driver type.
//
// Transitions are forward-only. Each is a single conditional UPDATE that
// names the states it may leave, so two racing transitions on one payment
// can't both win: the loser's UPDATE re-checks the row after the winner
// commits, matches nothing and gets ErrInvalidTransition.
package payments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/svout/strata/gateway/internal/idem"
	"github.com/svout/strata/gateway/internal/store"
)

// State is a payment's lifecycle state, as stored in payments.state.
type State string

const (
	Created  State = "created"
	Settling State = "settling"
	Settled  State = "settled"
	Failed   State = "failed"
)

// RailInternal is the only rail in P1 (SPEC-002 subtask 7); the service sets
// it, callers can't choose it.
const RailInternal = "internal"

// Currency is the only currency in P1; the schema's CHECK enforces it too.
const Currency = "GBP"

// Payment is one row of the payments table. Amount is in minor units (pence).
// MandateID and RailRef are nil until set.
type Payment struct {
	ID             uuid.UUID  `json:"id"`
	State          State      `json:"state"`
	Amount         int64      `json:"amount"`
	Currency       string     `json:"currency"`
	PayerAccountID uuid.UUID  `json:"payer_account_id"`
	PayeeAccountID uuid.UUID  `json:"payee_account_id"`
	MandateID      *uuid.UUID `json:"mandate_id"`
	Rail           string     `json:"rail"`
	RailRef        *string    `json:"rail_ref"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// CreateInput is what a caller supplies for a new payment.
type CreateInput struct {
	Amount         int64
	Currency       string
	PayerAccountID uuid.UUID
	PayeeAccountID uuid.UUID
	MandateID      *uuid.UUID
}

// Errors returned for requests that are wrong in themselves; anything else is
// a database failure. Messages are fixed: none quotes the input.
var (
	ErrNotFound            = errors.New("payments: payment not found")
	ErrInvalidTransition   = errors.New("payments: transition not allowed from the current state")
	ErrInvalidAmount       = errors.New("payments: amount must be greater than zero")
	ErrUnsupportedCurrency = errors.New("payments: currency must be GBP")
	ErrMissingAccount      = errors.New("payments: payer or payee account id is missing")
	ErrSameAccount         = errors.New("payments: payer and payee are the same account")
	ErrAccountNotFound     = errors.New("payments: payer or payee account does not exist")
	ErrMissingRailRef      = errors.New("payments: rail reference is empty")
)

// Service creates, reads and transitions payments.
type Service struct {
	pool   *pgxpool.Pool
	locker idem.Locker // nil: no in-flight lock, PostgreSQL alone
}

// Option configures a Service.
type Option func(*Service)

// WithLocker makes concurrent CreateIdempotent calls with the same key wait
// for each other (SPEC-002 8b). Correctness doesn't depend on it (ADR-118).
func WithLocker(l idem.Locker) Option {
	return func(s *Service) { s.locker = l }
}

// New returns a Service on pool.
func New(pool *pgxpool.Pool, opts ...Option) *Service {
	s := &Service{pool: pool}
	for _, o := range opts {
		o(s)
	}
	return s
}

// create validates in and inserts a payment in state created on the internal
// rail. It posts nothing to the ledger; settlement does (SPEC-002 subtask 10).
// It does not use an Idempotency-Key; the API uses CreateIdempotent, and
// this stays unexported for the package's own tests.
func (s *Service) create(ctx context.Context, in CreateInput) (Payment, error) {
	if err := validate(in); err != nil {
		return Payment{}, err
	}
	return insertPayment(ctx, s.pool, in)
}

// CreateResult is the answer to an idempotent create: the payment's id and the
// exact response body, which is byte-identical on every replay.
type CreateResult struct {
	PaymentID uuid.UUID
	Status    int
	Body      []byte // JSON, without a trailing newline
	Replayed  bool
}

// CreateIdempotent is Create guarded by an Idempotency-Key (ADR-118). In one
// SERIALIZABLE transaction it claims key for the request hash, inserts the
// payment and stores the 201 response; a retry with the same key and hash
// gets that stored response back (Replayed) and creates nothing. A different
// hash is idem.ErrMismatch. Invalid input is rejected before the key is
// touched, and any failure rolls the claim back, so only a 201 is ever
// stored. With a Locker, a concurrent duplicate waits for the first request's
// transaction instead of racing it; the lock is held around the transaction,
// never taken inside it, and a wait that outlasts ctx returns ctx's error.
func (s *Service) CreateIdempotent(ctx context.Context, key idem.Key, hash [32]byte, in CreateInput) (CreateResult, error) {
	if err := validate(in); err != nil {
		return CreateResult{}, err
	}
	if s.locker != nil {
		release, err := s.locker.Lock(ctx, key)
		if err != nil {
			return CreateResult{}, err
		}
		defer release()
	}
	var out CreateResult
	err := store.Serializable(ctx, s.pool, func(tx pgx.Tx) error {
		out = CreateResult{} // the function may run more than once
		stored, found, err := idem.Claim(ctx, tx, key, hash)
		if err != nil {
			return err
		}
		if found {
			// The stored body is a Payment as encoded when the key was
			// claimed; its "id" member must stay stable for Retention (24 h)
			// across releases, or replays of older keys fail.
			var p struct {
				ID uuid.UUID `json:"id"`
			}
			if err := json.Unmarshal(stored.Body, &p); err != nil {
				return fmt.Errorf("payments: stored response: %w", err)
			}
			out = CreateResult{PaymentID: p.ID, Status: stored.Status, Body: stored.Body, Replayed: true}
			return nil
		}
		p, err := insertPayment(ctx, tx, in)
		if err != nil {
			return err
		}
		body, err := json.Marshal(p)
		if err != nil {
			return fmt.Errorf("payments: encode response: %w", err)
		}
		out = CreateResult{PaymentID: p.ID, Status: http.StatusCreated, Body: body}
		return idem.Complete(ctx, tx, key, hash, idem.Response{Status: out.Status, Body: body})
	})
	if err != nil {
		return CreateResult{}, err
	}
	return out, nil
}

func validate(in CreateInput) error {
	// A missing account is a malformed request (400), so it is checked
	// before the business rules (422).
	switch {
	case in.PayerAccountID == uuid.Nil || in.PayeeAccountID == uuid.Nil:
		return ErrMissingAccount
	case in.Amount <= 0:
		return ErrInvalidAmount
	case in.Currency != Currency:
		return ErrUnsupportedCurrency
	case in.PayerAccountID == in.PayeeAccountID:
		return ErrSameAccount
	}
	return nil
}

// Get returns the payment with id, or ErrNotFound.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (Payment, error) {
	return selectPayment(ctx, s.pool, id)
}

// MarkSettling moves a created payment to settling and records the rail's
// reference, written before the rail is called so crash recovery can find it.
func (s *Service) MarkSettling(ctx context.Context, id uuid.UUID, railRef string) (Payment, error) {
	if railRef == "" {
		return Payment{}, ErrMissingRailRef
	}
	return transition(ctx, s.pool, id, Settling, &railRef)
}

// MarkSettled moves a settling payment to settled.
func (s *Service) MarkSettled(ctx context.Context, id uuid.UUID) (Payment, error) {
	return transition(ctx, s.pool, id, Settled, nil)
}

// MarkFailed moves a created or settling payment to failed.
func (s *Service) MarkFailed(ctx context.Context, id uuid.UUID) (Payment, error) {
	return transition(ctx, s.pool, id, Failed, nil)
}

// from lists the states each target may be reached from. Every edge moves
// forward; settled and failed are terminal. A same-state call is not an edge,
// so it fails with ErrInvalidTransition (subtask 10 decides how to treat it).
var from = map[State][]State{
	Settling: {Created},
	Settled:  {Settling},
	Failed:   {Created, Settling},
}
