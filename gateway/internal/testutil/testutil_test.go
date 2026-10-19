package testutil_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/svout/strata/gateway/internal/testutil"
)

// Two clients held at the same time must be on different logical databases
// and must not see each other's keys.
func TestRedisClientsAreIsolated(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	a, b := testutil.Redis(t), testutil.Redis(t)

	if a.Options().DB == b.Options().DB {
		t.Fatalf("both clients got Redis db %d", a.Options().DB)
	}
	if err := a.Set(ctx, "k", "from-a", 0).Err(); err != nil {
		t.Fatalf("set: %v", err)
	}
	if n, err := b.Exists(ctx, "k").Result(); err != nil || n != 0 {
		t.Errorf("b sees a's key: exists=%d err=%v", n, err)
	}
}

// Each test gets an empty database even when it reuses one an earlier test
// wrote to. 17 sequential subtests over 16 databases guarantee at least one
// reuse. Not parallel, so no other test holds a database meanwhile.
func TestRedisDatabaseIsEmptyOnEntry(t *testing.T) {
	for range 17 {
		t.Run("", func(t *testing.T) {
			ctx := testContext(t)
			c := testutil.Redis(t)
			if n, err := c.DBSize(ctx).Result(); err != nil || n != 0 {
				t.Fatalf("db %d not empty on entry: size=%d err=%v", c.Options().DB, n, err)
			}
			if err := c.Set(ctx, "leftover", "x", 0).Err(); err != nil {
				t.Fatalf("set: %v", err)
			}
		})
	}
}

// Two pools from Postgres(t) are on different databases, each migrated to
// schema version 1.
func TestPostgresDatabasesAreSeparateAndMigrated(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	a, b := testutil.Postgres(t), testutil.Postgres(t)

	if nameA, nameB := currentDatabase(ctx, t, a), currentDatabase(ctx, t, b); nameA == nameB {
		t.Fatalf("both pools are on database %s", nameA)
	}
	for _, pool := range []*pgxpool.Pool{a, b} {
		var version int64
		if err := pool.QueryRow(ctx, "SELECT max(version_id) FROM goose_db_version").Scan(&version); err != nil {
			t.Fatalf("read goose version: %v", err)
		}
		if version != 1 {
			t.Errorf("schema version = %d, want 1", version)
		}
	}
}

func currentDatabase(ctx context.Context, t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var name string
	if err := pool.QueryRow(ctx, "SELECT current_database()").Scan(&name); err != nil {
		t.Fatalf("current_database: %v", err)
	}
	return name
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}
