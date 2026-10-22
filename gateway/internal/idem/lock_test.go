package idem

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/svout/strata/gateway/internal/testutil"
)

func newTestLocker(t *testing.T, rdb *redis.Client) (*RedisLocker, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	return NewRedisLocker(rdb, slog.New(slog.NewJSONHandler(&buf, nil))), &buf
}

// A second Lock on the same key waits until the first is released; another
// key doesn't wait.
func TestRedisLocker_WaitsForRelease(t *testing.T) {
	t.Parallel()
	l, _ := newTestLocker(t, testutil.Redis(t))
	ctx := t.Context()
	k := Key{Value: "k-wait"}

	release, err := l.Lock(ctx, k)
	if err != nil {
		t.Fatal(err)
	}
	other, err := l.Lock(ctx, Key{Value: "k-other"})
	if err != nil {
		t.Fatalf("another key: %v", err)
	}
	other()

	got := make(chan error, 1)
	go func() {
		second, err := l.Lock(ctx, k)
		if err == nil {
			second()
		}
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("second Lock returned (%v) while the first was held", err)
	case <-time.After(150 * time.Millisecond):
	}
	release()
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("second Lock after release: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second Lock did not acquire after release")
	}
}

// A waiter gives up when its context ends and returns the context's error.
func TestRedisLocker_WaitEndsAtDeadline(t *testing.T) {
	t.Parallel()
	l, _ := newTestLocker(t, testutil.Redis(t))
	k := Key{Value: "k-deadline"}
	release, err := l.Lock(t.Context(), k)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := l.Lock(ctx, k); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Lock = %v, want DeadlineExceeded", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Lock returned after %s, want soon after the 100 ms deadline", d)
	}
	if l.Degraded() != 0 {
		t.Fatalf("Degraded = %d after a deadline, want 0", l.Degraded())
	}
}

// The lock has the TTL, its Redis key is the digest, and a holder whose lock
// expired and was taken over can't release the new holder's lock.
func TestRedisLocker_TTLAndOwnTokenOnly(t *testing.T) {
	t.Parallel()
	rdb := testutil.Redis(t)
	l, _ := newTestLocker(t, rdb)
	ctx := t.Context()
	k := Key{Value: "sk_live_lock_secret"}
	name := lockPrefix + k.Digest()

	first, err := l.Lock(ctx, k)
	if err != nil {
		t.Fatal(err)
	}
	if ttl := rdb.PTTL(ctx, name).Val(); ttl <= lockTTL-time.Second || ttl > lockTTL {
		t.Fatalf("PTTL = %s, want about %s", ttl, lockTTL)
	}
	keys := rdb.Keys(ctx, "*").Val()
	if len(keys) != 1 || strings.Contains(keys[0], "secret") {
		t.Fatalf("Redis keys = %v, want one digest key", keys)
	}

	rdb.Del(ctx, name) // as if it had expired
	second, err := l.Lock(ctx, k)
	if err != nil {
		t.Fatal(err)
	}
	first()
	if n := rdb.Exists(ctx, name).Val(); n != 1 {
		t.Fatal("an expired holder's release deleted the new holder's lock")
	}
	second()
	if n := rdb.Exists(ctx, name).Val(); n != 0 {
		t.Fatal("release left the lock in place")
	}
}

// Redis down: Lock fails open within the call budget, counts it and logs one
// fixed Warn line with neither the key nor the Redis address (AC8).
func TestRedisLocker_RedisDownFailsOpen(t *testing.T) {
	t.Parallel()
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", ContextTimeoutEnabled: true})
	t.Cleanup(func() { _ = rdb.Close() })
	l, buf := newTestLocker(t, rdb)

	start := time.Now()
	release, err := l.Lock(t.Context(), Key{Value: "sk_live_down_secret"})
	if err != nil {
		t.Fatalf("Lock = %v, want fail open", err)
	}
	release()
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("Lock took %s with Redis down, want about the 50 ms budget", d)
	}
	if l.Degraded() != 1 {
		t.Fatalf("Degraded = %d, want 1", l.Degraded())
	}
	out := buf.String()
	if strings.Count(out, `"level":"WARN"`) != 1 || !strings.Contains(out, "continuing on PostgreSQL only") {
		t.Fatalf("want one Warn line:\n%s", out)
	}
	if strings.Contains(out, "secret") || strings.Contains(out, "127.0.0.1") {
		t.Fatalf("key or address logged:\n%s", out)
	}
}

// 8b-i should 1: a go-redis retry of a SET that had already landed finds our
// own token, and that is our lock. Another holder's token is still "held",
// and the attempt leaves that holder's lock as it was.
func TestRedisLocker_OwnTokenIsAcquired(t *testing.T) {
	t.Parallel()
	rdb := testutil.Redis(t)
	l, _ := newTestLocker(t, rdb)
	ctx := t.Context()
	name := lockPrefix + Key{Value: "k-own"}.Digest()

	if ok, err := l.tryLock(ctx, name, "mine"); err != nil || !ok {
		t.Fatalf("free key: acquired = %v (%v), want true", ok, err)
	}
	if ttl := rdb.PTTL(ctx, name).Val(); ttl <= lockTTL-time.Second || ttl > lockTTL {
		t.Fatalf("PTTL = %s, want about %s", ttl, lockTTL)
	}
	if ok, err := l.tryLock(ctx, name, "mine"); err != nil || !ok {
		t.Fatalf("own token (a retried SET): acquired = %v (%v), want true", ok, err)
	}
	if ok, err := l.tryLock(ctx, name, "theirs"); err != nil || ok {
		t.Fatalf("another token: acquired = %v (%v), want false", ok, err)
	}
	if got := rdb.Get(ctx, name).Val(); got != "mine" {
		t.Fatalf("lock holds %q after another holder's attempt, want %q", got, "mine")
	}
}

// silentRedis is a client whose server accepts connections and never replies.
// go-redis opens each connection with a HELLO handshake, so every call times
// out there, before its own command is sent; the deadline is the same one a
// command stalled on an open connection would hit. A refused port
// (RedisDownFailsOpen) fails at once and never reaches the timeout path.
func silentRedis(t *testing.T, contextTimeout bool) *redis.Client {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu     sync.Mutex
		conns  []net.Conn
		closed bool
	)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return // listener closed
			}
			mu.Lock()
			if closed { // accepted while cleanup ran: close it here instead
				_ = c.Close()
			} else {
				conns = append(conns, c) // held open, never read or answered
			}
			mu.Unlock()
		}
	}()
	rdb := redis.NewClient(&redis.Options{Addr: ln.Addr().String(), ContextTimeoutEnabled: contextTimeout})
	t.Cleanup(func() {
		_ = rdb.Close()
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		closed = true
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return rdb
}

// 8b-i should 2: a Redis that never replies costs one redisCallTimeout for
// the SET plus one for the cleanup unlock (the accepted should-3 debt), then
// Lock fails open. The budget only holds with ContextTimeoutEnabled; without it
// go-redis waits its 3 s read timeout per call, and this test fails.
func TestRedisLocker_StalledRedisFailsOpenWithinBudget(t *testing.T) {
	t.Parallel()
	l, buf := newTestLocker(t, silentRedis(t, true))

	type result struct {
		err error
		d   time.Duration
	}
	got := make(chan result, 1)
	go func() {
		start := time.Now()
		release, err := l.Lock(t.Context(), Key{Value: "k-stalled"})
		release()
		got <- result{err, time.Since(start)}
	}()
	var r result
	select {
	case r = <-got:
	case <-time.After(time.Second): // without the budget a call takes 3 s
		t.Fatalf("Lock still waiting after 1 s on a stalled Redis, want about %s", 2*redisCallTimeout)
	}
	if r.err != nil {
		t.Fatalf("Lock = %v, want fail open", r.err)
	}
	// The lower bound shows the timeout path ran; the upper is two calls
	// plus scheduling slack.
	if r.d < redisCallTimeout || r.d > 2*redisCallTimeout+150*time.Millisecond {
		t.Fatalf("Lock took %s on a stalled Redis, want about 2 x %s", r.d, redisCallTimeout)
	}
	if l.Degraded() != 1 {
		t.Fatalf("Degraded = %d, want 1", l.Degraded())
	}
	if out := buf.String(); strings.Count(out, `"level":"WARN"`) != 1 {
		t.Fatalf("want one Warn line:\n%s", out)
	}
}
