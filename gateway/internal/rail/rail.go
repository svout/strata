// Package rail moves money for a payment: the Adapter interface every rail
// implements, the internal-ledger rail and a deterministic fake PSP. The
// contract every Adapter must meet is tested by package railtest (AC6) and
// recorded in ADR-120:
//
//   - Settle is idempotent on Request.Ref: the same reference always names the
//     same operation, so calling it again returns the first outcome and moves
//     no more money.
//   - Unknown is not failure. Any error from Settle (a timeout, a lost
//     response, a crash) means the outcome is unknown: the money may have
//     moved. The caller leaves the payment settling and asks Status(ref)
//     later. Only an explicit Declined result means nothing moved, and only it
//     may lead to payments.MarkFailed. Treating a timeout as a failure is how
//     a settled payment gets retried into a second one.
//   - The one exception is ErrInvalidRequest: the request is wrong in itself,
//     nothing was attempted, and it fails the same way every time. Retrying
//     it forever is a bug, not caution; the caller decides how to end it
//     (subtask 10).
//   - The reference is derived from the payment (Ref), never random per
//     attempt, so a retry hits the same rail operation and recovery needs no
//     lookup table.
package rail

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
)

// Outcome is what a rail reports for an operation.
type Outcome string

const (
	// Settled: the money moved. Final.
	Settled Outcome = "settled"
	// Pending: the rail accepted the operation but hasn't finished it
	// (asynchronous rails such as Stripe); ask Status later.
	Pending Outcome = "pending"
	// Declined: the rail refused it and nothing moved. Final.
	Declined Outcome = "declined"
)

// Result is a rail's answer for one operation. Reason is the rail's fixed
// decline code when Outcome is Declined, and empty otherwise; it never holds
// free text from the rail.
type Result struct {
	Outcome Outcome
	Reason  string
}

// Request asks a rail to move Amount minor units of Currency from Payer to
// Payee for PaymentID, as the operation named Ref.
type Request struct {
	PaymentID uuid.UUID
	Ref       string
	Amount    int64
	Currency  string
	Payer     uuid.UUID
	Payee     uuid.UUID
}

// Adapter is a payment rail. Every implementation passes railtest.Run.
type Adapter interface {
	// Name is the rail's name as stored in payments.rail.
	Name() string
	// Settle performs the operation req.Ref, or returns its earlier outcome
	// if that reference was already settled. A non-nil error other than
	// ErrInvalidRequest means the outcome is unknown, not that it failed
	// (see the package comment).
	Settle(ctx context.Context, req Request) (Result, error)
	// Status returns the current outcome of the operation ref, or
	// ErrUnknownRef if the rail has no record of it (it never took effect,
	// so Settle with the same reference is safe).
	Status(ctx context.Context, ref string) (Result, error)
}

var (
	// ErrUnknownRef means the rail has no operation with that reference.
	ErrUnknownRef = errors.New("rail: unknown reference")
	// ErrInvalidRequest means the request is wrong in itself; nothing was
	// attempted, and repeating it gives the same error. It is the only Settle
	// error that is not an unknown outcome. Messages are fixed and never
	// quote the input.
	ErrInvalidRequest = errors.New("rail: invalid request")
)

// Ref is the rail reference for settling paymentID: deterministic, so every
// attempt and every recovery for the payment names the same operation.
func Ref(paymentID uuid.UUID) string {
	return refPrefix + paymentID.String()
}

const refPrefix = "pay_"

// ErrInvalidRef means a string is not a reference Ref could have made. The
// message is fixed and never quotes the input: ParseRef reads text from
// unauthenticated webhooks (ADR-124), and quoting it would let a caller
// write into the log.
var ErrInvalidRef = errors.New("rail: reference is not pay_<uuid>")

// ParseRef is the inverse of Ref: the payment id in s, or ErrInvalidRef.
// It does no I/O, so a webhook can reject a malformed reference before
// touching the database or the rail (ADR-124).
//
// It is strict: s must be exactly what Ref returns for its id. uuid.Parse
// alone also accepts upper case, braces and urn:uuid: forms, which would
// give one payment several spellings. The exact length rules out braces
// and urn (and oversized input, at no cost); the round trip rules out the
// rest, such as upper case.
func ParseRef(s string) (uuid.UUID, error) {
	if len(s) != len(refPrefix)+36 || !strings.HasPrefix(s, refPrefix) {
		return uuid.Nil, ErrInvalidRef
	}
	id, err := uuid.Parse(s[len(refPrefix):])
	if err != nil || id == uuid.Nil || Ref(id) != s {
		return uuid.Nil, ErrInvalidRef
	}
	return id, nil
}

// validate checks a request without I/O. Which currencies are allowed is
// the payments package's rule (P1: GBP only); a rail only needs one.
func validate(req Request) error {
	switch {
	case req.PaymentID == uuid.Nil || req.Payer == uuid.Nil || req.Payee == uuid.Nil,
		req.Ref == "" || len(req.Ref) > 255,
		req.Amount <= 0,
		len(req.Currency) != 3,
		req.Payer == req.Payee:
		return ErrInvalidRequest
	}
	return nil
}
