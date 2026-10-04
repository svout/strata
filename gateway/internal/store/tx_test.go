package store_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/svout/agentgate/gateway/internal/store"
	"github.com/svout/agentgate/gateway/internal/testutil"
)

// maxAttempts mirrors store's unexported retry policy (SPEC-002 subtask 5a).
const maxAttempts = 5

func TestSerializableCommitsInSerializableTx(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	ctx := testContext(t)

	var isolation string
	err := store.Serializable(ctx, pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, "SELECT current_setting('transaction_isolation')").Scan(&isolation); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "INSERT INTO accounts (kind, currency) VALUES ('clearing', 'GBP')")
		return err
	})
	if err != nil {
		t.Fatalf("Serializable: %v", err)
	}
	if isolation != "serializable" {
		t.Errorf("isolation = %q, want serializable", isolation)
	}
	if n := countAccounts(ctx, t, pool); n != 1 {
		t.Errorf("accounts = %d after commit, want 1", n)
	}
}

// Two transactions each read the set of clearing accounts and then add one:
// classic write skew. Under SERIALIZABLE, PostgreSQL must abort one of them
// with 40001; Serializable should rerun it, so both end up committed.
func TestSerializableRetriesSerializationFailure(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	ctx := testContext(t)

	const workers = 2
	var (
		bothRead  sync.WaitGroup
		attempts  [workers]atomic.Int32
		results   [workers]error
		finished  sync.WaitGroup
		firstPass = make(chan struct{})
	)
	bothRead.Add(workers)
	go func() { bothRead.Wait(); close(firstPass) }()

	for w := range workers {
		finished.Go(func() {
			results[w] = store.Serializable(ctx, pool, func(tx pgx.Tx) error {
				n := attempts[w].Add(1)
				var count int
				if err := tx.QueryRow(ctx, "SELECT count(*) FROM accounts WHERE kind = 'clearing'").Scan(&count); err != nil {
					return err
				}
				if n == 1 {
					// Only the first attempt waits, so both transactions have
					// read before either writes. A retry runs on its own.
					bothRead.Done()
					select {
					case <-firstPass:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				_, err := tx.Exec(ctx, "INSERT INTO accounts (kind, currency) VALUES ('clearing', 'GBP')")
				return err
			})
		})
	}
	finished.Wait()

	for w := range workers {
		if results[w] != nil {
			t.Errorf("worker %d: %v", w, results[w])
		}
	}
	total := attempts[0].Load() + attempts[1].Load()
	if total < workers+1 {
		t.Errorf("total attempts = %d, want at least %d (one retry)", total, workers+1)
	}
	if n := countAccounts(ctx, t, pool); n != workers {
		t.Errorf("accounts = %d, want %d", n, workers)
	}
}

func TestSerializableStopsAtRetryLimit(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)

	for _, code := range []string{"40001", "40P01"} {
		t.Run(code, func(t *testing.T) {
			t.Parallel()
			ctx := testContext(t)
			var attempts int
			err := store.Serializable(ctx, pool, func(pgx.Tx) error {
				attempts++
				return &pgconn.PgError{Code: code}
			})
			if attempts != maxAttempts {
				t.Errorf("attempts = %d, want %d", attempts, maxAttempts)
			}
			if !errors.Is(err, store.ErrRetryLimit) {
				t.Errorf("err = %v, want ErrRetryLimit", err)
			}
			if !isCode(err, code) {
				t.Errorf("err = %v, want it to wrap SQLSTATE %s", err, code)
			}
		})
	}
}

func TestSerializableStopsOnCancelledContext(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	ctx, cancel := context.WithCancel(testContext(t))

	var attempts int
	start := time.Now()
	err := store.Serializable(ctx, pool, func(pgx.Tx) error {
		attempts++
		cancel() // the caller gives up while this attempt is failing
		return &pgconn.PgError{Code: "40001"}
	})
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if errors.Is(err, store.ErrRetryLimit) {
		t.Errorf("err = %v, should not report the retry limit", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %v to notice cancellation", d)
	}
}

func TestSerializableReturnsOtherErrorsAtOnceAndRollsBack(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	boom := errors.New("boom")

	for name, fnErr := range map[string]error{
		"plain error":      boom,
		"unique violation": &pgconn.PgError{Code: "23505"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := testContext(t)
			var attempts int
			err := store.Serializable(ctx, pool, func(tx pgx.Tx) error {
				attempts++
				if _, err := tx.Exec(ctx, "INSERT INTO accounts (kind, currency) VALUES ('clearing', 'GBP')"); err != nil {
					return err
				}
				return fnErr
			})
			if attempts != 1 {
				t.Errorf("attempts = %d, want 1", attempts)
			}
			if !errors.Is(err, fnErr) {
				t.Errorf("err = %v, want %v", err, fnErr)
			}
			if errors.Is(err, store.ErrRetryLimit) {
				t.Errorf("err = %v, should not report the retry limit", err)
			}
			if n := countAccounts(ctx, t, pool); n != 0 {
				t.Errorf("accounts = %d after failed fn, want 0 (rolled back)", n)
			}
		})
	}
}

func countAccounts(ctx context.Context, t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM accounts").Scan(&n); err != nil {
		t.Fatalf("count accounts: %v", err)
	}
	return n
}

func isCode(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}
