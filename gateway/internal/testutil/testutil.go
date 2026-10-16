// Package testutil gives integration tests real PostgreSQL and Redis
// instances in throwaway containers (testcontainers-go), so tests never
// depend on `make dev` being up.
//
// Each test package gets one PostgreSQL and one Redis container, started on
// first use and shared by every test in that package. Isolation comes from
// inside the container: every Postgres(t) call gets its own freshly migrated
// database, and every Redis(t) call gets its own logical Redis database. The
// containers are not stopped explicitly; testcontainers' reaper (Ryuk)
// removes them when the test binary exits.
//
// If Docker is unreachable, tests are skipped locally but fail when the CI
// environment variable is set, so CI can never pass by silently skipping.
package testutil

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/svout/strata/gateway/internal/store"
)

// Image tags must match infra/compose/compose.yml, so tests run against the
// same server versions as the local stack.
const (
	postgresImage = "postgres:16-alpine"
	redisImage    = "redis:7-alpine"
)

// Test-only credentials for a container that lives as long as one test
// binary. Not secrets.
const (
	pgUser     = "agentgate"
	pgPassword = "agentgate"
	pgDatabase = "agentgate"
)

const (
	// startTimeout bounds pulling and starting a container (a cold image
	// pull dominates; a warm start is a few seconds).
	startTimeout = 3 * time.Minute
	// setupTimeout bounds per-test work: creating and migrating a database,
	// or flushing a Redis database.
	setupTimeout = 30 * time.Second
	// redisDatabases is Redis's default number of logical databases (0-15).
	redisDatabases = 16
	// testPoolMaxConns caps each test's pool. pgxpool's default is
	// max(4, NumCPU), so on a large machine parallel tests x pool size could
	// exhaust the server's max_connections (raised to pgMaxConnections
	// below). 8 is still enough for a test to drive real concurrency, e.g.
	// SPEC-002 AC3's 100 concurrent requests (subtask 8) queue for a
	// connection, which is what the gateway's own pool would do too.
	testPoolMaxConns = 8
	// pgMaxConnections is the test server's max_connections (the default is
	// 100). With testPoolMaxConns = 8 it leaves room for about 35 parallel
	// tests plus the admin pool.
	pgMaxConnections = "300"
)

// Postgres returns a pool connected to a new database that exists only for
// this test, already migrated to the latest schema with store.Up. The pool is
// closed and the database dropped when the test ends.
func Postgres(t testing.TB) *pgxpool.Pool {
	t.Helper()
	requireDocker(t)
	pg := sharedPostgres(t)

	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()

	// A per-test database rather than per-test schemas or truncation: tests
	// may run in parallel and never see each other's rows, and dropping a
	// database is one statement. The counter makes names unique within this
	// test binary, which is all that matters since the container is ours.
	name := fmt.Sprintf("test_%d", pg.seq.Add(1))
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := pg.admin.Exec(ctx, "CREATE DATABASE "+ident); err != nil {
		t.Fatalf("testutil: create database %s: %v", name, err)
	}

	cfg := pg.cfg.Copy()
	cfg.ConnConfig.Database = name
	cfg.MaxConns = testPoolMaxConns
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("testutil: connect to %s: %v", name, err)
	}
	t.Cleanup(func() {
		// pool.Close waits for every acquired connection to be released, so
		// a test that leaked one (e.g. an unclosed pgx.Rows) would hang here
		// forever. Close in the background and give up after setupTimeout,
		// reporting the leak as a test error.
		closed := make(chan struct{})
		go func() {
			pool.Close()
			close(closed)
		}()
		select {
		case <-closed:
		case <-time.After(setupTimeout):
			t.Errorf("testutil: test leaked a database connection")
		}

		ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
		defer cancel()
		// Drop regardless of the above: FORCE terminates any connection still
		// open to the database (which also lets a stuck Close finish).
		if _, err := pg.admin.Exec(ctx, "DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)"); err != nil {
			t.Errorf("testutil: drop database %s: %v", name, err)
		}
	})

	// goose works over database/sql; OpenDBFromPool wraps the same pool, and
	// closing the wrapper leaves the pool open.
	db := stdlib.OpenDBFromPool(pool)
	defer func() { _ = db.Close() }()
	if err := store.Up(ctx, db); err != nil {
		t.Fatalf("testutil: migrate %s: %v", name, err)
	}
	return pool
}

// Redis returns a client on a logical Redis database reserved for this test
// and empty at the start. The database is flushed and released when the test
// ends. At most 16 tests hold one at a time; a 17th parallel caller waits.
func Redis(t testing.TB) *redis.Client {
	t.Helper()
	requireDocker(t)
	r := sharedRedis(t)

	var index int
	select {
	case index = <-r.free:
	case <-time.After(setupTimeout):
		t.Fatalf("testutil: no free Redis database after %s", setupTimeout)
	}

	opts := *r.opts
	opts.DB = index
	client := redis.NewClient(&opts)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
		defer cancel()
		if err := client.FlushDB(ctx).Err(); err != nil {
			t.Errorf("testutil: flush Redis db %d: %v", index, err)
		}
		_ = client.Close()
		r.free <- index
	})

	// Flush on the way in too, in case an earlier test's cleanup failed.
	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()
	if err := client.FlushDB(ctx).Err(); err != nil {
		t.Fatalf("testutil: flush Redis db %d: %v", index, err)
	}
	return client
}

// --- Docker availability ---------------------------------------------------

var (
	dockerOnce sync.Once
	dockerErr  error
)

// requireDocker skips the test (locally) or fails it (CI) when no Docker
// daemon is reachable. The check runs once per test binary.
func requireDocker(t testing.TB) {
	t.Helper()
	dockerOnce.Do(func() { dockerErr = dockerHealth() })
	if dockerErr == nil {
		return
	}
	if os.Getenv("CI") != "" {
		t.Fatalf("testutil: Docker is required in CI but unreachable: %v", dockerErr)
	}
	t.Skipf("testutil: Docker unreachable, skipping integration test: %v", dockerErr)
}

func dockerHealth() (err error) {
	// testcontainers can panic when no Docker host can be resolved at all;
	// treat that the same as an unhealthy daemon.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("docker provider panicked: %v", r)
		}
	}()
	p, err := testcontainers.NewDockerProvider()
	if err != nil {
		return err
	}
	defer func() { _ = p.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return p.Health(ctx)
}

// --- Shared containers -----------------------------------------------------

type postgresServer struct {
	cfg   *pgxpool.Config // template config; each test copies it and sets Database
	admin *pgxpool.Pool   // connected to pgDatabase; runs CREATE/DROP DATABASE
	seq   atomic.Int64    // names per-test databases
}

var (
	pgOnce   sync.Once
	pgShared *postgresServer
	pgErr    error
)

// sharedPostgres starts this package's PostgreSQL container on first use.
// A start failure is remembered, so every later test fails fast with the same
// error instead of retrying a slow start.
func sharedPostgres(t testing.TB) *postgresServer {
	t.Helper()
	pgOnce.Do(func() { pgShared, pgErr = startPostgres() })
	if pgErr != nil {
		t.Fatalf("testutil: start PostgreSQL: %v", pgErr)
	}
	return pgShared
}

func startPostgres() (*postgresServer, error) {
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()

	c, err := tcpostgres.Run(ctx, postgresImage,
		tcpostgres.WithUsername(pgUser),
		tcpostgres.WithPassword(pgPassword),
		tcpostgres.WithDatabase(pgDatabase),
		tcpostgres.BasicWaitStrategies(),
		// Durability is pointless for a throwaway database; turning it off
		// makes CREATE DATABASE and every commit noticeably faster.
		testcontainers.WithCmdArgs("-c", "fsync=off", "-c", "synchronous_commit=off", "-c", "full_page_writes=off",
			// Many parallel tests, each with its own pool, share this one
			// server; see testPoolMaxConns.
			"-c", "max_connections="+pgMaxConnections),
	)
	if err != nil {
		return nil, err
	}
	dsn, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return nil, err
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		// pgx redacts the password in parse errors, but the DSN itself is
		// never formatted into a message here.
		return nil, err
	}
	admin, err := pgxpool.NewWithConfig(ctx, cfg.Copy())
	if err != nil {
		return nil, err
	}
	return &postgresServer{cfg: cfg, admin: admin}, nil
}

type redisServer struct {
	opts *redis.Options
	free chan int // logical database indexes not currently held by a test
}

var (
	redisOnce   sync.Once
	redisShared *redisServer
	redisErr    error
)

// sharedRedis starts this package's Redis container on first use.
func sharedRedis(t testing.TB) *redisServer {
	t.Helper()
	redisOnce.Do(func() { redisShared, redisErr = startRedis() })
	if redisErr != nil {
		t.Fatalf("testutil: start Redis: %v", redisErr)
	}
	return redisShared
}

func startRedis() (*redisServer, error) {
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()

	c, err := tcredis.Run(ctx, redisImage)
	if err != nil {
		return nil, err
	}
	url, err := c.ConnectionString(ctx)
	if err != nil {
		return nil, err
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, err
	}
	opts.ContextTimeoutEnabled = true // as in cmd/gateway, so deadlines hold
	free := make(chan int, redisDatabases)
	for i := range redisDatabases {
		free <- i
	}
	return &redisServer{opts: opts, free: free}, nil
}
