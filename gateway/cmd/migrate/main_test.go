package main

import (
	"bytes"
	"context"
	"net"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/stdlib"

	"github.com/svout/strata/gateway/internal/store"
	"github.com/svout/strata/gateway/internal/testutil"
)

// env returns a getenv over m.
func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestRun_UsageErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"no command", nil},
		{"unknown command", []string{"down"}},
		{"extra argument", []string{"up", "now"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tt.args, env(map[string]string{"DATABASE_URL": "postgres://u@127.0.0.1:1/db"}), &stdout, &stderr)
			if code != 2 {
				t.Errorf("exit code = %d, want 2", code)
			}
			if !strings.HasPrefix(stderr.String(), "usage: migrate up") {
				t.Errorf("stderr = %q, want usage", stderr.String())
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want empty", stdout.String())
			}
		})
	}
}

func TestRun_ConfigErrors(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"missing DATABASE_URL", nil, "migrate: DATABASE_URL is required\n"},
		{"malformed DATABASE_URL", map[string]string{"DATABASE_URL": "postgres://u:p@[bad"}, "migrate: invalid DATABASE_URL\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run([]string{"up"}, env(tt.env), &stdout, &stderr); code != 1 {
				t.Errorf("exit code = %d, want 1", code)
			}
			if stderr.String() != tt.want {
				t.Errorf("stderr = %q, want %q", stderr.String(), tt.want)
			}
		})
	}
}

// TestRun_NeverPrintsPassword is SPEC-002 AC8 for this command: whether the
// DSN fails to parse or the server is unreachable (port 1 refuses at once, no
// Docker needed), the password never reaches stdout or stderr.
func TestRun_NeverPrintsPassword(t *testing.T) {
	const secret = "s3cr3t-Pa55word"
	tests := []struct {
		name string
		dsn  string
	}{
		{"malformed", "postgres://agentgate:" + secret + "@[bad"},
		{"bad port", "postgres://agentgate:" + secret + "@127.0.0.1:notaport/agentgate"},
		{"unreachable", "postgres://agentgate:" + secret + "@127.0.0.1:1/agentgate?sslmode=disable&connect_timeout=2"},
		{"unreachable key-value", "host=127.0.0.1 port=1 user=agentgate password=" + secret + " dbname=agentgate sslmode=disable connect_timeout=2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			getenv := env(map[string]string{"DATABASE_URL": tt.dsn, "LOG_LEVEL": "debug"})
			if code := run([]string{"up"}, getenv, &stdout, &stderr); code != 1 {
				t.Errorf("exit code = %d, want 1", code)
			}
			if stdout.Len()+stderr.Len() == 0 {
				t.Fatal("no output at all; the failure should be reported")
			}
			for name, out := range map[string]string{"stdout": stdout.String(), "stderr": stderr.String()} {
				if strings.Contains(out, secret) {
					t.Errorf("%s contains the password: %q", name, out)
				}
			}
		})
	}
}

// TestRun_UpAppliesThenNoOps drives the whole success path (config, pool,
// store.Up) against a real database: the first run applies every migration
// (versions 1 and 2), the second has nothing to do, and both exit 0.
func TestRun_UpAppliesThenNoOps(t *testing.T) {
	pool := testutil.Postgres(t) // already migrated; roll back to start empty
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	db := stdlib.OpenDBFromPool(pool)
	defer func() { _ = db.Close() }()
	if err := store.Down(ctx, db); err != nil {
		t.Fatalf("store.Down: %v", err)
	}

	// The DSN of testutil's database, rebuilt from the pool's config.
	cc := pool.Config().ConnConfig
	dsn := (&url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(cc.User, cc.Password),
		Host:     net.JoinHostPort(cc.Host, strconv.Itoa(int(cc.Port))),
		Path:     "/" + cc.Database,
		RawQuery: "sslmode=disable",
	}).String()
	getenv := env(map[string]string{"DATABASE_URL": dsn})

	for i := 1; i <= 2; i++ {
		var stdout, stderr bytes.Buffer
		if code := run([]string{"up"}, getenv, &stdout, &stderr); code != 0 {
			t.Fatalf("run %d: exit code = %d, want 0; stderr = %q, stdout = %q", i, code, stderr.String(), stdout.String())
		}
		if !strings.Contains(stdout.String(), `"msg":"migrate up done"`) {
			t.Errorf("run %d: stdout = %q, want a \"migrate up done\" line", i, stdout.String())
		}
	}

	for _, v := range []int{1, 2} {
		var applied int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM goose_db_version WHERE version_id = $1 AND is_applied", v).Scan(&applied); err != nil {
			t.Fatalf("read goose versions: %v", err)
		}
		if applied != 1 {
			t.Errorf("version %d applied %d times since Down, want 1", v, applied)
		}
	}
}
