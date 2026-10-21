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

	// The payments API is behind a gate until its rail is wired (wire.go).
	payGate := &gate{}
	checks := []httpx.ReadyCheck{
		{Name: "postgres", Check: pool.Ping},
		{Name: "redis", Check: func(ctx context.Context) error { return rdb.Ping(ctx).Err() }},
		payGate.readyCheck(),
	}

	router := httpx.NewRouter(log, checks...)
	locker := idem.NewRedisLocker(rdb, log)
	router.Mount("/v1/payments", payGate)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           router,
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
	wg.Go(func() {
		if err := wireSettlement(wireCtx, pool, cfg.AllowFakeRail, locker, log, payGate, defaultTimings); err != nil {
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

// redisLogger replaces go-redis's own logger, which prints plain-text lines to
// stderr outside the redacting JSON logger (they can include addresses or
// connection details). Its messages become one fixed debug line; Redis
// availability is reported by /readyz.
type redisLogger struct{ log *slog.Logger }

func (l redisLogger) Printf(ctx context.Context, _ string, _ ...any) {
	l.log.LogAttrs(ctx, slog.LevelDebug, "redis client message suppressed")
}
