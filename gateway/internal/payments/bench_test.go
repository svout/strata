package payments

import (
	"context"
	"io"
	"math"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/svout/strata/gateway/internal/idem"
	"github.com/svout/strata/gateway/internal/rail"
	"github.com/svout/strata/gateway/internal/testutil"
)

// The benchmarks below record the POST /v1/payments baseline SPEC-002 keeps
// for P6 and the evidence for ADR-119's review trigger. Run them with
// `make bench` (settings live there; never under -race, never in CI).
//
// Scope: the handler in-process through httptest on the real httpx router
// (deadline, logging, lock wiring), against the testcontainers PostgreSQL and
// Redis in the Docker VM. No TCP or HTTP transport, and the test pool (max 8
// connections, not pgxpool's production default), so these numbers are not
// comparable with network-level load tests. Logs go to io.Discard: JSON is
// still encoded, but no growing buffer is timed.
//
// Metrics: latencies are nearest-rank percentiles in ms. Counters are per
// request: non201/req and 503/req; commits/req and rollbacks/req are
// PostgreSQL transactions in the run's database from pg_stat_database
// (rollbacks are serialization retries; both include the warm-up and a few bookkeeping transactions: the counter
// reads and the payment count);
// degraded/req is the share of Redis-mode requests that ran without the lock.

// modes are the two ways the service runs: PostgreSQL alone, and with the
// Redis in-flight lock (ADR-119).
var modes = []struct {
	name string
	rdb  func(testing.TB) *redis.Client
}{
	{"postgres", func(testing.TB) *redis.Client { return nil }},
	{"redis", testutil.Redis},
}

// BenchmarkCreatePayment is one request per iteration, each with a new key:
// the uncontended latency P6 cites. ns/op is per request. It runs with no
// rail (payments stay created), exactly the code path of the 8b-ii baseline,
// so that comparison stays valid; BenchmarkCreatePaymentSettled adds the
// rail.
func BenchmarkCreatePayment(b *testing.B) {
	for _, mode := range modes {
		b.Run(mode.name, func(b *testing.B) {
			r := newBenchRun(b, mode.rdb(b), nil)
			r.post("bench-warmup") // dials the first connection outside the timer
			var lat []time.Duration
			for b.Loop() {
				start := time.Now()
				r.post("bench-" + strconv.Itoa(len(lat)))
				lat = append(lat, time.Since(start))
			}
			r.finish(b, r.created) // one payment per 201: every key is new
			reportLatency(b, "", lat)
		})
	}
}

// settleRails are the rails BenchmarkCreatePaymentSettled compares with the
// no-rail baseline. The fake answers in memory with no I/O, so fake minus
// the baseline is what the settlement machinery costs (the outbox
// transition, the call, the settled write); internal minus fake is what the
// internal rail's own posting transaction costs.
var settleRails = []struct {
	name    string
	newRail railFor
}{
	{"fake", func(testing.TB, *pgxpool.Pool) rail.Adapter { return rail.NewFake() }},
	{"internal", internalRail},
}

// BenchmarkCreatePaymentSettled is BenchmarkCreatePayment with each payment
// settled on a rail before the response returns (SPEC-002 subtask 10a). ns/op
// is per request.
func BenchmarkCreatePaymentSettled(b *testing.B) {
	for _, rl := range settleRails {
		for _, mode := range modes {
			b.Run(rl.name+"/"+mode.name, func(b *testing.B) {
				r := newBenchRun(b, mode.rdb(b), rl.newRail)
				r.post("bench-warmup")
				var lat []time.Duration
				for b.Loop() {
					start := time.Now()
					r.post("bench-" + strconv.Itoa(len(lat)))
					lat = append(lat, time.Since(start))
				}
				r.finish(b, r.created)
				reportLatency(b, "", lat)
			})
		}
	}
}

// BenchmarkCreatePaymentContended sends k concurrent requests with the same
// key per iteration, for k in a small sweep: does the Redis lock help over
// PostgreSQL alone (ADR-119)? ns/op is per batch of k requests, not per
// request. batch-* is the time until all k return; req-* is each request's
// own latency. With the lock the duplicates replay one at a time, without it
// they race into serialization retries, so the two spread latency
// differently and both views are reported.
func BenchmarkCreatePaymentContended(b *testing.B) {
	for _, k := range []int{2, 8, 16} {
		for _, mode := range modes {
			b.Run("k="+strconv.Itoa(k)+"/"+mode.name, func(b *testing.B) {
				r := newBenchRun(b, mode.rdb(b), nil) // no rail: the 8b-ii comparison
				r.batch(b, "bench-warmup", k)         // dials up to k connections outside the timer
				var batchLat, reqLat []time.Duration
				for b.Loop() {
					start := time.Now()
					reqLat = append(reqLat, r.batch(b, "bench-"+strconv.Itoa(len(batchLat)), k)...)
					batchLat = append(batchLat, time.Since(start))
				}
				r.finish(b, len(batchLat)+1) // exactly one payment per batch, warm-up included
				reportLatency(b, "batch-", batchLat)
				reportLatency(b, "req-", reqLat)
			})
		}
	}
}

// BenchmarkCreatePaymentConcurrentSettled sends k concurrent requests per
// iteration, each with its own key. Unlike BenchmarkCreatePaymentContended
// (k duplicates of one key, one settlement) this is k settlements at once.
// ns/op is per batch of k requests.
//
// It is the "before" for SPEC-002/10a-ii-a. Every bench payment already
// shares one payer and one payee, so a shared hot account is present here
// too; what 10a-ii-a changes is a second ledger.PostRef inside the
// SERIALIZABLE settling -> settled transaction, and the internal rail
// posting to clearing:internal. The before/after difference measures that
// change, not a new shared account.
//
// Hypothesis under test: the ledger is append-only (no balance column), so
// shared accounts add no row contention. That does not rule out
// serialization failures: every transaction here is SERIALIZABLE, and SSI
// predicate locks on index ranges can make unrelated payments conflict.
// rollbacks/req is where either would show. A request runs several
// transactions (create, transitions, the rail's posting) and a rollback may
// come from any of them, so only the differences between the rail variants
// attribute it: none is the create path alone (payments stay created), fake
// adds the settlement transitions, internal adds the ledger posting.
//
// PostgreSQL mode only: the Redis lock is per key, so with distinct keys it
// adds a fixed cost and no contention. The test pool has 8 connections, so
// database concurrency is capped at 8: k = 16 adds pool wait, not more
// contention.
func BenchmarkCreatePaymentConcurrentSettled(b *testing.B) {
	rails := append([]struct {
		name    string
		newRail railFor
	}{{"none", nil}}, settleRails...)
	for _, k := range []int{8, 16} {
		for _, rl := range rails {
			b.Run("k="+strconv.Itoa(k)+"/"+rl.name+"/postgres", func(b *testing.B) {
				r := newBenchRun(b, nil, rl.newRail)
				r.spread(b, "bench-warmup", k) // dials up to k connections outside the timer
				var batchLat, reqLat []time.Duration
				for b.Loop() {
					start := time.Now()
					reqLat = append(reqLat, r.spread(b, "bench-"+strconv.Itoa(len(batchLat)), k)...)
					batchLat = append(batchLat, time.Since(start))
				}
				r.finish(b, (len(batchLat)+1)*k) // k payments per batch, warm-up included
				reportLatency(b, "batch-", batchLat)
				reportLatency(b, "req-", reqLat)
			})
		}
	}
}

// benchRun is one benchmark run's API and outcome counts.
type benchRun struct {
	a       *testAPI
	locker  *idem.RedisLocker
	body    string
	mu      sync.Mutex
	reqs    int
	created int
	non201  int
	n503    int
	dbName  string
	before  [2]int64 // commits, rollbacks
	settles bool     // a rail is wired: every payment must end settled
}

func newBenchRun(b *testing.B, rdb *redis.Client, newRail railFor) *benchRun {
	a, locker := newTestAPILogTo(b, rdb, io.Discard, newRail)
	r := &benchRun{a: a, locker: locker, body: a.body(""), settles: newRail != nil}
	// pg_stat_database is updated when a backend goes idle (at most once a
	// second) or exits. Let the setup's transactions land, then snapshot.
	time.Sleep(1500 * time.Millisecond)
	if err := a.pool.QueryRow(b.Context(), "SELECT current_database()").Scan(&r.dbName); err != nil {
		b.Fatalf("database name: %v", err)
	}
	r.before = settledCounters(b, a.pool, r.dbName)
	return r
}

// post sends one create and counts its outcome.
func (r *benchRun) post(key string) (int, string, time.Duration) {
	start := time.Now()
	rec := r.a.do(http.MethodPost, "/v1/payments", key, r.body)
	d := time.Since(start)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs++
	switch rec.Code {
	case http.StatusCreated:
		r.created++
	case http.StatusServiceUnavailable:
		r.n503++
		r.non201++
	default:
		r.non201++
	}
	return rec.Code, rec.Body.String(), d
}

// batch sends k concurrent creates with key and returns their latencies. It
// fails the run only if no request got 201 or two 201s differ; 503s are
// counted, not fatal (they are evidence too).
func (r *benchRun) batch(b *testing.B, key string, k int) []time.Duration {
	codes, bodies, lat := make([]int, k), make([]string, k), make([]time.Duration, k)
	var wg sync.WaitGroup
	for j := range k {
		wg.Go(func() { codes[j], bodies[j], lat[j] = r.post(key) })
	}
	wg.Wait()
	first := -1
	for j := range k {
		if codes[j] != http.StatusCreated {
			continue
		}
		if first < 0 {
			first = j
		} else if bodies[j] != bodies[first] {
			b.Fatalf("batch %s: two different 201 bodies", key)
		}
	}
	if first < 0 {
		b.Fatalf("batch %s: no request got 201 (codes %v)", key, codes)
	}
	return lat
}

// spread sends k concurrent creates with distinct keys (prefix-0 …
// prefix-(k-1)) and returns their latencies. Every request must get 201:
// there is nothing to deduplicate, so anything else is a failure.
func (r *benchRun) spread(b *testing.B, prefix string, k int) []time.Duration {
	codes, lat := make([]int, k), make([]time.Duration, k)
	var wg sync.WaitGroup
	for j := range k {
		wg.Go(func() { codes[j], _, lat[j] = r.post(prefix + "-" + strconv.Itoa(j)) })
	}
	wg.Wait()
	for j, c := range codes {
		if c != http.StatusCreated {
			b.Fatalf("spread %s: request %d got %d, want 201 (codes %v)", prefix, j, c, codes)
		}
	}
	return lat
}

// finish checks the payment count, then reports the outcome and transaction
// counters per request.
func (r *benchRun) finish(b *testing.B, wantPayments int) {
	b.Helper()
	ctx := context.WithoutCancel(b.Context())
	var payments int
	if err := r.a.pool.QueryRow(ctx, "SELECT count(*) FROM payments").Scan(&payments); err != nil || payments != wantPayments {
		b.Fatalf("payments = %d (%v), want %d", payments, err, wantPayments)
	}
	if r.settles {
		var unsettled int
		if err := r.a.pool.QueryRow(ctx, "SELECT count(*) FROM payments WHERE state <> 'settled'").Scan(&unsettled); err != nil || unsettled != 0 {
			b.Fatalf("unsettled payments = %d (%v), want 0: the run didn't measure settlement", unsettled, err)
		}
	}
	// Closing the pool ends its backends, which flushes their statistics;
	// read them by name on a fresh pool (pg_stat_database is cluster-wide).
	cfg := r.a.pool.Config()
	r.a.pool.Close()
	stats, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		b.Fatalf("stats pool: %v", err)
	}
	defer stats.Close()
	after := settledCounters(b, stats, r.dbName)

	n := float64(r.reqs)
	b.ReportMetric(float64(r.non201)/n, "non201/req")
	b.ReportMetric(float64(r.n503)/n, "503/req")
	b.ReportMetric(float64(after[0]-r.before[0])/n, "commits/req")
	b.ReportMetric(float64(after[1]-r.before[1])/n, "rollbacks/req")
	if r.locker != nil {
		degraded := float64(r.locker.Degraded())
		b.ReportMetric(degraded/n, "degraded/req")
		if degraded > 0 {
			b.Logf("WARNING: %v of %d requests ran without the Redis lock (failed open); this run partly measures PostgreSQL alone", degraded, r.reqs)
		}
	}
}

// settledCounters reads database name's committed and rolled-back transaction
// counts, polling until two reads 200 ms apart agree (statistics arrive
// asynchronously), for at most 5 s.
func settledCounters(b *testing.B, pool *pgxpool.Pool, name string) [2]int64 {
	b.Helper()
	ctx := context.WithoutCancel(b.Context())
	read := func() [2]int64 {
		var c [2]int64
		if err := pool.QueryRow(ctx, "SELECT xact_commit, xact_rollback FROM pg_stat_database WHERE datname = $1", name).Scan(&c[0], &c[1]); err != nil {
			b.Fatalf("read pg_stat_database: %v", err)
		}
		return c
	}
	prev := read()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		time.Sleep(200 * time.Millisecond)
		cur := read()
		// Each read is itself a committed transaction once flushed, so allow
		// the reads' own commits to trail in.
		if cur[1] == prev[1] && cur[0]-prev[0] <= 1 {
			return cur
		}
		prev = cur
	}
	return prev
}

// reportLatency reports the nearest-rank p50, p99 and max of lat in ms, with
// prefix on the unit (e.g. "batch-p50-ms").
func reportLatency(b *testing.B, prefix string, lat []time.Duration) {
	b.Helper()
	slices.Sort(lat)
	at := func(p float64) float64 {
		i := max(int(math.Ceil(p*float64(len(lat))))-1, 0)
		return float64(lat[i]) / float64(time.Millisecond)
	}
	b.ReportMetric(at(0.50), prefix+"p50-ms")
	b.ReportMetric(at(0.99), prefix+"p99-ms")
	b.ReportMetric(at(1), prefix+"max-ms")
}
