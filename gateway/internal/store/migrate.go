// Package store owns the gateway's PostgreSQL schema, the code that migrates
// it, pool setup (OpenPool) and the transaction helper (Serializable).
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"

	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"
	"github.com/pressly/goose/v3/lock"
)

// The SQL files are compiled into the binary, so a migration run never
// depends on the working directory or on files shipped beside the binary.
//
//go:embed migrations/*.sql
var embedded embed.FS

// Up applies every pending migration to db. db must use a PostgreSQL
// database/sql driver; the gateway uses pgx's stdlib adapter
// (stdlib.OpenDBFromPool or sql.Open("pgx", ...)).
func Up(ctx context.Context, db *sql.DB) error {
	p, err := newProvider(db)
	if err != nil {
		return err
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("store: migrate up: %w", err)
	}
	return nil
}

// Down rolls back every applied migration, leaving an empty schema (goose's
// own version table stays, recording version 0). It exists for tests and
// local resets; production schemas only move forward.
func Down(ctx context.Context, db *sql.DB) error {
	p, err := newProvider(db)
	if err != nil {
		return err
	}
	if _, err := p.DownTo(ctx, 0); err != nil {
		return fmt.Errorf("store: migrate down: %w", err)
	}
	return nil
}

// ErrSchemaVersion means the schema version could not be read. Its text is
// fixed: the driver's error is left out because on a first connection it
// names host, user and database (root CLAUDE.md, error text).
var ErrSchemaVersion = errors.New("store: could not read the schema version")

// SchemaVersion returns the highest migration version applied to db, or 0 if
// nothing has been migrated (no goose version table yet). The gateway reads
// it at startup to tell "not migrated yet" from "migrated and misconfigured"
// (ADR-125 §3).
//
// It only reads. goose's Provider.GetDBVersion would be the obvious call, but
// it creates the version table if it is missing and takes the migration lock,
// so a gateway that called it would write to a database it must never
// migrate (`make dev` doesn't migrate; `make migrate` does). goose's
// database.Store runs the same queries without either side effect.
func SchemaVersion(ctx context.Context, db *sql.DB) (int64, error) {
	s, err := database.NewStore(database.DialectPostgres, goose.DefaultTablename)
	if err != nil {
		return 0, ErrSchemaVersion
	}
	// goose's PostgreSQL store implements the extended interface; the
	// assertion only fails if a goose upgrade drops it, which a test catches.
	st, ok := s.(database.StoreExtender)
	if !ok {
		return 0, ErrSchemaVersion
	}
	exists, err := st.TableExists(ctx, db)
	if err != nil {
		return 0, ErrSchemaVersion
	}
	if !exists {
		return 0, nil
	}
	v, err := st.GetLatestVersion(ctx, db)
	if errors.Is(err, database.ErrVersionNotFound) {
		return 0, nil // the table exists but has no rows
	}
	if err != nil {
		return 0, ErrSchemaVersion
	}
	return v, nil
}

// Lock retry policy for concurrent migration runs. goose's default polls every
// 5 s for up to 5 min; polling every second (goose's minimum) for the same
// 5 min means a waiting run starts within about a second of the holder
// finishing, while still giving up on a lock that is stuck.
const (
	lockRetryPeriodSeconds = 1
	lockRetryAttempts      = 300
)

// newProvider builds a goose Provider rather than using goose's package-level
// functions: those keep the dialect, base FS and logger in globals, which
// would make two tests migrating different databases in parallel race.
//
// The provider takes goose's PostgreSQL session-level advisory lock
// (pg_try_advisory_lock on lock.DefaultLockID, per database) around every
// run, so two migrators started at once (two `make migrate`s, or several
// gateway replicas later) are serialised: the second waits, then finds
// nothing pending. PostgreSQL releases the lock if the session dies, so a
// crashed run cannot leave the database locked.
func newProvider(db *sql.DB) (*goose.Provider, error) {
	// Strip the "migrations/" prefix so goose sees the .sql files at the root.
	fsys, err := fs.Sub(embedded, "migrations")
	if err != nil {
		return nil, fmt.Errorf("store: migrations fs: %w", err)
	}
	locker, err := lock.NewPostgresSessionLocker(lock.WithLockTimeout(lockRetryPeriodSeconds, lockRetryAttempts))
	if err != nil {
		return nil, fmt.Errorf("store: migration lock: %w", err)
	}
	p, err := goose.NewProvider(goose.DialectPostgres, db, fsys, goose.WithSessionLocker(locker))
	if err != nil {
		return nil, fmt.Errorf("store: goose provider: %w", err)
	}
	return p, nil
}
