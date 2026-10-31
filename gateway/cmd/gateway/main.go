// Command gateway runs the AgentGate payment gateway.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/svout/strata/gateway/internal/config"
	"github.com/svout/strata/gateway/internal/httpx"
	"github.com/svout/strata/gateway/internal/idem"
	"github.com/svout/strata/gateway/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gateway:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	log := httpx.NewLogger(os.Stdout, cfg.LogLevel)

	// Both clients connect lazily: the gateway starts even if PostgreSQL or
	// Redis is down, and /readyz reports it until they are up. Setup errors
	// are reported with a fixed message, since the DSN or its error text
	// may contain credentials (OpenPool only returns its sentinels).
	pool, err := store.OpenPool(context.Background(), cfg.DatabaseURL)
	if errors.Is(err, store.ErrInvalidDSN) {
		return errors.New("invalid DATABASE_URL")
	}
	if err != nil {
		return errors.New("could not set up the PostgreSQL pool")
	}
	defer pool.Close()
	redis.SetLogger(redisLogger{log})
	// ContextTimeoutEnabled makes the client honour context deadlines on
	// network I/O, so the idempotency lock's 50 ms per call holds.
	rdb := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr, ContextTimeoutEnabled: true})
	defer func() { _ = rdb.Close() }()

	gw, err := compose(cfg, pool, log,
		httpx.ReadyCheck{Name: "postgres", Check: pool.Ping},
		httpx.ReadyCheck{Name: "redis", Check: func(ctx context.Context) error { return rdb.Ping(ctx).Err() }})
	if err != nil {
		log.LogAttrs(context.Background(), slog.LevelError, "invalid configuration; the gateway will exit",
			slog.String("error", err.Error()))
		return err
	}
	locker := idem.NewRedisLocker(rdb, log)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           gw.router,
		ErrorLog:          httpx.NewServerErrorLog(log),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Info("gateway listening", "addr", cfg.Addr)
		errCh <- srv.ListenAndServe()
	}()

	// Wiring and recovery run in the background until shutdown. Deferred
	// calls run last-first: cancel the context, wait for the goroutine to
	// stop, and only then close the pool it uses (deferred above).
	var wg sync.WaitGroup
	defer wg.Wait()
	wireCtx, cancelWire := context.WithCancel(ctx)
	defer cancelWire()
	wireErr := make(chan error, 1)
	// gw.wired is passed as compose decided it. This one assignment is the
	// only part of the bound's path no test reaches (compose and
	// wireSettlement are both tested through it); a deliberate stopping
	// point, since it has no branches (Ivan, 2026-10-31).
	wg.Go(func() {
		if err := wireSettlement(wireCtx, pool, gw.wired, locker, log, gw.pay, gw.hooks, defaultTimings); err != nil {
			wireErr <- err
		}
	})

	var exitErr error
	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case exitErr = <-wireErr:
		// A configuration error: stop serving and exit with it.
	case <-ctx.Done():
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return errors.Join(exitErr, srv.Shutdown(shutdownCtx))
}

// gateway is what run serves: the router with both APIs mounted behind their
// gates, and the wiring wireSettlement opens the gates with.
type gateway struct {
	router     http.Handler
	wired      wiring
	pay, hooks *gate
	limiter    *httpx.RateLimiter
}

// compose builds the gateway from cfg and the pool, with checks as the
// readiness checks besides "rail". It is the composition run uses, kept out
// of run so tests can call it: run keeps only the process (signals, the
// server, exit codes). The webhook bound is decided here, before serving,
// from the pool's size (ADR-127; pool.Config() opens no connection), so a
// bound over half the pool is an errConfig error and the gateway exits,
// and the bound wireSettlement passes to payments.New is never 0.
func compose(cfg config.Config, pool *pgxpool.Pool, log *slog.Logger, checks ...httpx.ReadyCheck) (gateway, error) {
	wired, err := wiringFor(cfg, pool.Config().MaxConns)
	if err != nil {
		return gateway{}, err
	}
	// Both APIs are behind gates until the rail is wired (wire.go). Only
	// the payments gate is a readiness check. wireSettlement opens the
	// webhook gate first, so a ready gateway serves both. Between the two
	// sets, webhooks are already served while /readyz is not ready and
	// payments still answer 503; that is harmless, because the Service
	// behind both gates is fully built before either opens. The order isn't
	// pinned by a test (Ivan, 2026-10-31).
	gw := gateway{wired: wired, pay: newGate(payGateTitle), hooks: newGate(hookGateTitle),
		limiter: httpx.NewRateLimiter(cfg.WebhookRate, cfg.WebhookBurst, log)}
	r := httpx.NewRouter(log, append(checks, gw.pay.readyCheck())...)
	mountAPIs(r, gw.pay, gw.hooks, gw.limiter)
	gw.router = r
	return gw, nil
}

// The gates' 503 titles, one per API, so neither tells its callers about
// the other.
const (
	payGateTitle  = "Payments are not available yet; retry later"
	hookGateTitle = "Rail webhooks are not available yet; retry later"
)

// mountAPIs mounts the gateway's APIs on r. It is the one place they are
// mounted; the end-to-end tests call it too, so they test this order.
//
// The rail webhook's chain is limiter → gate → payments.Webhook, with the
// limiter outermost: a flood is refused before anything else runs, gate
// closed or not. The webhook is unauthenticated (ADR-124), so the limiter
// is load-bearing twice over: for the rail's quota, and for log volume. A
// failing webhook logs one Error line per request, and only the limiter's
// per-source rate (with its overflow bucket for the many-source case)
// bounds how many lines a caller can cause (Ivan, 2026-10-31). Moving the
// limiter inward, exempting a webhook route from it, or raising the rate
// far beyond its default of 20/s reopens an unbounded log flood on an
// unauthenticated endpoint. TestMountedAPIs_LimiterBeforeGate pins the
// order. /v1/payments has no limiter: P1's payments API has no per-source
// limit, and a test pins that too.
func mountAPIs(r chi.Router, pay, hooks http.Handler, limiter *httpx.RateLimiter) {
	r.Mount("/v1/payments", pay)
	r.Mount("/v1/rail/webhooks", limiter.Middleware(hooks))
}

// redisLogger replaces go-redis's own logger, which prints plain-text lines to
// stderr outside the redacting JSON logger (they can include addresses or
// connection details). Its messages become one fixed debug line; Redis
// availability is reported by /readyz.
type redisLogger struct{ log *slog.Logger }

func (l redisLogger) Printf(ctx context.Context, _ string, _ ...any) {
	l.log.LogAttrs(ctx, slog.LevelDebug, "redis client message suppressed")
}
