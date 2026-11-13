package httpx

import (
	"log/slog"
	"math"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
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
// Source identity (Ivan, 2026-10-29; SPEC-002 14c, ADR-128). The source is
// the client's address (see clientAddr): IPv4 per address, IPv4-mapped IPv6
// as IPv4, other IPv6 per /64, because one host usually holds a whole /64
// (assumed: not checked against a source) and a per-address limit would be
// trivial to dodge there. The client is the connection's peer unless the
// peer is a trusted proxy, in which case X-Forwarded-For is read; with no
// trusted proxies configured (the default) the header is ignored, since it
// is whatever the caller says. Neither address is ever logged (Data
// Management Plan §1: source IPs live in memory only).
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
	trusted    []netip.Prefix // proxies whose X-Forwarded-For is believed
	log        *slog.Logger
	elapsed    func() time.Duration // since the limiter was made; injectable for tests

	mu        sync.Mutex
	sources   map[netip.Prefix]*bucket
	overflow  bucket
	swept     bool // lastSweep is set
	lastSweep time.Duration
	limited   atomic.Uint64
	// unattributed counts requests with no usable client address (see
	// clientAddr), all of which share the overflow bucket.
	unattributed atomic.Uint64
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
// once and rate per second after that. trusted are the proxies whose
// X-Forwarded-For it believes (config.Config.TrustedProxies, validated
// there); nil trusts nobody. rate must be above 0 and burst at
// least 1, and rate finite (configuration validates both, SPEC-002
// 10b-ii-b; this is the second line); it panics otherwise. NaN and +Inf
// matter because rate is configurable: either would refuse every request,
// and a tiny rate overflows the sweep interval, so configuration bounds it. Limited requests are logged at Debug only: the Limited counter
// is the signal, and a log line per refused request would let a flood fill
// the log.
func NewRateLimiter(rate float64, burst int, trusted []netip.Prefix, log *slog.Logger) *RateLimiter {
	if !(rate > 0) || math.IsInf(rate, 0) || burst < 1 { // !(rate > 0) also rejects NaN
		panic("httpx: NewRateLimiter needs a finite rate > 0 and burst >= 1")
	}
	start := time.Now() // carries a monotonic reading, so time.Since(start) does too
	l := &RateLimiter{
		rate: rate, burst: float64(burst), maxSources: defaultMaxSources, trusted: trusted, log: log,
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

// Unattributed is the number of requests whose client address couldn't be
// worked out (clientAddr's false), allowed or not. All of them share the
// overflow bucket, so a steady rise means one shared limit for that
// traffic: typically a trusted proxy writing X-Forwarded-For in a form
// clientAddr refuses, such as ip:port (ADR-128). Read in-process, like
// Limited; no log line, which would carry nothing safe to say per request.
func (l *RateLimiter) Unattributed() uint64 { return l.unattributed.Load() }

// Middleware refuses a request with 429 when its source has no token left.
func (l *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		client, known := clientAddr(r, l.trusted)
		if !known {
			l.unattributed.Add(1)
		}
		ok, wait := l.allow(client, known)
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

// allow takes a token for client's source (ok false: no usable address).
// When there is none, it returns false and the whole seconds until the
// next token (at least 1).
// The time is read under the lock, so buckets see readings in the order they
// are applied: read before it, a request could apply an earlier reading
// after a later one from the same source (SPEC-002 10b-ii-a review S1). The
// address is worked out before the lock: it reads only the request.
func (l *RateLimiter) allow(client netip.Addr, ok bool) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.elapsed()
	return l.take(l.bucketFor(client, ok, now), now)
}

// bucketFor returns the source's bucket, making one if there is room. No
// usable address (a RemoteAddr no real connection has, or a malformed
// X-Forwarded-For from a trusted proxy) shares the overflow bucket rather
// than going unlimited.
func (l *RateLimiter) bucketFor(client netip.Addr, ok bool, now time.Duration) *bucket {
	if !ok {
		return &l.overflow
	}
	src := sourceOf(client)
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
// the same. It runs at most once per burst/rate, the longest any bucket takes
// to refill, so each sweep drops every bucket untouched since the one before,
// and the walk of the whole map under the lock happens at most that often
// rather than on every request from a new source. The cost: once the map is
// full, a legitimate new source can wait in the overflow bucket until the
// next sweep, about 2 s at the planned defaults (burst 40,
// 20 per second; SPEC-002 10b-ii), even if most
// buckets refilled sooner. That is acceptable because the map fills only
// under abnormal traffic, and a PSP retries.
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

// sourceOf is the bucket key for a client address (normalised by
// clientAddr): the IPv4 address, or the IPv6 /64. Prefix can't fail for a
// valid address at these lengths.
func sourceOf(a netip.Addr) netip.Prefix {
	bits := 64
	if a.Is4() {
		bits = 32
	}
	p, _ := a.Prefix(bits)
	return p
}

// maxForwardedHops bounds the X-Forwarded-For walk. assumed: no real proxy
// chain is anywhere near 16 hops (not measured); the cap only stops a long
// header from costing a long walk.
const maxForwardedHops = 16

// clientAddr is the address a request's rate limit is keyed on (ADR-128).
//
//   - The peer (r.RemoteAddr's host) is not a trusted proxy: the peer,
//     whatever X-Forwarded-For says. With no trusted proxies, always this.
//   - The peer is trusted: X-Forwarded-For is walked from the right (the
//     entry the nearest proxy appended), across every header line in order;
//     the first entry that is not a trusted proxy is the client. Entries
//     left of it are never parsed, so whatever a caller put there can't
//     choose the result. No header: the peer. Every entry trusted: the
//     leftmost, an internal caller such as a health check.
//   - An entry that isn't a bare address (a port, brackets or a zone, or
//     not an address at all), or a walk past maxForwardedHops, is false:
//     the overflow bucket (SPEC-002 14c decision 3). Not the proxy's own
//     bucket, which its own header-less requests (health checks) use: a
//     malformed header would then use up their limit. The overflow bucket
//     is one shared limit too, and is also where new sources go while the
//     source map is full; Unattributed counts how many requests land
//     there this way.
//
// Addresses are normalised as sourceOf expects: IPv4-mapped IPv6 as IPv4,
// and the peer's zone dropped. The proxy must append to X-Forwarded-For,
// not replace it.
func clientAddr(r *http.Request, trusted []netip.Prefix) (netip.Addr, bool) {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}, false
	}
	peer := normalise(ap.Addr())
	if !isTrusted(peer, trusted) {
		return peer, true
	}
	lines := r.Header.Values("X-Forwarded-For")
	if len(lines) == 0 {
		return peer, true
	}
	var leftmost netip.Addr
	hops := 0
	for i := len(lines) - 1; i >= 0; i-- {
		rest := lines[i]
		for {
			// Take the last entry without allocating a split slice.
			entry := rest
			j := strings.LastIndexByte(rest, ',')
			if j >= 0 {
				entry = rest[j+1:]
			}
			hops++
			if hops > maxForwardedHops {
				return netip.Addr{}, false
			}
			a, err := netip.ParseAddr(strings.TrimSpace(entry))
			if err != nil || a.Zone() != "" { // a remote client has no meaningful zone
				return netip.Addr{}, false
			}
			a = normalise(a)
			if !isTrusted(a, trusted) {
				return a, true
			}
			leftmost = a
			if j < 0 {
				break
			}
			rest = rest[:j]
		}
	}
	return leftmost, true
}

func normalise(a netip.Addr) netip.Addr { return a.WithZone("").Unmap() }

func isTrusted(a netip.Addr, trusted []netip.Prefix) bool {
	for _, p := range trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
