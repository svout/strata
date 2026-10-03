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
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/svout/agentgate/gateway/internal/config"
	"github.com/svout/agentgate/gateway/internal/httpx"
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
	// Redis is down, and /readyz reports it until they are up. Parse errors
	// are reported with a fixed message, since the DSN or its error text
	// may contain credentials.
	pgCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return errors.New("invalid DATABASE_URL")
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), pgCfg)
	if err != nil {
		return errors.New("could not set up the PostgreSQL pool")
	}
	defer pool.Close()
	redis.SetLogger(redisLogger{log})
	rdb := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr})
	defer func() { _ = rdb.Close() }()

	checks := []httpx.ReadyCheck{
		{Name: "postgres", Check: pool.Ping},
		{Name: "redis", Check: func(ctx context.Context) error { return rdb.Ping(ctx).Err() }},
	}

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           httpx.NewRouter(log, checks...),
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

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// redisLogger replaces go-redis's own logger, which prints plain-text lines to
// stderr outside the redacting JSON logger (they can include addresses or
// connection details). Its messages become one fixed debug line; Redis
// availability is reported by /readyz.
type redisLogger struct{ log *slog.Logger }

func (l redisLogger) Printf(ctx context.Context, _ string, _ ...any) {
	l.log.LogAttrs(ctx, slog.LevelDebug, "redis client message suppressed")
}
