package httpx

import (
	"log/slog"
	"math"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// RateLimiter limits requests per source with a token bucket each
// (SPEC-002 10b-ii, ADR-124: the rail webhook is unauthenticated, and each
// request it accepts can cost a rail call). A source gets burst requests at
// once and rate per second after that; beyond it a request gets 429
// rate_limited with Retry-After. It is in-process: one gateway, one set of
// buckets.
//
// Source identity (Ivan, 2026-10-29). The source is the host of
// r.RemoteAddr: IPv4 per address, IPv4-mapped IPv6 as IPv4, other IPv6 per
// /64, because one host usually holds a whole /64 and a per-address limit
// would be trivial to dodge there. X-Forwarded-For is ignored: P1 has no
// trusted proxy, and the header is whatever the caller says. Behind a proxy
// every caller would share the proxy's bucket and this would become one
// global limit, so trusted-proxy configuration is on P1's pre-deploy
// correctness list.
//
// Memory is fixed: at most maxSources buckets. A handful of PSPs never come
// near that, so the map only fills when someone sends from thousands of
// addresses. When it is full, buckets that have refilled completely are
// dropped (they hold no state worth keeping), and a source that still finds
// no room shares one overflow bucket with every other such source, limited
// like a single source. The overflow is an attack path, not a capacity path,
// which is why it is tight rather than generous. Its limitation, named: an
// attacker who fills the map pushes legitimate new sources into the shared
// bucket. Every fixed-memory per-source limiter has this property; the
// alternatives are unbounded memory or letting an attacker evict legitimate
// sources' buckets.
//
// Time is kept as durations since the limiter was made, read with
// time.Since, which uses the monotonic clock: no absolute wall time is stored
// and compared later, so an NTP correction or a suspend and resume can't
// grant a burst of tokens or stall a bucket.
type RateLimiter struct {
	rate       float64 // tokens per second
	burst      float64
	maxSources int
	log        *slog.Logger
	elapsed    func() time.Duration // since the limiter was made; injectable for tests

	mu        sync.Mutex
	sources   map[netip.Prefix]*bucket
	overflow  bucket
	swept     bool // lastSweep is set
	lastSweep time.Duration
	limited   atomic.Uint64
}

// bucket holds tokens as of last, a duration since the limiter was made.
type bucket struct {
	tokens float64
	last   time.Duration
}

// defaultMaxSources caps the buckets kept: about 1 MB of map at this size
// (assumed: ~100 bytes per entry; not measured).
const defaultMaxSources = 10_000

// NewRateLimiter returns a limiter allowing each source burst requests at
// once and rate per second after that. rate must be above 0 and burst at
// least 1 (configuration validates both, SPEC-002 10b-ii-b); it panics
// otherwise. Limited requests are logged at Debug only: the Limited counter
// is the signal, and a log line per refused request would let a flood fill
// the log.
func NewRateLimiter(rate float64, burst int, log *slog.Logger) *RateLimiter {
	if rate <= 0 || burst < 1 {
		panic("httpx: NewRateLimiter needs rate > 0 and burst >= 1")
	}
	start := time.Now() // carries a monotonic reading, so time.Since(start) does too
	l := &RateLimiter{
		rate: rate, burst: float64(burst), maxSources: defaultMaxSources, log: log,
		elapsed: func() time.Duration { return time.Since(start) },
		sources: map[netip.Prefix]*bucket{},
	}
	l.overflow = bucket{tokens: l.burst}
	return l
}

// Limited is the number of requests refused with 429. Like
// payments.Service.WebhookShed (ADR-119's pattern), it is read in-process for
// now; the OTel work (SPEC-002 subtask 13) exports it.
func (l *RateLimiter) Limited() uint64 { return l.limited.Load() }

// Middleware refuses a request with 429 when its source has no token left.
func (l *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok, wait := l.allow(r.RemoteAddr)
		if !ok {
			l.limited.Add(1)
			l.log.LogAttrs(r.Context(), slog.LevelDebug, "request rate limited",
				slog.String("request_id", RequestID(r.Context())))
			w.Header().Set("Retry-After", strconv.Itoa(wait))
			WriteProblem(w, http.StatusTooManyRequests, "rate_limited", "Too many requests from this source; retry later")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// allow takes a token for remoteAddr's source. When there is none, it
// returns false and the whole seconds until the next token (at least 1).
func (l *RateLimiter) allow(remoteAddr string) (bool, int) {
	now := l.elapsed()
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.take(l.bucketFor(remoteAddr, now), now)
}

// bucketFor returns the source's bucket, making one if there is room. An
// address that doesn't parse (no real connection has one) shares the
// overflow bucket rather than going unlimited.
func (l *RateLimiter) bucketFor(remoteAddr string, now time.Duration) *bucket {
	src, ok := sourceOf(remoteAddr)
	if !ok {
		return &l.overflow
	}
	if b, ok := l.sources[src]; ok {
		return b
	}
	if len(l.sources) >= l.maxSources {
		l.sweep(now)
	}
	if len(l.sources) >= l.maxSources {
		return &l.overflow
	}
	b := &bucket{tokens: l.burst, last: now}
	l.sources[src] = b
	return b
}

// sweep drops buckets that have refilled completely: a fresh bucket would be
// the same. It walks the whole map, so it runs at most once per refill time
// (burst/rate): sooner, few more buckets could have refilled, and a flood of
// new sources would make every request walk the map.
func (l *RateLimiter) sweep(now time.Duration) {
	refill := time.Duration(l.burst / l.rate * float64(time.Second))
	if l.swept && now-l.lastSweep < refill {
		return
	}
	l.swept, l.lastSweep = true, now
	for src, b := range l.sources {
		if l.refilled(b, now) >= l.burst {
			delete(l.sources, src)
		}
	}
}

// refilled is b's token count at now, capped at burst. A negative interval
// (which the monotonic clock shouldn't produce) counts as none, so it can
// neither add nor remove tokens.
func (l *RateLimiter) refilled(b *bucket, now time.Duration) float64 {
	d := now - b.last
	if d < 0 {
		d = 0
	}
	return math.Min(l.burst, b.tokens+d.Seconds()*l.rate)
}

func (l *RateLimiter) take(b *bucket, now time.Duration) (bool, int) {
	// last never moves backwards: after a backwards reading, the return to
	// the true time would otherwise count the whole gap as refill.
	b.tokens, b.last = l.refilled(b, now), max(b.last, now)
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, max(1, int(math.Ceil((1-b.tokens)/l.rate)))
}

// sourceOf is the bucket key for a RemoteAddr ("ip:port"): the IPv4
// address, or the IPv6 /64.
func sourceOf(remoteAddr string) (netip.Prefix, bool) {
	ap, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		return netip.Prefix{}, false
	}
	a := ap.Addr().WithZone("").Unmap()
	bits := 64
	if a.Is4() {
		bits = 32
	}
	p, err := a.Prefix(bits)
	return p, err == nil
}
