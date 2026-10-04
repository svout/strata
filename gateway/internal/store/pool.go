package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"
)

// OpenPool's errors are fixed sentinels, never pgx's own error: a DSN, and
// pgx's error text about it, can contain the password. Callers map them with
// errors.Is to their own messages.
var (
	// ErrInvalidDSN means the DSN could not be parsed.
	ErrInvalidDSN = errors.New("invalid PostgreSQL DSN")
	// ErrPoolSetup means the pool could not be created from a valid DSN.
	ErrPoolSetup = errors.New("could not set up the PostgreSQL pool")
)

// OpenPool parses dsn and creates a pgx pool with pgx's default settings. It
// does not connect or ping: connections are opened lazily on first use, so
// a process can start while PostgreSQL is down (the gateway's /readyz reports
// it). The pool is shared by everything in the process; callers Close it.
func OpenPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, ErrInvalidDSN
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, ErrPoolSetup
	}
	return pool, nil
}
