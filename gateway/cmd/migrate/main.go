// Command migrate applies the gateway's PostgreSQL schema migrations
// (internal/store) to the database named by DATABASE_URL.
//
//	migrate up    apply every pending migration
//
// Only "up" exists: production schemas move forward only, and store.Down is
// kept for tests. Runs are serialised by goose's advisory lock, so starting
// two at once is safe. Exit codes: 0 done, 1 failed, 2 usage error.
package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/stdlib"

	"github.com/svout/strata/gateway/internal/config"
	"github.com/svout/strata/gateway/internal/httpx"
	"github.com/svout/strata/gateway/internal/store"
)

const usage = `usage: migrate up

Applies every pending schema migration to the database in DATABASE_URL.
Environment: DATABASE_URL (required), LOG_LEVEL (default info).
`

// runTimeout bounds a whole run: up to 5 min waiting for another migrator's
// lock (store's retry policy) plus the migrations themselves.
const runTimeout = 10 * time.Minute

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// run is main without the process globals, so tests can drive it. Usage and
// configuration errors go to stderr as plain text; once the logger exists,
// everything goes through it (JSON on stdout, with redaction).
func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) != 1 || args[0] != "up" {
		printErr(stderr, usage)
		return 2
	}

	// config.Load also reads GATEWAY_ADDR, REDIS_ADDR, the rail settings
	// and the webhook bound, rate and burst, but only DATABASE_URL is
	// required, so it fits here unchanged. An invalid rail or webhook
	// setting fails the migration too, which is fine: the same environment
	// would stop the gateway. (The bound's check against the pool size is
	// in cmd/gateway only, so migrate doesn't make that one.)
	cfg, err := config.Load(getenv)
	if err != nil {
		printErr(stderr, "migrate: "+err.Error()+"\n")
		return 1
	}
	log := httpx.NewLogger(stdout, cfg.LogLevel)

	// Ctrl-C cancels the run; goose then releases its lock on a detached
	// context, and PostgreSQL drops it anyway when the session ends.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()

	// Fixed messages: the DSN, and pgx's parse error about it, may contain
	// the password (OpenPool only returns its sentinels).
	pool, err := store.OpenPool(ctx, cfg.DatabaseURL)
	if errors.Is(err, store.ErrInvalidDSN) {
		printErr(stderr, "migrate: invalid DATABASE_URL\n")
		return 1
	}
	if err != nil {
		printErr(stderr, "migrate: could not set up the PostgreSQL pool\n")
		return 1
	}
	defer pool.Close()
	db := stdlib.OpenDBFromPool(pool) // goose works over database/sql
	defer func() { _ = db.Close() }()

	start := time.Now()
	log.Info("migrate up starting")
	// pgx connection errors name the host, user and database but never the
	// password, so the error text is safe to log.
	if err := store.Up(ctx, db); err != nil {
		log.Error("migrate up failed", "error", err)
		return 1
	}
	log.Info("migrate up done", "duration_ms", time.Since(start).Milliseconds())
	return 0
}

// printErr writes msg to stderr. A failed write has nowhere better to be
// reported, and the exit code already carries the outcome.
func printErr(stderr io.Writer, msg string) {
	_, _ = io.WriteString(stderr, msg)
}
