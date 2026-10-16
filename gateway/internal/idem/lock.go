package idem

import (
	"context"
	"crypto/rand"
	"log/slog"
	randv2 "math/rand/v2"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// Locker makes concurrent requests with the same key wait for each other, so
// a duplicate replays the first request's committed response instead of
// racing it into a serialization retry. It is only an optimisation: the
// transaction in Claim/Complete is what makes a key exactly-once (ADR-118),
// so a Locker may fail open.
//
// Lock blocks until the key is free or ctx ends, and returns ctx's error in
// the second case. The caller takes the lock before its transaction, never
// inside it, and calls release once the transaction has finished.
type Locker interface {
	Lock(ctx context.Context, key Key) (release func(), err error)
}

const (
	// lockTTL bounds how long a crashed holder blocks its key. It is at least
	// the 5 s request deadline (httpx), so a live holder never loses the lock.
	lockTTL = 10 * time.Second
	// redisCallTimeout is the budget for each Redis call (SPEC-002 8b). It
	// only holds if the client has ContextTimeoutEnabled; cmd/gateway sets it.
	redisCallTimeout = 50 * time.Millisecond
	// releaseTimeout is longer: a release that times out leaves the lock in
	// place for lockTTL, and every duplicate waits on it until its deadline
	// (503). It is spent only after the transaction, and only when Redis is
	// slow.
	releaseTimeout = 250 * time.Millisecond
	// pollMin and pollJitter set how often a waiter retries: 10–25 ms, jittered
	// so waiters don't retry in step.
	pollMin    = 10 * time.Millisecond
	pollJitter = 15 * time.Millisecond
	// lockPrefix namespaces the lock keys. The rest is Key.Digest(): the
	// caller-chosen key never reaches Redis.
	lockPrefix = "idem:lock:"
)

// releaseScript deletes the lock only if it still holds this holder's token,
// so a holder whose lock expired can't delete the next holder's lock.
var releaseScript = redis.NewScript(`if redis.call("GET", KEYS[1]) == ARGV[1] then return redis.call("DEL", KEYS[1]) end return 0`)

// RedisLocker is a Locker on one Redis key per Idempotency-Key: SET NX with a
// random token, polled until it succeeds or the request's deadline ends.
// When Redis fails, it fails open: Lock returns at once without a lock, logs
// a Warn and counts it in Degraded, and the request runs on PostgreSQL alone.
type RedisLocker struct {
	rdb      *redis.Client
	log      *slog.Logger
	degraded atomic.Uint64
}

// NewRedisLocker returns a RedisLocker on rdb. rdb must have
// ContextTimeoutEnabled set, or a slow Redis can hold a request for the
// client's own dial and read timeouts instead of 50 ms per call.
func NewRedisLocker(rdb *redis.Client, log *slog.Logger) *RedisLocker {
	return &RedisLocker{rdb: rdb, log: log}
}

// Degraded is the number of Lock calls that ran without a lock because Redis
// failed. It is read in-process for now; the OTel work (SPEC-002 subtask 13)
// exports it.
func (l *RedisLocker) Degraded() uint64 { return l.degraded.Load() }

// Lock implements Locker.
func (l *RedisLocker) Lock(ctx context.Context, key Key) (func(), error) {
	name := lockPrefix + key.Digest()
	token := rand.Text()
	for {
		acquired, err := l.tryLock(ctx, name, token)
		if err != nil {
			// A timed-out SET may still have reached Redis. Remove our token
			// so the key isn't blocked for lockTTL by a lock nobody holds;
			// if this fails too, the lock expires on its own.
			_ = l.unlock(ctx, name, token, redisCallTimeout)
			if ctxErr := ctx.Err(); ctxErr != nil {
				return noRelease, ctxErr
			}
			l.degraded.Add(1)
			// Fixed message: the error text can hold the Redis address.
			l.log.LogAttrs(ctx, slog.LevelWarn, "idempotency lock unavailable, continuing on PostgreSQL only",
				slog.String("idempotency_key_digest", key.Digest()),
			)
			return noRelease, nil
		}
		if acquired {
			return func() { l.release(ctx, key, name, token) }, nil
		}
		wait := time.NewTimer(pollMin + randv2.N(pollJitter)) //nolint:gosec // jitter, not a secret
		select {
		case <-ctx.Done():
			wait.Stop()
			return noRelease, ctx.Err()
		case <-wait.C:
		}
	}
}

func (l *RedisLocker) tryLock(ctx context.Context, name, token string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, redisCallTimeout)
	defer cancel()
	return l.rdb.SetNX(ctx, name, token, lockTTL).Result()
}

// release runs even if the request's context has ended, so a timed-out
// holder still frees its key. If it fails, the lock expires after lockTTL.
func (l *RedisLocker) release(ctx context.Context, key Key, name, token string) {
	if err := l.unlock(ctx, name, token, releaseTimeout); err != nil {
		l.log.LogAttrs(ctx, slog.LevelWarn, "idempotency lock release failed, it expires on its own",
			slog.String("idempotency_key_digest", key.Digest()),
		)
	}
}

// unlock deletes the lock if it holds token. It runs even if ctx has ended.
func (l *RedisLocker) unlock(ctx context.Context, name, token string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	return releaseScript.Run(ctx, l.rdb, []string{name}, token).Err()
}

func noRelease() {}
