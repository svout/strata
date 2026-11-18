package httpx

import (
	"bytes"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pgregory.net/rapid"
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
	l := NewRateLimiter(2, 3, nil, NewLogger(&buf, slog.LevelDebug))
	clock := &fakeElapsed{}
	l.elapsed = clock.get
	return l, clock, &buf
}

// allowFrom takes a token for a request from remoteAddr (no headers), as
// the middleware would.
func allowFrom(l *RateLimiter, remoteAddr string) (bool, int) {
	return l.allow(clientAddr(&http.Request{RemoteAddr: remoteAddr}, l.trusted))
}

// allowN reports how many of n requests from addr are allowed.
func allowN(l *RateLimiter, addr string, n int) int {
	allowed := 0
	for range n {
		if ok, _ := allowFrom(l, addr); ok {
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
	if ok, wait := allowFrom(l, "192.0.2.1:1000"); ok || wait != 1 {
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
	l := NewRateLimiter(0.25, 1, nil, NewLogger(&buf, slog.LevelDebug)) // one token per 4 s
	clock := &fakeElapsed{}
	l.elapsed = clock.get
	allowFrom(l, "192.0.2.1:1")
	clock.advance(1500 * time.Millisecond) // 0.375 tokens: 2.5 s to go
	if _, wait := allowFrom(l, "192.0.2.1:1"); wait != 3 {
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
	l := NewRateLimiter(200, 10, nil, NewLogger(&buf, slog.LevelDebug))
	l.maxSources = 16
	clock := &lockedClock{l: l}
	l.elapsed = clock.get

	if allowFrom(l, "192.0.2.1:1"); clock.unlocked.Load() != 0 {
		t.Fatal("the clock was read outside the lock")
	}
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Go(func() {
			for i := range perGoroutine {
				if ok, _ := allowFrom(l, "192.0.2.1:1"); ok {
					allowed.Add(1)
				}
				allowFrom(l, fmt.Sprintf("198.51.100.%d:1", (g*perGoroutine+i)%64)) // churns the map
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

// With no trusted proxies (the default), X-Forwarded-For is ignored: only
// RemoteAddr names the source.
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
	a, ok := clientAddr(&http.Request{RemoteAddr: addr}, nil)
	if !ok {
		t.Fatalf("clientAddr(%q) failed", addr)
	}
	return sourceOf(a)
}

// The middleware answers 429 rate_limited with Retry-After, never calls the
// handler for a refused request, counts it in Limited, and logs it only at
// Debug.
func TestRateLimiter_Middleware(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := NewRateLimiter(2, 1, nil, NewLogger(&buf, slog.LevelInfo))
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
			NewRateLimiter(tc.rate, tc.burst, nil, slog.New(slog.DiscardHandler))
		}()
	}
}

// SPEC-002 14c (ADR-128): the client address behind trusted proxies.

var testTrusted = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("2001:db8:ffff::/48"),
}

func TestClientAddr(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		remote string
		xff    []string // header lines, in order
		want   string   // "" means false: the overflow bucket
	}{
		{"untrusted peer, forged header ignored", "192.0.2.1:1", []string{"198.51.100.9"}, "192.0.2.1"},
		{"trusted peer, no header", "10.0.0.1:1", nil, "10.0.0.1"},
		{"one trusted hop", "10.0.0.1:1", []string{"198.51.100.9"}, "198.51.100.9"},
		{"chain of trusted hops", "10.0.0.1:1", []string{"198.51.100.9, 10.1.0.1, 10.2.0.1"}, "198.51.100.9"},
		{"forged entries left of the client are never read", "10.0.0.1:1", []string{"garbage, 203.0.113.5,198.51.100.9"}, "198.51.100.9"},
		{"every hop trusted: the leftmost", "10.0.0.1:1", []string{"10.9.0.1, 10.8.0.1"}, "10.9.0.1"},
		{"several header lines, walked last line first", "10.0.0.1:1", []string{"203.0.113.5", "198.51.100.9, 10.1.0.1"}, "198.51.100.9"},
		{"several lines, client in the first", "10.0.0.1:1", []string{"203.0.113.5", "10.1.0.1"}, "203.0.113.5"},
		{"IPv4-mapped entry as IPv4", "10.0.0.1:1", []string{"::ffff:198.51.100.9"}, "198.51.100.9"},
		{"IPv4-mapped trusted peer", "[::ffff:10.0.0.1]:1", []string{"198.51.100.9"}, "198.51.100.9"},
		{"IPv6 trusted peer and client", "[2001:db8:ffff::1]:1", []string{"2001:db8:1::5"}, "2001:db8:1::5"},
		{"zone in an entry", "10.0.0.1:1", []string{"fe80::1%eth0"}, ""},
		{"malformed entry right of the client", "10.0.0.1:1", []string{"198.51.100.9, nope"}, ""},
		{"empty entry", "10.0.0.1:1", []string{"198.51.100.9,"}, ""},
		{"empty header line", "10.0.0.1:1", []string{""}, ""},
		{"port in entry", "10.0.0.1:1", []string{"198.51.100.9:443"}, ""},
		{"brackets in entry", "10.0.0.1:1", []string{"[2001:db8:1::5]"}, ""},
		{"unparseable RemoteAddr", "nope", []string{"198.51.100.9"}, ""},
		{"16 trusted hops: still the leftmost", "10.0.0.1:1", []string{strings.TrimSuffix(strings.Repeat("10.0.0.2,", 16), ",")}, "10.0.0.2"},
		{"17 hops: over the cap", "10.0.0.1:1", []string{strings.TrimSuffix(strings.Repeat("10.0.0.2,", 17), ",")}, ""},
	} {
		r := &http.Request{RemoteAddr: tc.remote, Header: http.Header{}}
		for _, v := range tc.xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		got, ok := clientAddr(r, testTrusted)
		switch {
		case tc.want == "" && ok:
			t.Errorf("%s: = %v, want the overflow bucket (false)", tc.name, got)
		case tc.want != "" && (!ok || got != netip.MustParseAddr(tc.want)):
			t.Errorf("%s: = %v, %v; want %s", tc.name, got, ok, tc.want)
		}
	}
}

// Through the middleware: behind a trusted proxy two clients get a bucket
// each, and a caller connecting directly can't pick its bucket with the
// header. The default limiter (no trusted proxies) is
// TestRateLimiter_IgnoresForwardedFor.
func TestRateLimiter_TrustedProxy(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := NewRateLimiter(2, 3, testTrusted, NewLogger(&buf, slog.LevelDebug))
	l.elapsed = (&fakeElapsed{}).get
	h := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	allowed := func(remote, xff string, n int) int {
		got := 0
		for range n {
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			req.RemoteAddr = remote
			req.Header.Set("X-Forwarded-For", xff)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code == http.StatusNoContent {
				got++
			}
		}
		return got
	}
	if a, b := allowed("10.0.0.1:1", "198.51.100.1", 5), allowed("10.0.0.1:2", "198.51.100.2", 5); a != 3 || b != 3 {
		t.Fatalf("two clients behind the proxy allowed %d and %d of 5, want a burst of 3 each", a, b)
	}
	// A direct caller rotating the header still has one bucket.
	got := 0
	for i := range 5 {
		got += allowed("192.0.2.1:1", "203.0.113."+strconv.Itoa(i+1), 1)
	}
	if got != 3 {
		t.Fatalf("direct caller with a rotating header allowed %d of 5, want 3 (one bucket)", got)
	}
	// Nothing about either address reaches the log, even at Debug (Data
	// Management Plan §1). The log isn't empty: the limited lines are there.
	if !strings.Contains(buf.String(), "request rate limited") {
		t.Fatalf("no limited line logged, so the check below would prove nothing:\n%s", &buf)
	}
	for _, s := range []string{"198.51.100", "203.0.113", "192.0.2.1", "10.0.0.1"} {
		if strings.Contains(buf.String(), s) {
			t.Fatalf("log contains %q:\n%s", s, &buf)
		}
	}
}

// Properties of clientAddr over drawn peers and X-Forwarded-For chains
// (addresses drawn from a small pool, so trusted and untrusted mix often).
func TestClientAddr_Properties(t *testing.T) {
	t.Parallel()
	pool := []string{"10.0.0.1", "10.2.3.4", "2001:db8:ffff::9", "::ffff:10.0.0.7", // trusted
		"192.0.2.1", "198.51.100.9", "2001:db8:1::5", "::ffff:203.0.113.5"} // not
	addr := rapid.SampledFrom(pool)
	chain := rapid.SliceOfN(addr, 0, 20)
	join := func(c []string) string { return strings.Join(c, ", ") }
	req := func(peer string, xff []string) *http.Request {
		r := &http.Request{RemoteAddr: netip.AddrPortFrom(netip.MustParseAddr(peer), 1).String(), Header: http.Header{}}
		if len(xff) > 0 {
			r.Header.Set("X-Forwarded-For", join(xff))
		}
		return r
	}

	// An untrusted peer is the client, whatever the header says.
	t.Run("untrusted peer ignores the header", func(t *testing.T) {
		rapid.Check(t, func(rt *rapid.T) {
			peer := rapid.SampledFrom(pool[4:]).Draw(rt, "peer")
			got, ok := clientAddr(req(peer, chain.Draw(rt, "xff")), testTrusted)
			if !ok || got != normalise(netip.MustParseAddr(peer)) {
				rt.Fatalf("= %v, %v; want the peer %s", got, ok, peer)
			}
		})
	})
	// Once an untrusted hop is in the chain, anything prepended to the left
	// (what a caller controls) changes nothing. The untrusted entry comes
	// first and trusted hops follow it, so the walk crosses those hops
	// before it reaches the client.
	t.Run("prepended entries can't move the result", func(t *testing.T) {
		rapid.Check(t, func(rt *rapid.T) {
			peer := rapid.SampledFrom(pool[:4]).Draw(rt, "peer")
			hops := rapid.SliceOfN(rapid.SampledFrom(pool[:4]), 0, maxForwardedHops-1).Draw(rt, "trusted hops")
			right := append([]string{rapid.SampledFrom(pool[4:]).Draw(rt, "untrusted")}, hops...)
			want, wok := clientAddr(req(peer, right), testTrusted)
			left := rapid.SliceOfN(rapid.SampledFrom(append(pool, "junk", "")), 1, 10).Draw(rt, "left")
			got, ok := clientAddr(req(peer, append(left, right...)), testTrusted)
			if got != want || ok != wok {
				rt.Fatalf("= %v, %v with entries prepended; %v, %v without", got, ok, want, wok)
			}
		})
	})
	// clientAddr agrees with a direct model of the rule: walk the peer and
	// then the chain nearest first; the first untrusted address is the
	// client, otherwise the leftmost; a client past maxForwardedHops (or an
	// all-trusted chain longer than that) is the overflow bucket.
	t.Run("matches the model", func(t *testing.T) {
		rapid.Check(t, func(rt *rapid.T) {
			peer := addr.Draw(rt, "peer")
			xff := chain.Draw(rt, "xff")
			got, ok := clientAddr(req(peer, xff), testTrusted)

			hops := append([]string{peer}, slicesReversed(xff)...) // nearest first
			want, wok := netip.Addr{}, false
			for i, h := range hops {
				if i > maxForwardedHops { // hops[0] is the peer, not counted
					break
				}
				a := normalise(netip.MustParseAddr(h))
				want, wok = a, true
				if !isTrusted(a, testTrusted) {
					break
				}
				if i == len(hops)-1 {
					break // all trusted: the leftmost
				}
				if i == maxForwardedHops {
					want, wok = netip.Addr{}, false // the walk would go on past the cap
				}
			}
			if got != want || ok != wok {
				rt.Fatalf("clientAddr = %v, %v; model says %v, %v", got, ok, want, wok)
			}
		})
	})
}

func slicesReversed(s []string) []string {
	out := make([]string, len(s))
	for i, v := range s {
		out[len(s)-1-i] = v
	}
	return out
}

// BenchmarkClientAddr is the per-request cost behind a trusted proxy: a
// three-hop chain, the worst common case.
func BenchmarkClientAddr(b *testing.B) {
	r := &http.Request{RemoteAddr: "10.0.0.1:443", Header: http.Header{}}
	r.Header.Set("X-Forwarded-For", "198.51.100.9, 10.1.0.1, 10.2.0.1")
	b.ReportAllocs()
	for b.Loop() {
		clientAddr(r, testTrusted)
	}
}

// Requests with no usable client address are counted, allowed or not; a
// request with an address isn't (ADR-128: how a proxy writing ip:port
// shows up).
func TestRateLimiter_Unattributed(t *testing.T) {
	t.Parallel()
	l := NewRateLimiter(2, 3, testTrusted, slog.New(slog.DiscardHandler))
	l.elapsed = (&fakeElapsed{}).get
	h := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	for _, xff := range []string{"198.51.100.9:443", "198.51.100.9:443", "198.51.100.9:443", "198.51.100.9:443", "198.51.100.9"} {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.RemoteAddr = "10.0.0.1:1"
		req.Header.Set("X-Forwarded-For", xff)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	if got := l.Unattributed(); got != 4 {
		t.Fatalf("Unattributed() = %d, want 4 (the ip:port entries, the limited one included)", got)
	}
}
