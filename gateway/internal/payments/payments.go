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
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/svout/strata/gateway/internal/idem"
	"github.com/svout/strata/gateway/internal/ledger"
	"github.com/svout/strata/gateway/internal/rail"
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

// RailInternal is the rail recorded when a Service has no rail (WithRail).
// The rail package owns the name; callers can't choose a rail.
const RailInternal = rail.InternalName

// FailureReason says why a payment failed, as stored in
// payments.failure_reason. It is set exactly when the state is Failed.
type FailureReason string

const (
	// FailureDeclined: the rail declined the payment. The payer's problem.
	FailureDeclined FailureReason = "declined"
	// FailureInvalidRequest: the rail rejected the request as wrong in
	// itself (rail.ErrInvalidRequest). Our bug: it is logged at Error.
	FailureInvalidRequest FailureReason = "invalid_request"
)

// Currency is the only currency in P1; the schema's CHECK enforces it too.
const Currency = "GBP"

// Payment is one row of the payments table. Amount is in minor units (pence).
// MandateID and RailRef are nil until set; FailureReason is set only when
// State is Failed.
type Payment struct {
	ID             uuid.UUID      `json:"id"`
	State          State          `json:"state"`
	Amount         int64          `json:"amount"`
	Currency       string         `json:"currency"`
	PayerAccountID uuid.UUID      `json:"payer_account_id"`
	PayeeAccountID uuid.UUID      `json:"payee_account_id"`
	MandateID      *uuid.UUID     `json:"mandate_id"`
	Rail           string         `json:"rail"`
	RailRef        *string        `json:"rail_ref"`
	FailureReason  *FailureReason `json:"failure_reason"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
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
	ErrNotFound             = errors.New("payments: payment not found")
	ErrInvalidTransition    = errors.New("payments: transition not allowed from the current state")
	ErrInvalidAmount        = errors.New("payments: amount must be greater than zero")
	ErrUnsupportedCurrency  = errors.New("payments: currency must be GBP")
	ErrMissingAccount       = errors.New("payments: payer or payee account id is missing")
	ErrSameAccount          = errors.New("payments: payer and payee are the same account")
	ErrAccountNotFound      = errors.New("payments: payer or payee account does not exist")
	ErrMissingRailRef       = errors.New("payments: rail reference is empty")
	ErrNoRail               = errors.New("payments: no rail configured")
	ErrInvalidFailureReason = errors.New("payments: failure reason must be declined or invalid_request")
)

// Service creates, reads and transitions payments.
type Service struct {
	pool   *pgxpool.Pool
	locker idem.Locker  // nil: no in-flight lock, PostgreSQL alone
	rail   rail.Adapter // nil: payments are created and never settled
	// clearing is rail's clearing account, credited by the payer_ posting
	// when a payment settles (ADR-123). Resolved by the caller at startup,
	// never read inside the settle transaction (ADR-125).
	clearing ledger.ClearingAccountID
	log      *slog.Logger

	// settleTimeout bounds one rail call. recoverGrace is how long a payment
	// must have been settling before Recover touches it, and recoverPage how
	// many it reads at a time; tests change both.
	settleTimeout time.Duration
	recoverGrace  time.Duration
	recoverPage   int

	// Rail webhooks (ADR-124, webhook.go). webhookSlots bounds concurrent
	// webhooks between the lookup and the end of Status (capacity
	// webhookBound); the two timeouts bound how long each of those holds a
	// slot; webhookShed counts requests turned away because every slot was
	// taken.
	webhookBound         int
	webhookSlots         chan struct{}
	webhookLookupTimeout time.Duration
	webhookStatusTimeout time.Duration
	webhookShed          atomic.Uint64
}

// Option configures a Service.
type Option func(*Service)

// WithLocker makes concurrent CreateIdempotent calls with the same key wait
// for each other (SPEC-002 8b). Correctness doesn't depend on it (ADR-118).
func WithLocker(l idem.Locker) Option {
	return func(s *Service) { s.locker = l }
}

// WithRail makes the Service settle every payment it creates on a (the
// outbox, ADR-118's consequence): the payment is recorded as settling with
// its rail reference in the transaction that creates it, and the rail is
// called after that commits. Without a rail, payments stay created.
//
// clearing is a's clearing account (ledger.ClearingAccount for a.Name()).
// When a payment settles, the payer is debited and clearing credited in the
// transaction that records settled (ADR-123).
func WithRail(a rail.Adapter, clearing ledger.ClearingAccountID) Option {
	return func(s *Service) { s.rail, s.clearing = a, clearing }
}

// WithLogger sets where settlement outcomes are logged (default: nowhere).
// It should be the redacting logger from httpx.NewLogger.
func WithLogger(l *slog.Logger) Option {
	return func(s *Service) { s.log = l }
}

// WithWebhookBound sets how many rail webhooks may be asking the rail's
// Status at once (default 8; ADR-124's amendment). A request beyond it is
// turned away with 503 at once, never queued. It is an option so that
// wiring can take it from configuration and an incident can change it
// without a deploy. n must be at least 1; New panics otherwise.
func WithWebhookBound(n int) Option {
	return func(s *Service) { s.webhookBound = n }
}

// New returns a Service on pool.
func New(pool *pgxpool.Pool, opts ...Option) *Service {
	s := &Service{pool: pool, log: slog.New(slog.DiscardHandler), settleTimeout: defaultSettleTimeout, recoverGrace: defaultRecoverGrace, recoverPage: defaultRecoverPage,
		webhookBound: defaultWebhookBound, webhookLookupTimeout: defaultWebhookLookupTimeout, webhookStatusTimeout: defaultWebhookStatusTimeout}
	for _, o := range opts {
		o(s)
	}
	if s.webhookBound < 1 {
		panic("payments: WithWebhookBound needs at least 1")
	}
	s.webhookSlots = make(chan struct{}, s.webhookBound)
	return s
}

// WebhookShed is the number of rail webhooks turned away with 503 because
// every Status slot was taken. Without it, a hung rail or a flood would look
// the same as quiet traffic. Like idem.RedisLocker.Degraded (ADR-119), it is
// read in-process for now; the OTel work (SPEC-002 subtask 13) exports it.
func (s *Service) WebhookShed() uint64 { return s.webhookShed.Load() }

// create validates in and inserts a payment in state created on the internal
// rail. It posts nothing to the ledger; settlement does (SPEC-002 subtask 10).
// It does not use an Idempotency-Key; the API uses CreateIdempotent, and
// this stays unexported for the package's own tests.
func (s *Service) create(ctx context.Context, in CreateInput) (Payment, error) {
	if err := validate(in); err != nil {
		return Payment{}, err
	}
	return insertPayment(ctx, s.pool, in, s.railName())
}

// railName is the name stored in payments.rail.
func (s *Service) railName() string {
	if s.rail == nil {
		return RailInternal
	}
	return s.rail.Name()
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
// transaction instead of racing it; the lock is held around the transaction
// only (never taken inside it, released before any rail call), and a wait
// that outlasts ctx returns ctx's error.
//
// With a rail (WithRail), the same transaction moves the payment to settling
// with its rail reference before the response is stored, so the 201 (and
// every replay of it, byte for byte) says "settling": a snapshot at
// creation, not the final state, which GET returns. After the commit, only
// the request that created the payment calls the rail; a replay never does,
// so a client can't drive money movement by retrying. A payment the call
// doesn't finish stays settling for Recover.
func (s *Service) CreateIdempotent(ctx context.Context, key idem.Key, hash [32]byte, in CreateInput) (CreateResult, error) {
	if err := validate(in); err != nil {
		return CreateResult{}, err
	}
	release := func() {}
	if s.locker != nil {
		unlock, err := s.locker.Lock(ctx, key)
		if err != nil {
			return CreateResult{}, err
		}
		// Released explicitly after the commit below; the defer only covers
		// a panic. OnceFunc makes the second call a no-op.
		release = sync.OnceFunc(unlock)
		defer release()
	}
	var (
		out     CreateResult
		created Payment
	)
	err := store.Serializable(ctx, s.pool, func(tx pgx.Tx) error {
		out, created = CreateResult{}, Payment{} // the function may run more than once
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
		p, err := insertPayment(ctx, tx, in, s.railName())
		if err != nil {
			return err
		}
		if s.rail != nil {
			ref := rail.Ref(p.ID)
			if p, err = transition(ctx, tx, p.ID, Settling, &ref, nil); err != nil {
				return err
			}
		}
		created = p
		body, err := json.Marshal(p)
		if err != nil {
			return fmt.Errorf("payments: encode response: %w", err)
		}
		out = CreateResult{PaymentID: p.ID, Status: http.StatusCreated, Body: body}
		return idem.Complete(ctx, tx, key, hash, idem.Response{Status: out.Status, Body: body})
	})
	// The lock is released before the rail call, never held across it. It
	// sheds load in front of the transaction, with a TTL and a millisecond
	// budget (ADR-119); held across a rail call of up to 2 x settleTimeout it
	// would make duplicates wait on the rail and could expire mid-settle.
	// Releasing early can't cause a second rail call: the key is completed
	// in the committed transaction, so a duplicate arriving now replays the
	// stored 201, and a replay never settles.
	release()
	if err != nil {
		return CreateResult{}, err
	}
	if s.rail != nil && !out.Replayed {
		s.settle(ctx, created)
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
	return transition(ctx, s.pool, id, Settling, &railRef, nil)
}

// MarkFailed moves a created or settling payment to failed, recording why.
// An unknown reason is ErrInvalidFailureReason, checked before the database,
// so it never surfaces as a raw CHECK violation naming the table.
func (s *Service) MarkFailed(ctx context.Context, id uuid.UUID, reason FailureReason) (Payment, error) {
	if reason != FailureDeclined && reason != FailureInvalidRequest {
		return Payment{}, ErrInvalidFailureReason
	}
	return transition(ctx, s.pool, id, Failed, nil, &reason)
}

// from lists the states each target may be reached from. Every edge moves
// forward; settled and failed are terminal. A same-state call is not an edge,
// so it fails with ErrInvalidTransition (subtask 10 decides how to treat it).
var from = map[State][]State{
	Settling: {Created},
	Settled:  {Settling},
	Failed:   {Created, Settling},
}
