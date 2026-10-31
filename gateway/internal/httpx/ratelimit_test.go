package httpx

import (
	"bytes"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// SPEC-002 10b-ii-a: the per-source limiter for the rail webhook (ADR-124).
// Time is a settable duration, as the limiter keeps it.

type fakeElapsed struct {
	mu sync.Mutex
	d  time.Duration
}

func (c *fakeElapsed) get() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.d
}

func (c *fakeElapsed) set(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.d = d
}

func (c *fakeElapsed) advance(d time.Duration) { c.set(c.get() + d) }

// newTestLimiter allows burst 3 and 2 per second, on a settable clock.
func newTestLimiter(t *testing.T) (*RateLimiter, *fakeElapsed, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	l := NewRateLimiter(2, 3, NewLogger(&buf, slog.LevelDebug))
	clock := &fakeElapsed{}
	l.elapsed = clock.get
	return l, clock, &buf
}

// allowN reports how many of n requests from addr are allowed.
func allowN(l *RateLimiter, addr string, n int) int {
	allowed := 0
	for range n {
		if ok, _ := l.allow(addr); ok {
			allowed++
		}
	}
	return allowed
}

func TestRateLimiter_BurstThenRefill(t *testing.T) {
	t.Parallel()
	l, clock, _ := newTestLimiter(t)
	if got := allowN(l, "192.0.2.1:1000", 5); got != 3 {
		t.Fatalf("allowed %d of 5 at once, want the burst of 3", got)
	}
	if ok, wait := l.allow("192.0.2.1:1000"); ok || wait != 1 {
		t.Fatalf("empty bucket = %v, Retry-After %d; want refused, 1", ok, wait)
	}
	clock.advance(500 * time.Millisecond) // one token at 2/s
	if got := allowN(l, "192.0.2.1:1000", 2); got != 1 {
		t.Fatalf("allowed %d after 0.5 s, want 1", got)
	}
	clock.advance(time.Hour) // refill is capped at the burst
	if got := allowN(l, "192.0.2.1:1000", 10); got != 3 {
		t.Fatalf("allowed %d after an hour, want 3 (capped at burst)", got)
	}
}

// Retry-After is the whole seconds until the next token, at least 1.
func TestRateLimiter_RetryAfterRoundsUp(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := NewRateLimiter(0.25, 1, NewLogger(&buf, slog.LevelDebug)) // one token per 4 s
	clock := &fakeElapsed{}
	l.elapsed = clock.get
	l.allow("192.0.2.1:1")
	clock.advance(1500 * time.Millisecond) // 0.375 tokens: 2.5 s to go
	if _, wait := l.allow("192.0.2.1:1"); wait != 3 {
		t.Fatalf("Retry-After = %d, want 3 (2.5 s rounded up)", wait)
	}
}

// lockedClock is a clock that ticks one millisecond per reading and records
// any reading taken while l.mu is free: the time must be read under the
// lock, or two requests from one source can apply their readings out of
// order (SPEC-002 10b-ii-a review S1).
type lockedClock struct {
	l        *RateLimiter
	ticks    atomic.Int64
	unlocked atomic.Int64
}

func (c *lockedClock) get() time.Duration {
	if c.l.mu.TryLock() { // free: nobody, so not this request, holds it
		c.l.mu.Unlock()
		c.unlocked.Add(1)
	}
	return time.Duration(c.ticks.Add(1)) * time.Millisecond
}

// Concurrent requests share the limiter safely and never get more than the
// bucket allows. One source is hammered from many goroutines on a clock that
// ticks 1 ms per reading, at 200/s: 0.2 tokens a tick, and half the readings
// are this source's, so it earns 0.4 tokens per request and stays drained.
// Every grant is then paid for by elapsed time, so allowed can't exceed
// burst + rate * elapsed, and an interval credited twice would show. Many sources against a small map run the sweep and the overflow
// bucket concurrently too. Run with -race (make test does), which reports a
// missing or bypassed lock.
func TestRateLimiter_ConcurrentRequestsShareTheBucket(t *testing.T) {
	t.Parallel()
	const goroutines, perGoroutine = 8, 2000
	var buf syncBuffer
	l := NewRateLimiter(200, 10, NewLogger(&buf, slog.LevelDebug))
	l.maxSources = 16
	clock := &lockedClock{l: l}
	l.elapsed = clock.get

	if l.allow("192.0.2.1:1"); clock.unlocked.Load() != 0 {
		t.Fatal("the clock was read outside the lock")
	}
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Go(func() {
			for i := range perGoroutine {
				if ok, _ := l.allow("192.0.2.1:1"); ok {
					allowed.Add(1)
				}
				l.allow(fmt.Sprintf("198.51.100.%d:1", (g*perGoroutine+i)%64)) // churns the map
			}
		})
	}
	wg.Wait()
	if n := clock.unlocked.Load(); n != 0 {
		t.Fatalf("the clock was read outside the lock %d times", n)
	}
	// Every allow reads the clock once, so ticks is the elapsed time in ms.
	limit := l.burst + l.rate*(time.Duration(clock.ticks.Load())*time.Millisecond).Seconds()
	if got := float64(allowed.Load() + 1); got > limit { // +1: the first, serial request
		t.Fatalf("allowed %v requests from one source, more than burst + rate*elapsed = %v", got, limit)
	}
}

// A clock that goes backwards neither grants nor removes tokens; the
// limiter reads the monotonic clock, so this is defence in depth.
func TestRateLimiter_BackwardsClockGrantsNothing(t *testing.T) {
	t.Parallel()
	l, clock, _ := newTestLimiter(t)
	clock.set(time.Minute)
	allowN(l, "192.0.2.1:1", 3)
	clock.set(0)
	if got := allowN(l, "192.0.2.1:1", 3); got != 0 {
		t.Fatalf("allowed %d after the clock went back, want 0", got)
	}
	clock.set(time.Minute + 500*time.Millisecond)
	if got := allowN(l, "192.0.2.1:1", 3); got != 1 {
		t.Fatalf("allowed %d 0.5 s after the original time, want 1", got)
	}
}

func TestRateLimiter_SourceIdentity(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		a, b  string
		share bool
	}{
		"two IPv4 addresses":        {"192.0.2.1:1", "192.0.2.2:1", false},
		"same IPv4, other port":     {"192.0.2.1:1", "192.0.2.1:2", true},
		"IPv6 in one /64":           {"[2001:db8:0:1::1]:1", "[2001:db8:0:1:ffff::2]:1", true},
		"IPv6 in two /64s":          {"[2001:db8:0:1::1]:1", "[2001:db8:0:2::1]:1", false},
		"IPv4-mapped IPv6 and IPv4": {"[::ffff:192.0.2.1]:1", "192.0.2.1:2", true},
		"zone ignored":              {"[fe80::1%eth0]:1", "[fe80::2]:1", true},
	} {
		l, _, _ := newTestLimiter(t)
		allowN(l, tc.a, 3)
		got := allowN(l, tc.b, 1) == 0
		if got != tc.share {
			t.Errorf("%s: share a bucket = %v, want %v", name, got, tc.share)
		}
	}
}

// X-Forwarded-For is ignored: only RemoteAddr names the source.
func TestRateLimiter_IgnoresForwardedFor(t *testing.T) {
	t.Parallel()
	l, _, _ := newTestLimiter(t)
	h := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	codes := map[int]int{}
	for i := range 5 {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.RemoteAddr = "192.0.2.1:1"
		req.Header.Set("X-Forwarded-For", "198.51.100."+string(rune('1'+i)))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		codes[rec.Code]++
	}
	if codes[http.StatusNoContent] != 3 || codes[http.StatusTooManyRequests] != 2 {
		t.Fatalf("codes = %v, want 3×204 and 2×429 (one source)", codes)
	}
}

// An address that doesn't parse shares the overflow bucket: limited, never
// unlimited.
func TestRateLimiter_UnparseableAddrIsLimited(t *testing.T) {
	t.Parallel()
	l, _, _ := newTestLimiter(t)
	if got := allowN(l, "not an address", 2) + allowN(l, "", 5); got != 3 {
		t.Fatalf("allowed %d unparseable requests, want 3 (one shared bucket)", got)
	}
}

// With the map full, a new source shares the overflow bucket, which limits
// all such sources together like one source. Once the full sources' buckets
// have refilled, a sweep drops them and a new source gets its own again.
func TestRateLimiter_FullMapOverflowsThenSweeps(t *testing.T) {
	t.Parallel()
	l, clock, _ := newTestLimiter(t)
	l.maxSources = 2
	allowN(l, "192.0.2.1:1", 1)
	allowN(l, "192.0.2.2:1", 1)

	if got := allowN(l, "192.0.2.3:1", 2) + allowN(l, "192.0.2.4:1", 5); got != 3 {
		t.Fatalf("overflow sources allowed %d together, want 3 (one shared bucket)", got)
	}
	if len(l.sources) != 2 {
		t.Fatalf("%d sources kept, want 2 (the cap)", len(l.sources))
	}

	// Within the refill time (burst/rate = 1.5 s) of the last sweep (at 0,
	// when 192.0.2.3 overflowed) no further sweep runs: the map stays full
	// even though both old buckets have refilled by 1 s (2 tokens + 2/s).
	clock.advance(time.Second)
	allowN(l, "192.0.2.5:1", 1)
	if _, own := l.sources[mustSource(t, "192.0.2.5:1")]; own {
		t.Fatal("new source got its own bucket before the old ones refilled")
	}

	clock.advance(2 * time.Second)
	if got := allowN(l, "192.0.2.6:1", 5); got != 3 {
		t.Fatalf("new source after the sweep allowed %d, want its own burst of 3", got)
	}
	if _, own := l.sources[mustSource(t, "192.0.2.6:1")]; !own {
		t.Fatal("new source has no bucket of its own after the sweep")
	}
}

func mustSource(t *testing.T, addr string) netip.Prefix {
	t.Helper()
	p, ok := sourceOf(addr)
	if !ok {
		t.Fatalf("sourceOf(%q) failed", addr)
	}
	return p
}

// The middleware answers 429 rate_limited with Retry-After, never calls the
// handler for a refused request, counts it in Limited, and logs it only at
// Debug.
func TestRateLimiter_Middleware(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := NewRateLimiter(2, 1, NewLogger(&buf, slog.LevelInfo))
	called := 0
	h := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called++
		w.WriteHeader(http.StatusNoContent)
	}))
	serve := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.RemoteAddr = "192.0.2.1:1"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	if rec := serve(); rec.Code != http.StatusNoContent {
		t.Fatalf("first request = %d, want 204", rec.Code)
	}
	rec := serve()
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "1" ||
		!strings.Contains(rec.Body.String(), `"code":"rate_limited"`) {
		t.Fatalf("limited = %d %v %s, want 429, Retry-After 1, rate_limited", rec.Code, rec.Header(), rec.Body)
	}
	if called != 1 || l.Limited() != 1 {
		t.Fatalf("handler called %d times, Limited = %d; want 1 and 1", called, l.Limited())
	}
	if buf.Len() != 0 {
		t.Fatalf("limited request logged above Debug:\n%s", &buf)
	}
}

func TestNewRateLimiter_RejectsBadValues(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		rate  float64
		burst int
	}{{0, 1}, {-1, 1}, {math.NaN(), 1}, {math.Inf(1), 1}, {1, 0}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("NewRateLimiter(%v, %d) did not panic", tc.rate, tc.burst)
				}
			}()
			NewRateLimiter(tc.rate, tc.burst, slog.New(slog.DiscardHandler))
		}()
	}
}
