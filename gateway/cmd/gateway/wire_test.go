package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/svout/strata/gateway/internal/config"
	"github.com/svout/strata/gateway/internal/httpx"
	"github.com/svout/strata/gateway/internal/ledger"
	"github.com/svout/strata/gateway/internal/payments"
	"github.com/svout/strata/gateway/internal/rail"
	"github.com/svout/strata/gateway/internal/store"
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

// atVersion makes a migrated test database look like one migrated to v
// only, as far as startup can tell: no version row above v (startup reads
// the highest), and below 5 no clearing accounts (00005 seeds them). The
// tables stay; TestWireSettlement_SkewNotReadyThenMigrated rolls a real
// migration back instead.
func atVersion(t *testing.T, pool *pgxpool.Pool, v int64) {
	if v < 5 {
		exec(t, pool, "DELETE FROM accounts WHERE kind = 'clearing'")
	}
	exec(t, pool, "DELETE FROM goose_db_version WHERE version_id > "+strconv.FormatInt(v, 10))
}

// migrateToLatest is `make migrate` for a database atVersion left behind:
// the version rows back up to store.LatestVersion, and the clearing
// accounts if they are missing. Those are inserted by hand, not with
// testutil.NewAccount: they need the exact owner 00005 seeds
// ('rail:<name>'), which the factory doesn't set. It needs at least one
// version row: on an empty table max() is NULL and the series is empty.
func migrateToLatest(t *testing.T, pool *pgxpool.Pool) {
	for _, name := range []string{rail.InternalName, rail.FakeName} {
		exec(t, pool, "INSERT INTO accounts (owner, kind, currency) SELECT 'rail:"+name+"', 'clearing', 'GBP' "+
			"WHERE NOT EXISTS (SELECT 1 FROM accounts WHERE kind = 'clearing' AND owner = 'rail:"+name+"')")
	}
	exec(t, pool, "INSERT INTO goose_db_version (version_id, is_applied) SELECT v, true "+
		"FROM generate_series((SELECT max(version_id) FROM goose_db_version) + 1, "+strconv.FormatInt(store.LatestVersion(), 10)+") v")
}

func resolve(ctx context.Context, t *testing.T, pool *pgxpool.Pool, log *slog.Logger) (ledger.ClearingAccountID, error) {
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	return resolveClearing(ctx, pool, db, rail.InternalName, log, fastTimings.lookupRetry, fastTimings.lookupTimeout)
}

// within is a test context with a deadline, for a resolve expected to
// succeed: a regression that retries instead fails at the deadline rather
// than hanging until go test's own timeout.
func within(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestResolveClearing_Found(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	want, err := ledger.ClearingAccount(t.Context(), pool, rail.InternalName)
	if err != nil {
		t.Fatal(err)
	}
	log, _ := testLogger()
	if got, err := resolve(within(t), t, pool, log); err != nil || got != want {
		t.Fatalf("resolveClearing = %v, %v; want %v", got, err, want)
	}
}

// Schema version 4: below the version this binary needs, so "not migrated
// yet": keep retrying, with a line naming `make migrate`, never exit
// (ADR-125 §3 as amended). Once the migrations land, the next attempt
// finds the account.
func TestResolveClearing_SchemaV4RetriesUntilMigrated(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	atVersion(t, pool, 4)
	log, logs := testLogger()

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if _, err := resolve(ctx, t, pool, log); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("resolveClearing at version 4 = %v, want it still retrying at the deadline", err)
	}
	if n := strings.Count(logs.String(), "make migrate"); n < 2 {
		t.Errorf("%d retry lines naming make migrate, want at least 2; logs:\n%s", n, logs)
	}
	if !strings.Contains(logs.String(), "schema older than this binary needs") ||
		!strings.Contains(logs.String(), `"schema_version":4`) {
		t.Errorf("retry line doesn't say why; logs:\n%s", logs)
	}

	migrateToLatest(t, pool)
	if _, err := resolve(within(t), t, pool, log); err != nil {
		t.Fatalf("resolveClearing after migrating = %v, want the id", err)
	}
}

// Migrations committing between the two reads must not make a migrated
// gateway exit (10a-ii-b review should 1). lookupClearing reads the version
// first, so a 4 is a retry and the next attempt finds the account. Read
// the other way round, this interleaving is "no account" then the latest
// version: a wrong configuration error. Not parallel: it sets a
// package-level hook, and parallel tests only start once the sequential
// ones have finished.
func TestResolveClearing_MigrationBetweenReadsDoesNotExit(t *testing.T) {
	pool := testutil.Postgres(t)
	atVersion(t, pool, 4)
	var once sync.Once
	testHookBetweenReads = func() { once.Do(func() { migrateToLatest(t, pool) }) }
	t.Cleanup(func() { testHookBetweenReads = nil })
	log, _ := testLogger()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, err := resolve(ctx, t, pool, log); err != nil {
		t.Fatalf("resolveClearing with 00005 landing between the reads = %v, want the id", err)
	}
}

// The latest schema with the seed row deleted: the database is migrated
// and misconfigured, so startup exits at once with an error naming the rail.
func TestResolveClearing_LatestMissingSeedExits(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t) // migrated to store.LatestVersion, the boundary
	exec(t, pool, "DELETE FROM accounts WHERE owner = 'rail:internal'")
	log, logs := testLogger()

	// A deadline, so a regression that retries fails here instead of hanging.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, err := resolve(ctx, t, pool, log)
	if !errors.Is(err, errConfig) || !errors.Is(err, ledger.ErrNoClearingAccount) {
		t.Fatalf("resolveClearing = %v, want errConfig wrapping ErrNoClearingAccount", err)
	}
	for _, s := range []string{`"internal"`, "schema version " + strconv.FormatInt(store.LatestVersion(), 10)} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error %q doesn't contain %s", err, s)
		}
	}
	if strings.Contains(logs.String(), "retrying") {
		t.Errorf("a configuration error was retried; logs:\n%s", logs)
	}
}

// One version behind (the skew ADR-126 is about), with the seed deleted as
// well: still "not migrated yet", never an exit. The account isn't read
// below the latest version, so a missing seed there can't decide anything.
// The retry line carries only fixed text and integers (AC8).
func TestResolveClearing_BehindRetries(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	behind := store.LatestVersion() - 1
	atVersion(t, pool, behind)
	exec(t, pool, "DELETE FROM accounts WHERE owner = 'rail:internal'")
	log, logs := testLogger()

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if _, err := resolve(ctx, t, pool, log); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("resolveClearing one version behind = %v, want it still retrying at the deadline", err)
	}
	want := `"schema_version":` + strconv.FormatInt(behind, 10) + `,"required_version":` + strconv.FormatInt(store.LatestVersion(), 10)
	if n := strings.Count(logs.String(), want); n < 2 {
		t.Errorf("%d retry lines with %s, want at least 2; logs:\n%s", n, want, logs)
	}
	if strings.Contains(logs.String(), "configuration error") {
		t.Errorf("a schema behind the binary was treated as a configuration error; logs:\n%s", logs)
	}
}

// A schema newer than this binary knows wires (the gate is a minimum,
// ADR-125 §3's second amendment), with one Warn saying so.
func TestResolveClearing_NewerSchemaWires(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	exec(t, pool, "INSERT INTO goose_db_version (version_id, is_applied) VALUES ("+strconv.FormatInt(store.LatestVersion()+1, 10)+", true)")
	log, logs := testLogger()
	if _, err := resolve(within(t), t, pool, log); err != nil {
		t.Fatalf("resolveClearing on a newer schema = %v, want the id", err)
	}
	if n := strings.Count(logs.String(), "schema is newer than this binary knows"); n != 1 {
		t.Errorf("%d newer-schema lines, want 1; logs:\n%s", n, logs)
	}
}

// The gate only reads (root CLAUDE.md: no startup path may call anything
// capable of applying a migration). goose's Provider.GetDBVersion would
// create a missing version table and, on a Provider with a session locker
// (store's), wait on the migration lock; here the
// table stays absent, and an attempt with that lock held by another
// session (a running `make migrate`) still answers within its deadline.
func TestResolveClearing_NoMigrationSideEffects(t *testing.T) {
	t.Parallel()
	t.Run("version table stays absent", func(t *testing.T) {
		t.Parallel()
		pool := testutil.Postgres(t)
		exec(t, pool, "DROP TABLE goose_db_version")
		log, _ := testLogger()
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer cancel()
		_, _ = resolve(ctx, t, pool, log)
		var exists bool
		if err := pool.QueryRow(t.Context(), "SELECT to_regclass('goose_db_version') IS NOT NULL").Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists {
			t.Error("resolveClearing created goose_db_version; startup must only read the version")
		}
	})
	t.Run("migration lock held", func(t *testing.T) {
		t.Parallel()
		pool := testutil.Postgres(t)
		conn, err := pool.Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		if _, err := conn.Exec(t.Context(), "SELECT pg_advisory_lock($1)", lock.DefaultLockID); err != nil {
			t.Fatal(err)
		}
		defer func() { _, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", lock.DefaultLockID) }()
		log, _ := testLogger()
		start := time.Now()
		if _, err := resolve(within(t), t, pool, log); err != nil {
			t.Fatalf("resolveClearing with the migration lock held = %v, want the id", err)
		}
		if d := time.Since(start); d > fastTimings.lookupTimeout {
			t.Errorf("resolveClearing took %v with the migration lock held, want under %v (it must not wait on it)", d, fastTimings.lookupTimeout)
		}
	})
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
	if !strings.Contains(logs.String(), "database unreachable or not answering as expected") {
		t.Errorf("no retry line giving the read-failure reason; logs:\n%s", logs)
	}
	if strings.Contains(logs.String(), "make migrate") {
		t.Errorf("a read failure was blamed on migrations; logs:\n%s", logs)
	}
	for _, s := range []string{"leakuser", "leakpass", "leakdb", "127.0.0.1"} {
		if strings.Contains(logs.String(), s) {
			t.Errorf("log contains %q:\n%s", s, logs)
		}
	}
}

// A reachable database that was never migrated (a fresh `make dev` stack):
// no version table, so version 0, which is behind: retry with the schema
// reason, never exit, and never read the (missing) accounts table.
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
	if n := strings.Count(logs.String(), `"schema_version":0`); n < 2 {
		t.Errorf("%d retry lines giving schema version 0, want at least 2; logs:\n%s", n, logs)
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

// AC10 (and AC7), end to end through the router, on a real schema one
// migration behind this binary: 00007 rolled back, so no key_hash, which
// every create needs. Before 14a this gateway reported ready and answered
// 500 to every create. Now it serves at once, /readyz answers not ready
// (the wiring doesn't block it) and payments get 503 try_again_later.
// After `make migrate` (store.Up), the rail is wired, /readyz is ready and
// a payment settles through the internal rail. Cancelling stops the
// background work.
func TestWireSettlement_SkewNotReadyThenMigrated(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	p, err := goose.NewProvider(goose.DialectPostgres, db, os.DirFS("../../internal/store/migrations"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.DownTo(t.Context(), store.LatestVersion()-1); err != nil {
		t.Fatalf("roll back one migration: %v", err)
	}
	log, logs := testLogger()

	g, hooks := newGate(payGateTitle), newGate(hookGateTitle)
	router := httpx.NewRouter(log, g.readyCheck())
	mountAPIs(router, g, hooks, httpx.NewRateLimiter(config.DefaultWebhookRate, config.DefaultWebhookBurst, nil, log))

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- wireSettlement(ctx, pool, wiring{webhookBound: 2}, nil, log, g, hooks, fastTimings) }()

	// Let the gate decide, and retry once, before asserting: checked straight
	// after the goroutine starts, the 503s would only show the wiring hadn't
	// run yet, and a gateway without the gate (one that wires at once on
	// this schema) would pass (14a review B1).
	waitFor(t, func() bool { return strings.Count(logs.String(), "schema older than this binary needs") >= 2 })
	if g.ready() || hooks.ready() {
		t.Fatalf("rail wired on a schema one migration behind (payments ready %v, webhooks ready %v)", g.ready(), hooks.ready())
	}
	if code := serve(router, http.MethodGet, "/readyz", "").Code; code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz before migrating = %d, want 503", code)
	}
	for _, path := range []string{"/v1/payments", "/v1/rail/webhooks/internal"} {
		rec := serve(router, http.MethodPost, path, `{}`)
		if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "1" || !strings.Contains(rec.Body.String(), "try_again_later") {
			t.Fatalf("POST %s before migrating = %d %q (Retry-After %q), want 503 try_again_later", path, rec.Code, rec.Body, rec.Header().Get("Retry-After"))
		}
	}

	if err := store.Up(t.Context(), db); err != nil {
		t.Fatalf("store.Up: %v", err)
	}
	waitFor(t, g.ready)
	// /readyz caches a result for up to a second.
	waitFor(t, func() bool { return serve(router, http.MethodGet, "/readyz", "").Code == http.StatusOK })

	payer := testutil.NewAccount(t, pool, "user")
	payee := testutil.NewAccount(t, pool, "merchant")
	body := `{"amount":1250,"currency":"GBP","payer_account_id":"` + payer.String() + `","payee_account_id":"` + payee.String() + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/payments", strings.NewReader(body))
	req.Header.Set("Idempotency-Key", "wire-test-1")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
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
	g, hooks := newGate(payGateTitle), newGate(hookGateTitle)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	err := wireSettlement(ctx, pool, wiring{allowFake: true, webhookBound: 2}, nil, log, g, hooks, fastTimings)
	if !errors.Is(err, errConfig) {
		t.Fatalf("wireSettlement = %v, want errConfig", err)
	}
	if g.ready() || hooks.ready() {
		t.Fatal("a gate opened despite the configuration error")
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
	newGate(payGateTitle).set(nil)
}

func serve(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	return serveFrom(h, method, path, body, "")
}

// serveFrom is serve from remoteAddr ("ip:port"; empty keeps httptest's
// 192.0.2.1:1234).
func serveFrom(h http.Handler, method, path, body, remoteAddr string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	h.ServeHTTP(rec, req)
	return rec
}

// ADR-127: an unset bound is a quarter of the pool, clamped to 1..64; an
// explicit one may take at most half. Configuration never scripts the fake.
func TestWiringFor(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		bound    int
		maxConns int32
		want     int // 0: errConfig
	}{
		{0, 2, 1}, {0, 4, 1}, {0, 8, 2}, {0, 16, 4}, {0, 400, 64},
		{1, 2, 1}, {2, 4, 2}, {4, 8, 4}, {3, 8, 3},
		{2, 3, 0}, {3, 4, 0}, {5, 8, 0}, {64, 100, 0},
	} {
		for _, fake := range []bool{false, true} {
			w, err := wiringFor(config.Config{WebhookBound: tt.bound, AllowFakeRail: fake}, tt.maxConns)
			if tt.want == 0 {
				if !errors.Is(err, errConfig) || !errors.Is(err, errWebhookBoundTooLarge) {
					t.Errorf("bound %d, pool %d: err = %v, want errWebhookBoundTooLarge", tt.bound, tt.maxConns, err)
				}
				continue
			}
			if err != nil || w.webhookBound != tt.want || w.allowFake != fake {
				t.Errorf("bound %d, pool %d: %+v, %v; want bound %d", tt.bound, tt.maxConns, w, err, tt.want)
			}
			if w.fakeSteps != nil {
				t.Errorf("bound %d, pool %d: configuration scripted the fake: %v", tt.bound, tt.maxConns, w.fakeSteps)
			}
		}
	}
}

// A pool below 2 connections is refused whatever the bound: it can't give
// payments the majority and the webhook a share.
func TestWiringFor_PoolBelowTwoRefused(t *testing.T) {
	t.Parallel()
	for _, bound := range []int{0, 1} {
		for _, maxConns := range []int32{0, 1} {
			if _, err := wiringFor(config.Config{WebhookBound: bound}, maxConns); !errors.Is(err, errConfig) || !errors.Is(err, errPoolTooSmall) {
				t.Errorf("bound %d, pool %d: err = %v, want errPoolTooSmall", bound, maxConns, err)
			}
		}
	}
}

// ADR-127's invariant: the derived default always passes the check an
// explicit value gets. For every pool size wiringFor accepts, setting the
// default explicitly is accepted too, so the rule can't contradict itself
// at a boundary.
func TestWiringFor_DefaultPassesTheExplicitCheck(t *testing.T) {
	t.Parallel()
	for maxConns := int32(2); maxConns <= 1000; maxConns++ {
		w, err := wiringFor(config.Config{}, maxConns)
		if err != nil {
			t.Fatalf("pool %d: default refused: %v", maxConns, err)
		}
		if _, err := wiringFor(config.Config{WebhookBound: w.webhookBound}, maxConns); err != nil {
			t.Fatalf("pool %d: the default bound %d, set explicitly, is refused: %v", maxConns, w.webhookBound, err)
		}
	}
}

// The default relationship, on real pools: whatever size store.OpenPool
// gives (pgx's default max(4, NumCPU) on this machine, or pool_max_conns),
// the derived bound leaves the payments API at least three quarters of it.
// Before ADR-127 the bound was 8 whatever the pool, twice a 4-connection
// pool. OpenPool doesn't connect, so no database is needed.
func TestWiringFor_DefaultLeavesPaymentsTheMajority(t *testing.T) {
	t.Parallel()
	for _, dsn := range []string{
		"postgres://u@localhost:1/db",
		"postgres://u@localhost:1/db?pool_max_conns=4",
		"postgres://u@localhost:1/db?pool_max_conns=10",
	} {
		pool, err := store.OpenPool(t.Context(), dsn)
		if err != nil {
			t.Fatal(err)
		}
		maxConns := pool.Config().MaxConns
		pool.Close()
		w, err := wiringFor(config.Config{}, maxConns)
		if err != nil {
			t.Fatal(err)
		}
		if w.webhookBound < 1 || w.webhookBound*4 > int(maxConns) {
			t.Errorf("pool of %d: bound %d, want 1 to a quarter of the pool", maxConns, w.webhookBound)
		}
	}
}

// run's composition (compose) decides the webhook bound from the pool it is
// given, before serving: this is what makes ADR-127 hold in production, not
// only in wiringFor. An unset bound comes out as a quarter of the pool, never
// 0 (payments.New would panic on it, in the background wiring goroutine); an
// explicit one over half the pool fails compose, so run exits. The router it
// returns has both APIs mounted behind their own gates. OpenPool doesn't
// connect, so no database is needed.
func TestCompose_DecidesTheBoundFromThePool(t *testing.T) {
	t.Parallel()
	open := func(maxConns string) *pgxpool.Pool { return lazyPool(t, maxConns) }
	log, _ := testLogger()
	cfg := config.Config{WebhookRate: config.DefaultWebhookRate, WebhookBurst: config.DefaultWebhookBurst}
	for maxConns, want := range map[string]int{"4": 1, "40": 10} {
		gw, err := compose(cfg, open(maxConns), log)
		if err != nil || gw.wired.webhookBound != want {
			t.Fatalf("pool of %s: bound %d (%v), want %d", maxConns, gw.wired.webhookBound, err, want)
		}
	}

	explicit := cfg
	explicit.WebhookBound = 3
	if _, err := compose(explicit, open("4"), log); !errors.Is(err, errWebhookBoundTooLarge) {
		t.Fatalf("bound 3 on a pool of 4: err = %v, want errWebhookBoundTooLarge", err)
	}

	gw, err := compose(cfg, open("4"), log)
	if err != nil {
		t.Fatal(err)
	}
	for path, title := range map[string]string{"/v1/payments": payGateTitle, "/v1/rail/webhooks/fake": hookGateTitle} {
		if rec := serve(gw.router, http.MethodPost, path, `{}`); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), title) {
			t.Errorf("POST %s before wiring = %d %s, want 503 %q", path, rec.Code, rec.Body, title)
		}
	}
	if code := serve(gw.router, http.MethodGet, "/readyz", "").Code; code != http.StatusServiceUnavailable {
		t.Errorf("/readyz before wiring = %d, want 503", code)
	}
}

// lazyPool is a pool of maxConns connections that never connects: OpenPool
// is lazy, so a test that never reaches the database runs without Docker.
func lazyPool(t *testing.T, maxConns string) *pgxpool.Pool {
	t.Helper()
	pool, err := store.OpenPool(t.Context(), "postgres://u@localhost:1/db?pool_max_conns="+maxConns)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// mounted is the gateway as run builds it (compose), on a test database.
type mounted struct {
	router    http.Handler
	pool      *pgxpool.Pool
	pay, hook *gate
	limiter   *httpx.RateLimiter
	wired     wiring // as compose decided it
	log       *slog.Logger
	logs      *syncBuffer
}

// newMounted composes the gateway from cfg on a fresh test database, with
// the fake rail allowed.
func newMounted(t *testing.T, cfg config.Config) *mounted {
	t.Helper()
	pool := testutil.Postgres(t)
	log, logs := testLogger()
	cfg.AllowFakeRail = true
	gw, err := compose(cfg, pool, log)
	if err != nil {
		t.Fatal(err)
	}
	return &mounted{router: gw.router, pool: pool, pay: gw.pay, hook: gw.hooks, limiter: gw.limiter, wired: gw.wired, log: log, logs: logs}
}

// defaultWebhookConfig is the webhook part of a default config.
func defaultWebhookConfig() config.Config {
	return config.Config{WebhookRate: config.DefaultWebhookRate, WebhookBurst: config.DefaultWebhookBurst}
}

// wire runs wireSettlement with the wiring compose decided, adding only the
// fake's script (which configuration can't set), until the test ends, and
// waits for both gates to open.
func (m *mounted) wire(t *testing.T, steps ...rail.Step) {
	t.Helper()
	w := m.wired
	w.fakeSteps = steps
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = wireSettlement(t.Context(), m.pool, w, nil, m.log, m.pay, m.hook, fastTimings)
	}()
	// Registered after testutil.Postgres's cleanup, so it runs first: the
	// background work stops (t.Context is cancelled by then) before the
	// pool closes.
	t.Cleanup(func() { <-done })
	waitFor(t, func() bool { return m.pay.ready() && m.hook.ready() })
}

// createSettling creates a payment from payer through the mounted
// payments API and checks the fake left it settling.
func (m *mounted) createSettling(t *testing.T, payer uuid.UUID, key string) uuid.UUID {
	t.Helper()
	payee := testutil.NewAccount(t, m.pool, "merchant")
	req := httptest.NewRequest(http.MethodPost, "/v1/payments", strings.NewReader(
		`{"amount":1250,"currency":"GBP","payer_account_id":"`+payer.String()+`","payee_account_id":"`+payee.String()+`"}`))
	req.Header.Set("Idempotency-Key", key)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	m.router.ServeHTTP(rec, req)
	var created payments.Payment
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &created) != nil || created.State != payments.Settling {
		t.Fatalf("POST = %d %s, want 201 settling", rec.Code, rec.Body)
	}
	return created.ID
}

// The bound compose decides (ADR-127) is the one the mounted webhook
// enforces: GATEWAY_WEBHOOK_BOUND=1 goes through compose (legal on the
// test pool of 8) and on to wireSettlement unchanged. With a bound of 1, one webhook holds the only slot while the
// fake's Status waits on a locked table, and a second, from another source
// so the limiter isn't what refuses it, is shed at once: 503
// try_again_later, not a wait. With a larger bound it would wait on the
// lock instead and end in request_timeout, so this fails if wiring stops
// passing its bound to payments.
func TestMountedAPIs_WebhookBoundIsWired(t *testing.T) {
	t.Parallel()
	cfg := defaultWebhookConfig()
	cfg.WebhookBound = 1
	m := newMounted(t, cfg)
	if m.wired.webhookBound != 1 {
		t.Fatalf("compose decided bound %d, want 1", m.wired.webhookBound)
	}
	m.wire(t, rail.Step{Outcome: rail.Pending})
	id := m.createSettling(t, testutil.NewAccount(t, m.pool, "user"), "bound-1")
	body := `{"ref":"` + rail.Ref(id) + `"}`

	tx, err := m.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(t.Context(), "LOCK TABLE fake_rail_operations IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	var firstCode int
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		firstCode = serveFrom(m.router, http.MethodPost, "/v1/rail/webhooks/fake", body, "198.51.100.1:1").Code
	}()
	// Joined on every path, a failing one included, so the first webhook is
	// never still running when the cleanups close the pool. Deferred after
	// the rollback above, so it runs first and the lock goes before the join.
	defer func() { _ = tx.Rollback(context.Background()); <-firstDone }()
	// The first webhook holds the slot once its Status is waiting on the lock.
	waitFor(t, func() bool {
		var n int
		err := m.pool.QueryRow(t.Context(), "SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'").Scan(&n)
		return err == nil && n > 0
	})

	start := time.Now()
	rec := serveFrom(m.router, http.MethodPost, "/v1/rail/webhooks/fake", body, "198.51.100.2:1")
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "Too many concurrent webhooks") || time.Since(start) > time.Second {
		t.Fatalf("second webhook = %d %s after %v, want an immediate 503 shed", rec.Code, rec.Body, time.Since(start))
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	<-firstDone
	if firstCode != http.StatusNoContent {
		t.Fatalf("first webhook = %d, want 204 once the lock is gone", firstCode)
	}
}

// AC5 and ADR-124, end to end through the mounted router: a payment the
// fake leaves pending is settled by a webhook once the rail has finished,
// a duplicate changes nothing, and the money moved once (one rail move,
// the two-leg payer posting, payer and clearing balances opposite). AC8:
// nothing from the webhook body or the caller's address reaches the log.
func TestMountedAPIs_WebhookSettlesPendingPayment(t *testing.T) {
	t.Parallel()
	m := newMounted(t, defaultWebhookConfig())
	m.wire(t, rail.Step{Outcome: rail.Pending})
	payer := testutil.NewAccount(t, m.pool, "user")
	id := m.createSettling(t, payer, "mounted-1")
	// The fake's table is shared, so a second Fake resolves it, as the PSP
	// finishing would.
	if err := rail.NewFake(m.pool).Resolve(t.Context(), rail.Ref(id), rail.Settled, ""); err != nil {
		t.Fatal(err)
	}

	const addr, canary = "203.0.113.77:4444", "canary-7d1f"
	body := `{"ref":"` + rail.Ref(id) + `","note":"` + canary + `"}`
	for i := range 2 { // the second is a duplicate
		if rec := serveFrom(m.router, http.MethodPost, "/v1/rail/webhooks/fake", body, addr); rec.Code != http.StatusNoContent {
			t.Fatalf("webhook %d = %d %s, want 204", i, rec.Code, rec.Body)
		}
	}
	var state string
	if err := m.pool.QueryRow(t.Context(), "SELECT state FROM payments WHERE id = $1", id).Scan(&state); err != nil || state != "settled" {
		t.Fatalf("state = %q (%v), want settled", state, err)
	}
	if n, err := rail.NewFake(m.pool).Moves(t.Context(), rail.Ref(id)); err != nil || n != 1 {
		t.Fatalf("rail moves = %d (%v), want 1", n, err)
	}
	var legs int
	if err := m.pool.QueryRow(t.Context(), "SELECT count(*) FROM ledger_entries WHERE rail_ref = $1", payments.PayerRef(id)).Scan(&legs); err != nil || legs != 2 {
		t.Fatalf("payer posting legs = %d (%v), want 2", legs, err)
	}
	clearing, err := ledger.ClearingAccount(t.Context(), m.pool, rail.FakeName)
	if err != nil {
		t.Fatal(err)
	}
	for acct, want := range map[uuid.UUID]int64{payer: -1250, uuid.UUID(clearing): 1250} {
		if got, err := ledger.Balance(t.Context(), m.pool, acct); err != nil || got != want {
			t.Errorf("balance of %s = %d (%v), want %d", acct, got, err, want)
		}
	}

	if rec := serveFrom(m.router, http.MethodPost, "/v1/rail/webhooks/stripe", body, addr); rec.Code != http.StatusNotFound ||
		!strings.Contains(rec.Body.String(), "unknown_rail") {
		t.Fatalf("unknown rail = %d %s, want 404 unknown_rail", rec.Code, rec.Body)
	}
	for _, leak := range []string{canary, "203.0.113.77"} {
		if strings.Contains(m.logs.String(), leak) {
			t.Errorf("log contains %q; logs:\n%s", leak, m.logs)
		}
	}
}

// The limiter is outermost on the webhook route, in front of the gate: with
// the gate still closed, a source's first burst requests reach it (503,
// with the webhook's own title) and the next is refused by the limiter
// (429). Another source has its own bucket, and /v1/payments has no
// limiter. This pins the order mountAPIs's comment says is load-bearing
// for log volume (Ivan, 2026-10-31). The gates stay closed, so nothing
// reaches the database: it runs on a lazy pool and never skips when Docker
// is down, because a guard that vanishes silently looks like one that
// passed.
func TestMountedAPIs_LimiterBeforeGate(t *testing.T) {
	t.Parallel()
	const burst = 3
	log, _ := testLogger()
	m, err := compose(config.Config{WebhookRate: 1, WebhookBurst: burst}, lazyPool(t, "8"), log)
	if err != nil {
		t.Fatal(err)
	}
	const path, body = "/v1/rail/webhooks/fake", `{"ref":"x"}`

	for i := range burst {
		rec := serveFrom(m.router, http.MethodPost, path, body, "198.51.100.1:1")
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), hookGateTitle) {
			t.Fatalf("webhook %d before wiring = %d %s, want 503 %q", i, rec.Code, rec.Body, hookGateTitle)
		}
	}
	rec := serveFrom(m.router, http.MethodPost, path, body, "198.51.100.1:1")
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "rate_limited") || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("webhook over the burst = %d %s (Retry-After %q), want 429 rate_limited", rec.Code, rec.Body, rec.Header().Get("Retry-After"))
	}
	if got := m.limiter.Limited(); got != 1 {
		t.Fatalf("Limited() = %d, want 1", got)
	}
	if rec := serveFrom(m.router, http.MethodPost, path, body, "198.51.100.2:1"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("another source = %d, want 503 (its own bucket)", rec.Code)
	}
	for i := range 4 * burst {
		rec := serveFrom(m.router, http.MethodPost, "/v1/payments", `{}`, "198.51.100.1:1")
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), payGateTitle) {
			t.Fatalf("payments request %d = %d %s, want 503 %q (no limiter)", i, rec.Code, rec.Body, payGateTitle)
		}
	}
}

// SPEC-002 14c (ADR-128): compose hands GATEWAY_TRUSTED_PROXIES to the
// webhook's limiter. Behind the trusted proxy two clients get a bucket
// each; a caller connecting directly can't choose its bucket with the
// header; and no address, peer or forwarded, reaches the log (AC8, Data
// Management Plan §1). Gates closed and a lazy pool, as in
// TestMountedAPIs_LimiterBeforeGate, so it never skips without Docker.
func TestMountedAPIs_LimiterTrustsConfiguredProxy(t *testing.T) {
	t.Parallel()
	const burst = 2
	log, logs := testLogger()
	cfg := config.Config{WebhookRate: 1, WebhookBurst: burst, TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}
	m, err := compose(cfg, lazyPool(t, "8"), log)
	if err != nil {
		t.Fatal(err)
	}
	limited := func(remote, xff string, n int) int {
		got := 0
		for range n {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/rail/webhooks/fake", strings.NewReader(`{"ref":"x"}`))
			req.RemoteAddr = remote
			req.Header.Set("X-Forwarded-For", xff)
			m.router.ServeHTTP(rec, req)
			if rec.Code == http.StatusTooManyRequests {
				got++
			}
		}
		return got
	}
	if a, b := limited("10.0.0.1:1", "198.51.100.1", burst+1), limited("10.0.0.1:2", "198.51.100.2", burst+1); a != 1 || b != 1 {
		t.Fatalf("clients behind the proxy: %d and %d of %d limited, want 1 each (a bucket each)", a, b, burst+1)
	}
	if got := limited("192.0.2.1:1", "203.0.113.1", burst) + limited("192.0.2.1:1", "203.0.113.2", 1); got != 1 {
		t.Fatalf("direct caller rotating the header: %d limited, want 1 (one bucket)", got)
	}
	if !strings.Contains(logs.String(), `"status":429`) {
		t.Fatalf("no 429 in the request log, so the check below would prove nothing:\n%s", logs)
	}
	for _, s := range []string{"198.51.100", "203.0.113", "192.0.2.1", "10.0.0.1"} {
		if strings.Contains(logs.String(), s) {
			t.Fatalf("log contains %q:\n%s", s, logs)
		}
	}
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
