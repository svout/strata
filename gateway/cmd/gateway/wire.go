package main

// Settlement wiring (SPEC-002 10a-ii-b). The gateway starts serving at once;
// the rail is wired in the background once the schema is at the version
// this binary needs (SPEC-002 14a) and its clearing account is known, and
// until then /readyz is not ready and /v1/payments answers 503.
// This file is the one place that maps the startup lookup's errors to
// "retry" or "exit" (ADR-125 §3, as amended).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/svout/strata/gateway/internal/config"
	"github.com/svout/strata/gateway/internal/httpx"
	"github.com/svout/strata/gateway/internal/idem"
	"github.com/svout/strata/gateway/internal/ledger"
	"github.com/svout/strata/gateway/internal/payments"
	"github.com/svout/strata/gateway/internal/rail"
	"github.com/svout/strata/gateway/internal/store"
)

// timings are the wiring's delays; tests shorten them.
type timings struct {
	// lookupRetry spaces clearing-account lookups while the database is
	// down or not migrated. Capped at 30 s so a stack that gets migrated is
	// ready within half a minute.
	lookupRetry backoff
	// lookupTimeout bounds one lookup attempt (both queries).
	lookupTimeout time.Duration
	// recoverEvery is the gap between successful Recover passes. It is not
	// tied to payments' grace period (defaultRecoverGrace, also 15 s): the
	// grace decides which payments a pass may touch, the interval how often
	// passes run, and neither constrains the other. Together they bound
	// recovery: while passes succeed and finish the backlog, a stuck
	// payment is resolved at most about grace + recoverEvery (+ one pass)
	// after its last update, ~30 s with both at 15 s. Failed passes (backoff
	// up to 30 s) and passes cut short by recoverTimeout push that out. A
	// shorter interval only costs more backlog queries; a longer one only
	// delays recovery.
	recoverEvery time.Duration
	// recoverRetry spaces passes after one fails.
	recoverRetry backoff
	// recoverTimeout bounds one pass. A pass cut short leaves the rest of
	// the backlog settling for the next one, which Recover is built for.
	recoverTimeout time.Duration
}

// defaultTimings are the values Ivan approved at 10a-ii-b planning
// (2026-10-21): lookup 1 s doubling to 30 s; Recover every 15 s, 1 s
// doubling to 30 s after a failure.
var defaultTimings = timings{
	lookupRetry:    backoff{first: time.Second, max: 30 * time.Second},
	lookupTimeout:  5 * time.Second,
	recoverEvery:   15 * time.Second,
	recoverRetry:   backoff{first: time.Second, max: 30 * time.Second},
	recoverTimeout: time.Minute,
}

// wiring is what wireSettlement builds the payments Service from: settings
// decided once at startup.
type wiring struct {
	allowFake bool
	// webhookBound is the Service's webhook bound (payments.WithWebhookBound),
	// derived from or checked against the pool by wiringFor (ADR-127).
	webhookBound int
	// fakeSteps scripts the fake rail's outcomes. It is test-only and
	// unreachable from configuration: wiringFor never sets it, nothing
	// reads it from the environment, and only a test in this package can
	// construct a wiring with it (SPEC-002 10b-ii). In production the fake
	// runs unscripted (it settles at once), and only when allowFake.
	fakeSteps []rail.Step
}

// Webhook pool share (ADR-127): the payment path keeps the majority of the
// pool, so the webhook's bound is a minority fraction of it. A fraction
// rather than "below the pool", because a webhook gives its slot back
// before its write (payments, ADR-124 addendum), so in-flight webhook
// writes come on top of the bound. assumed: both fractions (not measured;
// revisit with P6's load numbers).
const (
	webhookPoolDivisor = 4 // default bound: a quarter of the pool
	webhookPoolMaxDiv  = 2 // an explicit bound may take at most half
	maxDefaultBound    = 64
)

// errPoolTooSmall is errConfig's text when the pool has fewer than 2
// connections: it can't give payments the majority and the webhook a
// share, and the default bound of 1 would break the half-pool rule below.
var errPoolTooSmall = errors.New("the PostgreSQL pool (pool_max_conns) needs at least 2 connections: " +
	"one route can't keep the majority of a one-connection pool while another has a share")

// errWebhookBoundTooLarge is errConfig's text when GATEWAY_WEBHOOK_BOUND
// would take more than half the pool. Fixed text: the numbers stay out, as
// with every errConfig error.
var errWebhookBoundTooLarge = errors.New("GATEWAY_WEBHOOK_BOUND is more than half the PostgreSQL pool (pool_max_conns); " +
	"the payments API must keep the majority, so lower the bound or raise pool_max_conns")

// wiringFor decides the wiring from cfg and the pool's size (maxConns,
// from pool.Config(), which opens no connection). An unset bound is a
// quarter of the pool, clamped to 1..64; an explicit one above half the
// pool is an errConfig error and the gateway exits, as is a pool below 2.
// The derived default always passes the check explicit values get
// (ADR-127's invariant): with at least 2 connections, the clamped quarter
// is at most half.
func wiringFor(cfg config.Config, maxConns int32) (wiring, error) {
	if maxConns < 2 {
		return wiring{}, fmt.Errorf("%w: %w", errConfig, errPoolTooSmall)
	}
	w := wiring{allowFake: cfg.AllowFakeRail, webhookBound: cfg.WebhookBound}
	if w.webhookBound == 0 {
		w.webhookBound = min(max(int(maxConns)/webhookPoolDivisor, 1), maxDefaultBound)
		return w, nil
	}
	if w.webhookBound*webhookPoolMaxDiv > int(maxConns) {
		return wiring{}, fmt.Errorf("%w: %w", errConfig, errWebhookBoundTooLarge)
	}
	return w, nil
}

// railName is the one rail this process registers. The fake replaces the
// internal rail rather than sitting beside it: choosing a rail per payment
// is a P2 decision (mandates, policy), not a flag.
func railName(allowFake bool) string {
	if allowFake {
		return rail.FakeName
	}
	return rail.InternalName
}

// wireSettlement wires the payments API to its rail and then runs recovery
// until ctx ends. It returns nil when ctx ends, and an errConfig error if
// the gateway must exit. It runs in the background, so /readyz answers
// throughout and reports "rail" as failing until pay is set. hooks (the
// rail webhook) opens first, so a ready gateway serves both; the gap
// between the two sets is harmless (see compose).
func wireSettlement(ctx context.Context, pool *pgxpool.Pool, w wiring, locker idem.Locker, log *slog.Logger, pay, hooks *gate, tm timings) error {
	// goose (store.SchemaVersion) works over database/sql; this wraps the
	// same pool, and closing it leaves the pool open.
	db := stdlib.OpenDBFromPool(pool)
	defer func() { _ = db.Close() }()

	name := railName(w.allowFake)
	id, err := resolveClearing(ctx, pool, db, name, log, tm.lookupRetry, tm.lookupTimeout)
	if err == nil {
		err = checkClearing(name, id)
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil // shutting down before the rail was wired
		}
		// Logged here as well as returned: run prints the returned error to
		// stderr only, and an operator's log pipeline collects the JSON log,
		// so without this line the exit would be invisible to monitoring.
		// The text is safe to log: errConfig only wraps fixed-text errors.
		log.LogAttrs(ctx, slog.LevelError, "rail wiring failed; the gateway will exit",
			slog.String("rail", name), slog.String("error", err.Error()))
		return err
	}
	var adapter rail.Adapter
	if w.allowFake {
		// The fake keeps its records in fake_rail_operations (migration
		// 00006). Wiring needs the latest schema (14a), so the table is
		// always there by now.
		adapter = rail.NewFake(pool, w.fakeSteps...)
		log.LogAttrs(ctx, slog.LevelWarn, "fake rail registered: payments settle without moving real money (GATEWAY_ALLOW_FAKE_RAIL=1)")
	} else {
		adapter = rail.NewInternal(pool, id)
	}
	svc := payments.New(pool, payments.WithLocker(locker), payments.WithRail(adapter, id), payments.WithLogger(log),
		payments.WithWebhookBound(w.webhookBound))
	hooks.set(svc.Webhook(log))
	pay.set(svc.Handler(log))
	log.LogAttrs(ctx, slog.LevelInfo, "rail wired", slog.String("rail", name))

	recoverLoop(ctx, svc, log, tm.recoverEvery, tm.recoverRetry, tm.recoverTimeout)
	return nil
}

// errSchemaBehind means the database's schema is older than this binary
// needs (store.LatestVersion): not migrated yet, so retry. Wiring there
// would serve 500s once a request reached a column or table the code
// expects (SPEC-002 14a, ADR-126's version skew).
var errSchemaBehind = errors.New("schema older than this binary needs")

// backoff is a capped exponential retry delay: first, then doubling up to
// max. The zero value is not usable; see defaultTimings.
type backoff struct {
	first, max, cur time.Duration
}

// next returns the delay before the next attempt and doubles the one after.
func (b *backoff) next() time.Duration {
	if b.cur == 0 {
		b.cur = b.first
	}
	d := b.cur
	b.cur = min(2*b.cur, b.max)
	return d
}

func (b *backoff) reset() { b.cur = 0 }

// sleep waits d or until ctx ends, and reports whether it waited the full d.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// errConfig marks a startup failure that retrying can't fix: the gateway
// exits with it. Its text is logged and printed to stderr, so it only ever
// wraps errors with fixed text (ledger's and our own), never a driver error.
var errConfig = errors.New("configuration error")

// resolveClearing looks up railName's clearing account, retrying until it
// can tell the answer is final:
//
//   - found: return it. A schema newer than this binary knows is accepted
//     (the gate is a minimum, ADR-125 §3's second amendment) with one Warn.
//   - the version couldn't be read (database unreachable): transient; retry.
//   - the schema is behind store.LatestVersion: not migrated yet; retry.
//   - no account at the latest schema: the seed row was removed: errConfig,
//     the gateway exits.
//   - the account couldn't be read: transient; retry.
//
// Every retry logs one fixed line; the schema-behind one names `make
// migrate`, the usual cause on a fresh `make dev` stack, and the other names
// no remedy, since an unreachable database has several. The underlying error
// is never logged: a driver
// error on the first connection names host, user and database. It returns
// ctx's error if ctx ends first.
func resolveClearing(ctx context.Context, pool *pgxpool.Pool, db *sql.DB, railName string, log *slog.Logger, wait backoff, timeout time.Duration) (ledger.ClearingAccountID, error) {
	for {
		id, version, err := lookupClearing(ctx, pool, db, railName, timeout)
		required := store.LatestVersion()
		switch {
		case err == nil:
			if version > required {
				// Not an error: migrate-then-deploy means a schema briefly
				// ahead of an old binary. Still worth a line, since a
				// binary older than its schema is ADR-126's known break.
				log.LogAttrs(ctx, slog.LevelWarn, "schema is newer than this binary knows; deploy the binary that matches it",
					slog.Int64("schema_version", version), slog.Int64("binary_version", required))
			}
			return id, nil
		case errors.Is(err, errSchemaBehind):
			d := wait.next()
			log.LogAttrs(ctx, slog.LevelWarn, "rail not wired yet: schema older than this binary needs; retrying (run `make migrate`)",
				slog.String("rail", railName), slog.Int64("schema_version", version),
				slog.Int64("required_version", required), slog.Int64("retry_in_ms", d.Milliseconds()))
			if !sleep(ctx, d) {
				return ledger.ClearingAccountID{}, ctx.Err()
			}
			continue
		case errors.Is(err, ledger.ErrNoClearingAccount):
			// lookupClearing only reads the account at the latest schema,
			// where 00005 has seeded it, so the row was removed.
			return ledger.ClearingAccountID{}, fmt.Errorf("%w: %w at schema version %d", errConfig, err, version)
		}
		// ErrSchemaVersion or ErrClearingLookup: a read failed, so all we
		// know is that the database didn't answer as expected.
		d := wait.next()
		log.LogAttrs(ctx, slog.LevelWarn, "rail not wired yet: clearing account unavailable; retrying",
			slog.String("rail", railName), slog.String("reason", "database unreachable or not answering as expected"), slog.Int64("retry_in_ms", d.Milliseconds()))
		if !sleep(ctx, d) {
			return ledger.ClearingAccountID{}, ctx.Err()
		}
	}
}

// lookupClearing is one attempt, under its own deadline: the schema
// version, then (only at the latest schema) the clearing account. A schema
// behind store.LatestVersion returns errSchemaBehind with the version read.
//
// The order matters; don't swap it. The two reads are separate statements,
// each seeing the database as it is when it runs, and `make migrate` may
// commit between them (the usual way: migrating a running `make dev`
// stack). Each of today's migrations commits with its version row (a future
// `-- +goose NO TRANSACTION` one would not), and 00005 seeds the clearing
// accounts, so:
//
//   - Version first: at least the latest version means the seeds are
//     already visible, so "no account" after it really is a configuration
//     error.
//     An older version is a retry, whatever lands after the read.
//   - Account first: "no account", then the migrations commit, then the
//     latest version, and a correctly migrated gateway exits. That is the
//     case ADR-125 §3 says must retry.
//
// The version read is on every attempt's path, which is one more reason
// store.SchemaVersion must not go through a goose Provider with a session
// locker: each attempt would wait behind a running `make migrate`.
func lookupClearing(ctx context.Context, pool *pgxpool.Pool, db *sql.DB, railName string, timeout time.Duration) (ledger.ClearingAccountID, int64, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	v, err := store.SchemaVersion(ctx, db)
	if err != nil {
		return ledger.ClearingAccountID{}, 0, err
	}
	if testHookBetweenReads != nil {
		testHookBetweenReads()
	}
	if v < store.LatestVersion() {
		return ledger.ClearingAccountID{}, v, errSchemaBehind
	}
	id, err := ledger.ClearingAccount(ctx, pool, railName)
	return id, v, err
}

// testHookBetweenReads, if set, runs right after lookupClearing's first read
// (before the version is judged), so a test can land a migration in the
// window. Always nil outside tests.
var testHookBetweenReads func()

// checkClearing is the non-zero assertion (SPEC-002 10a-ii-b, a2 review
// nit 3), run before the id reaches payments.WithRail or rail.NewInternal.
// ClearingAccount never returns a zero id, so a zero means a wiring
// mistake; the constructors don't check (ADR-125), so startup does.
func checkClearing(railName string, id ledger.ClearingAccountID) error {
	if id == (ledger.ClearingAccountID{}) {
		return fmt.Errorf("%w: zero clearing account id for rail %q", errConfig, railName)
	}
	return nil
}

// recoverer is the part of payments.Service the recovery loop needs.
type recoverer interface {
	Recover(context.Context) (payments.RecoverStats, error)
}

// recoverLoop runs Recover now and then every `every`, until ctx ends. A
// pass that fails (it couldn't read the backlog) is retried on wait's
// backoff instead, and the backoff resets after a pass succeeds.
//
// One pass at startup isn't enough: Recover skips payments settling for
// less than its grace period (15 s), so the ones in flight when the last
// process died are only picked up by a later pass. The error isn't logged,
// for the same reason as in resolveClearing; Recover logs its own outcomes.
func recoverLoop(ctx context.Context, svc recoverer, log *slog.Logger, every time.Duration, wait backoff, timeout time.Duration) {
	for {
		d := every
		if err := recoverPass(ctx, svc, timeout); err != nil {
			if ctx.Err() != nil {
				return
			}
			d = wait.next()
			log.LogAttrs(ctx, slog.LevelWarn, "settlement recovery pass failed; retrying",
				slog.Int64("retry_in_ms", d.Milliseconds()))
		} else {
			wait.reset()
		}
		if !sleep(ctx, d) {
			return
		}
	}
}

func recoverPass(ctx context.Context, svc recoverer, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, err := svc.Recover(ctx)
	return err
}

// gate serves an API once it is wired and 503 before that, so no payment is
// ever created, and no webhook handled, without a rail. There is one per
// mount (/v1/payments, /v1/rail/webhooks), each with its own 503 title, so
// an integrator reading the webhook's 503 isn't told about payments.
//
// It covers the whole /v1/payments mount, GET included, although only POST
// needs a rail. "Not ready, so 503 for everything" is a simpler contract
// for a client than per-route rules, and the window is the same one in
// which /readyz keeps a load balancer's traffic away anyway.
//
// atomic.Pointer needs a concrete type, so it holds a pointer to the
// handler interface value; ServeHTTP dereferences it.
type gate struct {
	title string // the 503's problem title until set
	h     atomic.Pointer[http.Handler]
}

func newGate(title string) *gate { return &gate{title: title} }

// set opens the gate. A nil handler panics here, at startup, rather than on
// the first request after the gate opened: a missing handler is a wiring
// bug, and it should fail where it was made.
func (g *gate) set(h http.Handler) {
	if h == nil {
		panic("gate: nil handler")
	}
	g.h.Store(&h)
}

func (g *gate) ready() bool { return g.h.Load() != nil }

func (g *gate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h := g.h.Load(); h != nil {
		(*h).ServeHTTP(w, r)
		return
	}
	// Same code and Retry-After as the payments API's own 503s.
	w.Header().Set("Retry-After", "1")
	httpx.WriteProblem(w, http.StatusServiceUnavailable, "try_again_later", g.title)
}

// errNotWired is the readiness check's failure until the rail is wired.
// /readyz never shows check errors, only "fail".
var errNotWired = errors.New("rail not wired yet")

// readyCheck reports the gate on /readyz as "rail".
func (g *gate) readyCheck() httpx.ReadyCheck {
	return httpx.ReadyCheck{Name: "rail", Check: func(context.Context) error {
		if !g.ready() {
			return errNotWired
		}
		return nil
	}}
}
