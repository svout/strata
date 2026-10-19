package idem

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/svout/strata/gateway/internal/store"
	"github.com/svout/strata/gateway/internal/testutil"
)

var base = Request{Caller: "", Method: "POST", Path: "/v1/payments", Body: []byte(`{"amount":1}`)}

// ADR-118: the hash covers caller, method, path and body, and its encoding
// is unambiguous.
func TestHash_CoversEveryField(t *testing.T) {
	if Hash(base) != Hash(Request{Caller: "", Method: "POST", Path: "/v1/payments", Body: []byte(`{"amount":1}`)}) {
		t.Fatal("same request, different hash")
	}
	for name, r := range map[string]Request{
		"caller": {Caller: "agent-b", Method: base.Method, Path: base.Path, Body: base.Body},
		"method": {Caller: base.Caller, Method: "PUT", Path: base.Path, Body: base.Body},
		"path":   {Caller: base.Caller, Method: base.Method, Path: "/v1/refunds", Body: base.Body},
		"body":   {Caller: base.Caller, Method: base.Method, Path: base.Path, Body: []byte(`{"amount":2}`)},
		// Without length prefixes these two would concatenate identically.
		"boundary": {Caller: base.Caller, Method: "POST/v1", Path: "/payments", Body: base.Body},
	} {
		if Hash(r) == Hash(base) {
			t.Errorf("%s: changing it does not change the hash", name)
		}
	}
}

func TestParseKey(t *testing.T) {
	for _, bad := range []string{"", strings.Repeat("k", MaxKeyLen+1)} {
		if _, err := ParseKey("", bad); !errors.Is(err, ErrMissingKey) {
			t.Errorf("ParseKey(%d bytes) = %v, want ErrMissingKey", len(bad), err)
		}
	}
	k, err := ParseKey("agent-a", strings.Repeat("k", MaxKeyLen))
	if err != nil || k.Scope != "agent-a" || len(k.Value) != MaxKeyLen {
		t.Fatalf("ParseKey(255 bytes) = %+v, %v", k, err)
	}
}

func TestDigest_StableAndHidesKey(t *testing.T) {
	k := Key{Value: "sk_live_digest_secret"}
	if d := k.Digest(); d != k.Digest() || len(d) != 32 || strings.Contains(d, "secret") {
		t.Fatalf("Digest = %q", d)
	}
	if (Key{Scope: "a", Value: "x"}).Digest() == (Key{Scope: "b", Value: "x"}).Digest() {
		t.Fatal("digest ignores the scope")
	}
}

func TestClaim_Lifecycle(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	ctx := t.Context()
	key := Key{Value: "k-1"}
	h := Hash(base)
	resp := Response{Status: 201, Body: []byte(`{"id":"x"}`)}

	// First use: claimed, then completed.
	inTx(t, pool, func(tx pgx.Tx) error {
		if _, found, err := Claim(ctx, tx, key, h); err != nil || found {
			t.Fatalf("first Claim = found %v, %v", found, err)
		}
		return Complete(ctx, tx, key, h, resp)
	})

	// Same request: the stored response, byte for byte.
	inTx(t, pool, func(tx pgx.Tx) error {
		got, found, err := Claim(ctx, tx, key, h)
		if err != nil || !found || got.Status != 201 || string(got.Body) != string(resp.Body) {
			t.Fatalf("replay Claim = %+v, %v, %v", got, found, err)
		}
		return nil
	})

	// Ivan's case: same key and body on another route is not a replay.
	other := base
	other.Path = "/v1/refunds"
	for name, r := range map[string]Request{"route": other, "body": {Method: "POST", Path: "/v1/payments", Body: []byte(`{"amount":2}`)}} {
		inTx(t, pool, func(tx pgx.Tx) error {
			if _, found, err := Claim(ctx, tx, key, Hash(r)); !errors.Is(err, ErrMismatch) || found {
				t.Fatalf("%s: Claim = found %v, %v; want ErrMismatch", name, found, err)
			}
			return nil
		})
	}

	// Another caller's scope is a different key altogether.
	inTx(t, pool, func(tx pgx.Tx) error {
		if _, found, err := Claim(ctx, tx, Key{Scope: "agent-b", Value: "k-1"}, h); err != nil || found {
			t.Fatalf("other scope: found %v, %v; want a fresh claim", found, err)
		}
		return nil
	})
}

// A claim whose transaction rolls back leaves nothing: the key is free.
func TestClaim_RollbackLeavesNoKey(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	ctx := t.Context()
	key := Key{Value: "k-rollback"}
	boom := errors.New("effect failed")

	err := store.Serializable(ctx, pool, func(tx pgx.Tx) error {
		if _, _, err := Claim(ctx, tx, key, Hash(base)); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Serializable = %v, want the effect's error", err)
	}
	if n := count(t, pool); n != 0 {
		t.Fatalf("keys after rollback = %d, want 0", n)
	}
}

// Past Retention a key is free again, even for a different request: the
// claim overwrites it in its own transaction, without a sweeper.
func TestClaim_ExpiredKeyIsReclaimed(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	ctx := t.Context()
	key := Key{Value: "k-old"}
	inTx(t, pool, func(tx pgx.Tx) error {
		if _, _, err := Claim(ctx, tx, key, Hash(base)); err != nil {
			return err
		}
		return Complete(ctx, tx, key, Hash(base), Response{Status: 201, Body: []byte(`{}`)})
	})
	if _, err := pool.Exec(ctx, "UPDATE idempotency_keys SET created_at = now() - interval '25 hours'"); err != nil {
		t.Fatal(err)
	}

	other := base
	other.Body = []byte(`{"amount":9}`)
	inTx(t, pool, func(tx pgx.Tx) error {
		if _, found, err := Claim(ctx, tx, key, Hash(other)); err != nil || found {
			t.Fatalf("expired key: found %v, %v; want a fresh claim", found, err)
		}
		return Complete(ctx, tx, key, Hash(other), Response{Status: 201, Body: []byte(`{"new":true}`)})
	})
	var fresh bool
	if err := pool.QueryRow(ctx, "SELECT created_at > now() - interval '1 minute' FROM idempotency_keys").Scan(&fresh); err != nil || !fresh || count(t, pool) != 1 {
		t.Fatalf("reclaimed row not refreshed (fresh=%v, %v)", fresh, err)
	}
}

// Two claims racing on one expired key: the first reclaims it; the second,
// which also read the expired row, blocks on the first's UPDATE, gets a
// serialization failure, and on retry replays the fresh response.
func TestClaim_ExpiredKeyRace(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	ctx := t.Context()
	key := Key{Value: "k-race"}
	h := Hash(base)
	inTx(t, pool, func(tx pgx.Tx) error {
		if _, _, err := Claim(ctx, tx, key, h); err != nil {
			return err
		}
		return Complete(ctx, tx, key, h, Response{Status: 201, Body: []byte(`{"old":true}`)})
	})
	if _, err := pool.Exec(ctx, "UPDATE idempotency_keys SET created_at = now() - interval '25 hours'"); err != nil {
		t.Fatal(err)
	}

	claimed := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- store.Serializable(ctx, pool, func(tx pgx.Tx) error {
			if _, found, err := Claim(ctx, tx, key, h); err != nil || found {
				return fmt.Errorf("first claim: found %v, %w", found, err)
			}
			select { // signal once; a retried attempt must not close twice
			case <-claimed:
			default:
				close(claimed)
			}
			time.Sleep(200 * time.Millisecond) // hold the row while the second claim starts
			return Complete(ctx, tx, key, h, Response{Status: 201, Body: []byte(`{"new":true}`)})
		})
	}()

	<-claimed
	var got Response
	var found bool
	err := store.Serializable(ctx, pool, func(tx pgx.Tx) error {
		var err error
		got, found, err = Claim(ctx, tx, key, h)
		if err == nil && !found {
			return errors.New("second claim reclaimed the key too")
		}
		return err
	})
	if err := <-firstDone; err != nil {
		t.Fatalf("first: %v", err)
	}
	if err != nil || !found || string(got.Body) != `{"new":true}` {
		t.Fatalf("second claim = %s, found %v, %v; want the first claim's fresh response", got.Body, found, err)
	}
}

// Complete only completes the request that was claimed.
func TestComplete_WrongHashFails(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	ctx := t.Context()
	key := Key{Value: "k-pair"}
	other := base
	other.Body = []byte(`{"amount":7}`)
	err := store.Serializable(ctx, pool, func(tx pgx.Tx) error {
		if _, _, err := Claim(ctx, tx, key, Hash(base)); err != nil {
			return err
		}
		return Complete(ctx, tx, key, Hash(other), Response{Status: 201, Body: []byte(`{}`)})
	})
	if err == nil {
		t.Fatal("Complete with another request's hash succeeded")
	}
}

func TestComplete_WithoutClaimFails(t *testing.T) {
	t.Parallel()
	pool := testutil.Postgres(t)
	ctx := t.Context()
	err := store.Serializable(ctx, pool, func(tx pgx.Tx) error {
		return Complete(ctx, tx, Key{Value: "never-claimed"}, Hash(base), Response{Status: 201, Body: []byte(`{}`)})
	})
	if err == nil {
		t.Fatal("Complete without Claim succeeded")
	}
}

func inTx(t *testing.T, pool *pgxpool.Pool, fn func(pgx.Tx) error) {
	t.Helper()
	if err := store.Serializable(context.Background(), pool, fn); err != nil {
		t.Fatalf("transaction: %v", err)
	}
}

func count(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM idempotency_keys").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
