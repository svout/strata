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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/svout/strata/gateway/internal/rail"
	"github.com/svout/strata/gateway/internal/testutil"
)

// env returns a getenv over m.
func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// unreachable is a DSN whose server refuses at once (port 1), so a test
// that must not connect, or that connects and fails, needs no Docker.
const unreachable = "postgres://u:p@127.0.0.1:1/db?sslmode=disable&connect_timeout=2"

func TestRun_UsageErrors(t *testing.T) {
	id := uuid.New()
	ref := rail.Ref(id)
	tests := []struct {
		name string
		args []string
	}{
		{"no command", nil},
		{"unknown command", []string{"settle", "--ref", ref}},
		{"missing ref", []string{"pend"}},
		{"malformed ref", []string{"pend", "--ref", "pay_not-a-uuid"}},
		{"upper-case uuid", []string{"pend", "--ref", "pay_" + strings.ToUpper(id.String())}},
		{"help", []string{"pend", "-h"}},
		{"extra argument", []string{"pend", "--ref", ref, "now"}},
		{"pend takes no outcome", []string{"pend", "--ref", ref, "--outcome", "settled"}},
		{"missing outcome", []string{"resolve", "--ref", ref}},
		{"pending is not a resolution", []string{"resolve", "--ref", ref, "--outcome", "pending"}},
		{"reason without a decline", []string{"resolve", "--ref", ref, "--outcome", "settled", "--reason", "x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(tt.args, env(map[string]string{"DATABASE_URL": unreachable}), &stdout, &stderr); code != exitUsage {
				t.Errorf("exit code = %d, want %d", code, exitUsage)
			}
			if !strings.Contains(stderr.String(), "usage: fakepsp pend") {
				t.Errorf("stderr = %q, want usage", stderr.String())
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want empty", stdout.String())
			}
		})
	}
}

func TestRun_MissingDatabaseURL(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"pend", "--ref", rail.Ref(uuid.New())}, env(nil), &stdout, &stderr); code != exitFailed {
		t.Errorf("exit code = %d, want %d", code, exitFailed)
	}
	if want := "fakepsp: DATABASE_URL is required\n"; stderr.String() != want {
		t.Errorf("stderr = %q, want %q", stderr.String(), want)
	}
}

// TestRun_RefusesProduction: STRATA_ENV=production is refused before any
// connection, with or without the gateway's fake-rail flag. The DSN is
// unreachable, so reaching the database would show as a store error.
func TestRun_RefusesProduction(t *testing.T) {
	for _, flag := range []string{"", "1"} {
		var stdout, stderr bytes.Buffer
		getenv := env(map[string]string{"DATABASE_URL": unreachable, "STRATA_ENV": "production", "GATEWAY_ALLOW_FAKE_RAIL": flag})
		code := run([]string{"pend", "--ref", rail.Ref(uuid.New())}, getenv, &stdout, &stderr)
		if code != exitFailed {
			t.Errorf("flag %q: exit code = %d, want %d", flag, code, exitFailed)
		}
		if !strings.Contains(stderr.String(), "production") || strings.Contains(stderr.String(), "store") {
			t.Errorf("flag %q: stderr = %q, want a production refusal and no store error", flag, stderr.String())
		}
	}
}

// TestRun_NeverPrintsPassword is SPEC-002 AC8 for this command: whether the
// DSN fails to parse or the server is unreachable, no part of the DSN
// reaches stdout or stderr.
func TestRun_NeverPrintsPassword(t *testing.T) {
	const secret = "s3cr3t-Pa55word"
	tests := []struct {
		name string
		dsn  string
	}{
		{"malformed", "postgres://leakuser:" + secret + "@[bad"},
		{"unreachable", "postgres://leakuser:" + secret + "@127.0.0.1:1/leakdb?sslmode=disable&connect_timeout=2"},
		{"unreachable key-value", "host=127.0.0.1 port=1 user=leakuser password=" + secret + " dbname=leakdb sslmode=disable connect_timeout=2"},
	}
	ref := rail.Ref(uuid.New())
	for _, tt := range tests {
		for _, args := range [][]string{{"pend", "--ref", ref}, {"resolve", "--ref", ref, "--outcome", "settled"}} {
			t.Run(tt.name+"/"+args[0], func(t *testing.T) {
				var stdout, stderr bytes.Buffer
				if code := run(args, env(map[string]string{"DATABASE_URL": tt.dsn}), &stdout, &stderr); code != exitFailed {
					t.Errorf("exit code = %d, want %d", code, exitFailed)
				}
				if stdout.Len()+stderr.Len() == 0 {
					t.Fatal("no output at all; the failure should be reported")
				}
				for name, out := range map[string]string{"stdout": stdout.String(), "stderr": stderr.String()} {
					for _, leak := range []string{secret, "leakuser", "leakdb", "127.0.0.1"} {
						if strings.Contains(out, leak) {
							t.Errorf("%s contains %q: %q", name, leak, out)
						}
					}
				}
			})
		}
	}
}

// TestRun_PendThenResolve drives both commands against a real database, as
// the chaos run does from outside the gateway: the fake on the gateway's
// side reads what this process wrote.
func TestRun_PendThenResolve(t *testing.T) {
	pool := testutil.Postgres(t)
	getenv := env(map[string]string{"DATABASE_URL": dsnOf(pool)})
	fake := rail.NewFake(pool)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	do := func(t *testing.T, want int, args ...string) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := run(args, getenv, &stdout, &stderr); code != want {
			t.Fatalf("%v: exit code = %d, want %d; stderr = %q", args, code, want, stderr.String())
		}
		return stdout.String() + stderr.String()
	}
	status := func(t *testing.T, ref string) rail.Result {
		t.Helper()
		res, err := fake.Status(ctx, ref)
		if err != nil {
			t.Fatalf("Status(%s): %v", ref, err)
		}
		return res
	}
	moves := func(t *testing.T, ref string) int {
		t.Helper()
		n, err := fake.Moves(ctx, ref)
		if err != nil {
			t.Fatalf("Moves(%s): %v", ref, err)
		}
		return n
	}

	t.Run("settled", func(t *testing.T) {
		ref := rail.Ref(uuid.New())
		do(t, exitOK, "pend", "--ref", ref)
		if got := status(t, ref); got.Outcome != rail.Pending || moves(t, ref) != 0 {
			t.Fatalf("after pend: %+v, moves %d; want pending, 0", got, moves(t, ref))
		}
		do(t, exitOK, "pend", "--ref", ref) // pending again is success, and takes no step
		do(t, exitOK, "resolve", "--ref", ref, "--outcome", "settled")
		if got := status(t, ref); got.Outcome != rail.Settled || moves(t, ref) != 1 {
			t.Fatalf("after resolve: %+v, moves %d; want settled, 1", got, moves(t, ref))
		}
		// Out of order: a second resolution, either way, and a late pend
		// change nothing.
		if out := do(t, exitNotPending, "resolve", "--ref", ref, "--outcome", "declined"); !strings.Contains(out, "not pending") {
			t.Errorf("second resolve said %q", out)
		}
		do(t, exitNotPending, "resolve", "--ref", ref, "--outcome", "settled")
		if out := do(t, exitNotPending, "pend", "--ref", ref); !strings.Contains(out, "already settled") {
			t.Errorf("late pend said %q", out)
		}
		if got := status(t, ref); got.Outcome != rail.Settled || moves(t, ref) != 1 {
			t.Fatalf("at the end: %+v, moves %d; want settled, 1", got, moves(t, ref))
		}
	})

	t.Run("declined", func(t *testing.T) {
		ref := rail.Ref(uuid.New())
		do(t, exitOK, "pend", "--ref", ref)
		do(t, exitOK, "resolve", "--ref", ref, "--outcome", "declined", "--reason", "insufficient_funds")
		do(t, exitNotPending, "resolve", "--ref", ref, "--outcome", "settled")
		got := status(t, ref)
		if got.Outcome != rail.Declined || got.Reason != "insufficient_funds" || moves(t, ref) != 0 {
			t.Fatalf("got %+v, moves %d; want declined (insufficient_funds), 0", got, moves(t, ref))
		}
	})

	// The chaos run declines without --reason: the fake's default applies.
	t.Run("declined, default reason", func(t *testing.T) {
		ref := rail.Ref(uuid.New())
		do(t, exitOK, "pend", "--ref", ref)
		do(t, exitOK, "resolve", "--ref", ref, "--outcome", "declined")
		if got := status(t, ref); got.Outcome != rail.Declined || got.Reason != rail.DefaultDeclineReason {
			t.Fatalf("got %+v, want declined (%s)", got, rail.DefaultDeclineReason)
		}
	})

	t.Run("unknown ref", func(t *testing.T) {
		do(t, exitUnknownRef, "resolve", "--ref", rail.Ref(uuid.New()), "--outcome", "settled")
	})
}

// dsnOf rebuilds the DSN of testutil's database from the pool's config
// (pool.Config().ConnString() is the admin string, not this database). A
// copy of cmd/migrate's test code; a third copy moves it into testutil.
func dsnOf(pool *pgxpool.Pool) string {
	cc := pool.Config().ConnConfig
	return (&url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(cc.User, cc.Password),
		Host:     net.JoinHostPort(cc.Host, strconv.Itoa(int(cc.Port))),
		Path:     "/" + cc.Database,
		RawQuery: "sslmode=disable",
	}).String()
}
