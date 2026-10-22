package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/svout/strata/gateway/internal/httpx"
	"github.com/svout/strata/gateway/internal/ledger"
	"github.com/svout/strata/gateway/internal/payments"
	"github.com/svout/strata/gateway/internal/rail"
	"github.com/svout/strata/gateway/internal/testutil"
)

// fastTimings retries every few milliseconds, so a test sees several
// attempts in well under a second.
var fastTimings = timings{
	lookupRetry:    backoff{first: 5 * time.Millisecond, max: 20 * time.Millisecond},
	lookupTimeout:  2 * time.Second,
	recoverEvery:   10 * time.Millisecond,
	recoverRetry:   backoff{first: 5 * time.Millisecond, max: 20 * time.Millisecond},
	recoverTimeout: 2 * time.Second,
}

// syncBuffer is a log sink that background goroutines may write to while
// the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func testLogger() (*slog.Logger, *syncBuffer) {
	var buf syncBuffer
	return httpx.NewLogger(&buf, slog.LevelDebug), &buf
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// atSchemaV4 makes a migrated test database look like one migrated to 4
// only, as far as startup can tell: no clearing accounts, no version 5 row.
func atSchemaV4(t *testing.T, pool *pgxpool.Pool) {
	exec(t, pool, "DELETE FROM accounts WHERE kind = 'clearing'")
	exec(t, pool, "DELETE FROM goose_db_version WHERE version_id = 5")
}

// migrateToV5 is `make migrate` for a database atSchemaV4 left behind. The
// clearing accounts are inserted by hand, not with testutil.NewAccount: they
// need the exact owner 00005 seeds ('rail:<name>'), which the factory
// doesn't set.
func migrateToV5(t *testing.T, pool *pgxpool.Pool) {
	exec(t, pool, "INSERT INTO accounts (owner, kind, currency) VALUES ('rail:internal', 'clearing', 'GBP'), ('rail:fake', 'clearing', 'GBP')")
	exec(t, pool, "INSERT INTO goose_db_version (version_id, is_applied) VALUES (5, true)")
}

func resolve(ctx context.Context, t *testing.T, pool *pgxpool.Pool, log *slog.Logger) (ledger.ClearingAccountID, error) {
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	return resolveClearing(ctx, pool, db, rail.InternalName, log, fastTimings.lookupRetry, fastTimings.lookupTimeout)
}

func TestResolveClearing_Found(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	want, err := ledger.ClearingAccount(t.Context(), pool, rail.InternalName)
	if err != nil {
		t.Fatal(err)
	}
	log, _ := testLogger()
	if got, err := resolve(t.Context(), t, pool, log); err != nil || got != want {
		t.Fatalf("resolveClearing = %v, %v; want %v", got, err, want)
	}
}

// Schema version 4: the database answers that there is no clearing account,
// but 00005 hasn't run. That is "not migrated yet": keep retrying, with a
// line naming `make migrate`, never exit (ADR-125 §3 as amended). Once the
// migration lands, the next attempt finds the account.
func TestResolveClearing_SchemaV4RetriesUntilMigrated(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	atSchemaV4(t, pool)
	log, logs := testLogger()

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if _, err := resolve(ctx, t, pool, log); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("resolveClearing at version 4 = %v, want it still retrying at the deadline", err)
	}
	if n := strings.Count(logs.String(), "make migrate"); n < 2 {
		t.Errorf("%d retry lines naming make migrate, want at least 2; logs:\n%s", n, logs)
	}
	if !strings.Contains(logs.String(), "clearing accounts not seeded yet") {
		t.Errorf("retry line doesn't say why; logs:\n%s", logs)
	}

	migrateToV5(t, pool)
	if _, err := resolve(t.Context(), t, pool, log); err != nil {
		t.Fatalf("resolveClearing after migrating = %v, want the id", err)
	}
}

// 00005 committing between the two reads must not make a migrated gateway
// exit (review should 1). lookupClearing reads the version first, so a 4
// followed by a found account is a success. Read the other way round, this
// interleaving is "no account" then "version 5": a wrong configuration
// error. Not parallel: it sets a package-level hook, and parallel tests
// only start once the sequential ones have finished.
func TestResolveClearing_MigrationBetweenReadsDoesNotExit(t *testing.T) {
	pool := testutil.Postgres(t)
	atSchemaV4(t, pool)
	var once sync.Once
	testHookBetweenReads = func() { once.Do(func() { migrateToV5(t, pool) }) }
	t.Cleanup(func() { testHookBetweenReads = nil })
	log, _ := testLogger()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, err := resolve(ctx, t, pool, log); err != nil {
		t.Fatalf("resolveClearing with 00005 landing between the reads = %v, want the id", err)
	}
}

// Schema version 5 with the seed row deleted: the database is migrated and
// misconfigured, so startup exits at once with an error naming the rail.
func TestResolveClearing_SchemaV5MissingSeedExits(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	exec(t, pool, "DELETE FROM accounts WHERE owner = 'rail:internal'")
	log, logs := testLogger()

	// A deadline, so a regression that retries fails here instead of hanging.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, err := resolve(ctx, t, pool, log)
	if !errors.Is(err, errConfig) || !errors.Is(err, ledger.ErrNoClearingAccount) {
		t.Fatalf("resolveClearing = %v, want errConfig wrapping ErrNoClearingAccount", err)
	}
	for _, s := range []string{`"internal"`, "schema version 5"} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error %q doesn't contain %s", err, s)
		}
	}
	if strings.Contains(logs.String(), "retrying") {
		t.Errorf("a configuration error was retried; logs:\n%s", logs)
	}
}

// An unreachable database is transient: retry, never exit. Nothing from the
// DSN reaches the log (AC8): the lookup is the first real connection, and
// the driver's error would name host, user and database.
func TestResolveClearing_UnreachableRetriesWithoutLeakingDSN(t *testing.T) {
	t.Parallel()
	pool, err := pgxpool.New(t.Context(), "postgres://leakuser:leakpass@127.0.0.1:1/leakdb?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	log, logs := testLogger()

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if _, err := resolve(ctx, t, pool, log); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("resolveClearing on an unreachable database = %v, want it still retrying at the deadline", err)
	}
	if !strings.Contains(logs.String(), "database unreachable or not migrated") {
		t.Errorf("no retry line giving the read-failure reason; logs:\n%s", logs)
	}
	for _, s := range []string{"leakuser", "leakpass", "leakdb", "127.0.0.1"} {
		if strings.Contains(logs.String(), s) {
			t.Errorf("log contains %q:\n%s", s, logs)
		}
	}
}

// A reachable database that was never migrated (a fresh `make dev` stack):
// no version table, so version 0, and no accounts table, so the account read
// fails with ErrClearingLookup rather than "no account". Still transient:
// retry with the read-failure reason, never exit.
func TestResolveClearing_NeverMigratedRetries(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	exec(t, pool, "DROP TABLE goose_db_version")
	exec(t, pool, "DROP TABLE accounts CASCADE")
	log, logs := testLogger()

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if _, err := resolve(ctx, t, pool, log); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("resolveClearing on an unmigrated database = %v, want it still retrying at the deadline", err)
	}
	if n := strings.Count(logs.String(), "database unreachable or not migrated"); n < 2 {
		t.Errorf("%d retry lines giving the read-failure reason, want at least 2; logs:\n%s", n, logs)
	}
}

func TestCheckClearing_RejectsZero(t *testing.T) {
	t.Parallel()
	err := checkClearing(rail.FakeName, ledger.ClearingAccountID{})
	if !errors.Is(err, errConfig) || !strings.Contains(err.Error(), `"fake"`) {
		t.Fatalf("checkClearing(zero) = %v, want errConfig naming the rail", err)
	}
	if err := checkClearing(rail.FakeName, ledger.ClearingAccountID{1}); err != nil {
		t.Fatalf("checkClearing(non-zero) = %v", err)
	}
}

func TestRailName(t *testing.T) {
	t.Parallel()
	if got := railName(false); got != rail.InternalName {
		t.Errorf("railName(false) = %q", got)
	}
	if got := railName(true); got != rail.FakeName {
		t.Errorf("railName(true) = %q", got)
	}
}

// scriptedRecoverer fails its first `failures` passes, then succeeds.
type scriptedRecoverer struct {
	failures int32
	calls    atomic.Int32
}

func (r *scriptedRecoverer) Recover(context.Context) (payments.RecoverStats, error) {
	if r.calls.Add(1) <= r.failures {
		return payments.RecoverStats{}, errors.New("backlog read failed")
	}
	return payments.RecoverStats{}, nil
}

// The loop keeps going after failed passes, runs on a timer after good
// ones, and stops when its context ends.
func TestRecoverLoop_RetriesAndStops(t *testing.T) {
	t.Parallel()
	r := &scriptedRecoverer{failures: 2}
	log, logs := testLogger()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		recoverLoop(ctx, r, log, fastTimings.recoverEvery, fastTimings.recoverRetry, fastTimings.recoverTimeout)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for r.calls.Load() < 5 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := r.calls.Load(); n < 5 {
		t.Fatalf("%d Recover passes in 2 s, want at least 5 (two failures, then periodic)", n)
	}
	if n := strings.Count(logs.String(), "settlement recovery pass failed"); n != 2 {
		t.Errorf("%d failure lines, want 2; logs:\n%s", n, logs)
	}
	if strings.Contains(logs.String(), "backlog read failed") {
		t.Errorf("the error text was logged; logs:\n%s", logs)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("recoverLoop didn't stop after cancel")
	}
}

func TestBackoff_DoublesToCapAndResets(t *testing.T) {
	t.Parallel()
	b := backoff{first: time.Second, max: 5 * time.Second}
	var got []time.Duration
	for range 5 {
		got = append(got, b.next())
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("delays = %v, want %v", got, want)
		}
	}
	b.reset()
	if d := b.next(); d != time.Second {
		t.Fatalf("after reset = %v, want 1s", d)
	}
}

// AC7, end to end through the router: on a database at version 4 the
// gateway serves at once, /readyz answers not ready (the wiring doesn't
// block it) and payments get 503 try_again_later. After `make migrate`,
// the rail is wired, /readyz is ready and a payment settles through
// the internal rail. Cancelling stops the background work.
func TestWireSettlement_NotReadyUntilMigratedThenSettles(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	atSchemaV4(t, pool)
	log, logs := testLogger()

	g := &gate{}
	router := httpx.NewRouter(log, g.readyCheck())
	router.Mount("/v1/payments", g)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- wireSettlement(ctx, pool, false, nil, log, g, fastTimings) }()

	if code := serve(router, http.MethodGet, "/readyz", "").Code; code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz before migrating = %d, want 503", code)
	}
	rec := serve(router, http.MethodPost, "/v1/payments", `{}`)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "1" || !strings.Contains(rec.Body.String(), "try_again_later") {
		t.Fatalf("POST before wiring = %d %q (Retry-After %q), want 503 try_again_later", rec.Code, rec.Body, rec.Header().Get("Retry-After"))
	}

	migrateToV5(t, pool)
	waitFor(t, g.ready)
	// /readyz caches a result for up to a second.
	waitFor(t, func() bool { return serve(router, http.MethodGet, "/readyz", "").Code == http.StatusOK })

	payer := testutil.NewAccount(t, pool, "user")
	payee := testutil.NewAccount(t, pool, "merchant")
	body := `{"amount":1250,"currency":"GBP","payer_account_id":"` + payer.String() + `","payee_account_id":"` + payee.String() + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/payments", strings.NewReader(body))
	req.Header.Set("Idempotency-Key", "wire-test-1")
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST after wiring = %d %s, want 201", rec.Code, rec.Body)
	}
	var state string
	waitFor(t, func() bool {
		err := pool.QueryRow(t.Context(), "SELECT state FROM payments").Scan(&state)
		return err == nil && state == "settled"
	})
	if !strings.Contains(logs.String(), `"rail wired"`) {
		t.Errorf("no rail wired line; logs:\n%s", logs)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("wireSettlement after cancel = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wireSettlement didn't stop after cancel")
	}
}

// A configuration error comes back to run, which exits with it; the gate
// stays closed, and the reason is in the JSON log, not only on stderr.
func TestWireSettlement_ConfigErrorReturns(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	exec(t, pool, "DELETE FROM accounts WHERE owner = 'rail:fake'")
	log, logs := testLogger()
	g := &gate{}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	err := wireSettlement(ctx, pool, true, nil, log, g, fastTimings)
	if !errors.Is(err, errConfig) {
		t.Fatalf("wireSettlement = %v, want errConfig", err)
	}
	if g.ready() {
		t.Fatal("gate opened despite the configuration error")
	}
	if !strings.Contains(logs.String(), `"level":"ERROR","msg":"rail wiring failed; the gateway will exit"`) ||
		!strings.Contains(logs.String(), `"rail":"fake"`) {
		t.Fatalf("no Error line naming the rail; logs:\n%s", logs)
	}
}

func TestGate_SetNilPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("set(nil) didn't panic")
		}
	}()
	(&gate{}).set(nil)
}

func serve(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5 s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
