package store

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrRetryLimit is wrapped into the error Serializable returns when every
// attempt failed with a serialization failure or deadlock. Callers can map it
// to "try again later" rather than to an internal error.
var ErrRetryLimit = errors.New("store: serializable transaction retry limit reached")

// PostgreSQL SQLSTATEs that mean "this transaction lost a race; running it
// again from the start may succeed". Nothing else is retried.
const (
	sqlstateSerializationFailure = "40001"
	sqlstateDeadlockDetected     = "40P01"
)

// retryPolicy bounds Serializable's retries. It is an unexported value rather
// than constants so the bounds sit together and an in-package test could pass
// a shorter policy to serializable; the exported function always uses
// defaultRetry.
type retryPolicy struct {
	attempts   int           // total attempts, including the first
	minBackoff time.Duration // shortest wait between attempts
	maxBackoff time.Duration // longest wait between attempts
}

// SPEC-002 subtask 5a: 5 attempts, jittered 10–200 ms backoff. Under the
// gateway's contention (two payments touching the same account at once) one
// retry almost always succeeds; 5 attempts with at most ~0.5 s of waiting in
// total is a ceiling, not the expected cost.
var defaultRetry = retryPolicy{attempts: 5, minBackoff: 10 * time.Millisecond, maxBackoff: 200 * time.Millisecond}

// Serializable runs fn inside a SERIALIZABLE transaction on pool and commits
// it. If fn or the commit fails with a serialization failure (40001) or a
// deadlock (40P01), the transaction is rolled back and fn runs again in a new
// one, up to 5 attempts in total with a jittered 10–200 ms wait between them.
// Any other error is returned at once, after rolling back.
//
// fn may therefore run more than once: it must do all its work through the tx
// it is given and have no other side effects (no HTTP calls, no Redis writes).
//
// The retry has to live here, around the whole transaction, rather than in
// ledger.Post: once PostgreSQL reports 40001 the transaction is aborted, and
// the only correct recovery is to start again from BEGIN.
func Serializable(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	return serializable(ctx, pool, defaultRetry, fn)
}

func serializable(ctx context.Context, pool *pgxpool.Pool, p retryPolicy, fn func(pgx.Tx) error) error {
	opts := pgx.TxOptions{IsoLevel: pgx.Serializable}
	var lastErr error
	for attempt := 1; attempt <= p.attempts; attempt++ {
		if attempt > 1 {
			// Wait before retrying, but give up as soon as the caller does.
			t := time.NewTimer(p.backoff(attempt - 1))
			select {
			case <-ctx.Done():
				t.Stop()
				return fmt.Errorf("store: serializable transaction abandoned after %d attempts: %w (last error: %w)", attempt-1, ctx.Err(), lastErr)
			case <-t.C:
			}
		}
		// BeginTxFunc rolls back if fn returns an error and otherwise
		// commits, returning the commit's error (where SSI usually reports
		// 40001).
		err := pgx.BeginTxFunc(ctx, pool, opts, fn)
		if err == nil {
			return nil
		}
		if !retryable(err) {
			return err
		}
		lastErr = err
	}
	return fmt.Errorf("store: %d attempts: %w: %w", p.attempts, ErrRetryLimit, lastErr)
}

// retryable reports whether err carries a PostgreSQL error that a fresh
// transaction may not hit again.
func retryable(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == sqlstateSerializationFailure || pgErr.Code == sqlstateDeadlockDetected
}

// backoff returns the wait before retry number n (1 for the first retry).
// The upper bound doubles from 2×minBackoff with each retry, capped at
// maxBackoff, and the wait is drawn uniformly between minBackoff and that
// bound. The randomness ("jitter") stops two transactions that just collided
// from retrying in lockstep and colliding again.
func (p retryPolicy) backoff(n int) time.Duration {
	upper := p.maxBackoff
	if shift := min(n, 30); p.minBackoff<<shift < upper {
		upper = p.minBackoff << shift
	}
	if upper <= p.minBackoff {
		return p.minBackoff
	}
	// Jitter only needs to be unpredictable between two goroutines, not
	// cryptographically, so math/rand/v2 is the right tool.
	return p.minBackoff + rand.N(upper-p.minBackoff+1) //nolint:gosec // G404: jitter, not security
}
