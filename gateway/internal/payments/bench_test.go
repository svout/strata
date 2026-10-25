package payments

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
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
// no-rail baseline. Since 10a-ii-c the fake keeps its records in
// PostgreSQL, so each new settle costs it two round trips on the pool (read
// the reference, insert the outcome): fake minus the baseline is the
// settlement machinery (the outbox transition, the call, the settled write)
// plus those two statements, no longer the machinery alone. Internal minus
// fake is what the internal rail's posting transaction costs over the fake's
// two statements.
var settleRails = []struct {
	name    string
	newRail railFor
}{
	{"fake", func(_ testing.TB, pool *pgxpool.Pool) rail.Adapter { return rail.NewFake(pool) }},
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

// BenchmarkCreatePaymentConcurrentKeys is SPEC-002 Q11-i: why do k = 8
// concurrent creates on distinct keys roll back ~0.97 times per request
// (BenchmarkCreatePaymentConcurrentSettled, rail none)? Hypothesis: claim's
// read-first SELECT takes an SSI predicate lock on the primary-key btree page
// it searched (on the whole index while the index is empty), so creates whose
// keys share a leaf page conflict. TestClaim_DistinctKeysConflictOnSharedPage
// shows the mechanism with two transactions; this measures how much of the
// rollback rate it explains. The same mechanism was seen once before, on
// ledger.Posted inside the settle transaction (ADR-120 §4 clarification).
// Two axes, no rail, PostgreSQL only, k = 8 (the pool cap):
//   - keys: seq is the existing bench-N-j (consecutive, so every batch
//     appends at the index's rightmost leaf); random is 32 hex characters
//     per request.
//   - table: empty is a fresh database as before; prefilled first inserts
//     100k completed keys spread over the same hex space, so the index has
//     hundreds of leaf pages for random keys to scatter across.
//
// Expected if the hypothesis holds: seq stays high whatever the table size;
// random/prefilled falls to near zero; random/empty lies in between and
// DEPENDS ON -benchtime. The empty table grows during the run (k rows per
// batch), so random keys share one leaf page at first and scatter over more
// as the index splits: measured ~0.95 rollbacks/req at 20 batches, ~0.45 at
// 200. Quote a random/empty figure only with its batch count.
//
// siread-*-max are the most SIRead locks of each granularity seen at once in
// pg_locks, sampled every 5 ms on a separate connection; siread-samples and
// siread-failed say how far to trust them (a failed sample is not a zero).
// The sampler perturbs the run: its transactions are counted in commits/req
// (about 200 per second of run time), and reading SIRead entries in pg_locks
// takes the predicate-lock manager's locks, which the requests need too. So
// this benchmark's latencies are not comparable with ConcurrentSettled's;
// compare rollbacks/req, which the sampler doesn't add to.
func BenchmarkCreatePaymentConcurrentKeys(b *testing.B) {
	const k = 8
	keyings := []struct {
		name string
		keys func(batch int) []string
	}{
		{"seq", func(batch int) []string {
			keys := make([]string, k)
			for j := range k {
				keys[j] = "bench-" + strconv.Itoa(batch) + "-" + strconv.Itoa(j)
			}
			return keys
		}},
		{"random", func(int) []string {
			keys := make([]string, k)
			for j := range k {
				var buf [16]byte
				rand.Read(buf[:]) // never fails (crypto/rand, Go 1.24+)
				keys[j] = hex.EncodeToString(buf[:])
			}
			return keys
		}},
	}
	for _, keying := range keyings {
		for _, prefill := range []bool{false, true} {
			table := "empty"
			if prefill {
				table = "prefilled"
			}
			b.Run(keying.name+"/"+table+"/postgres", func(b *testing.B) {
				r := newBenchRun(b, nil, nil)
				if prefill {
					r.prefillKeys(b, 100_000)
				}
				r.spreadKeys(b, keying.keys(-1)) // warm-up: dials up to k connections outside the timer
				stop := sampleSIRead(b, r.a.pool)
				var batchLat, reqLat []time.Duration
				for b.Loop() {
					start := time.Now()
					reqLat = append(reqLat, r.spreadKeys(b, keying.keys(len(batchLat)))...)
					batchLat = append(batchLat, time.Since(start))
				}
				stop() // the sampler's commits must stop before finish reads pg_stat_database
				r.finish(b, (len(batchLat)+1)*k)
				reportLatency(b, "batch-", batchLat)
				reportLatency(b, "req-", reqLat)
			})
		}
	}
}

// prefillKeys inserts n completed idempotency keys (md5 hex, spread evenly
// over the key space), ANALYZEs the table, and re-takes the counter
// snapshot so the prefill isn't counted. It writes idempotency_keys from the
// payments package: a bench fixture, not production SQL (ADR-117 governs the
// latter; http_test.go reads the table the same way).
func (r *benchRun) prefillKeys(b *testing.B, n int) {
	b.Helper()
	ctx := b.Context()
	if _, err := r.a.pool.Exec(ctx, `
		INSERT INTO idempotency_keys (key, request_hash, status, response_code, response_body)
		SELECT md5(i::text), sha256(i::text::bytea), 'completed', 201, '{}'
		FROM generate_series(1, $1) AS i`, n); err != nil {
		b.Fatalf("prefill: %v", err)
	}
	if _, err := r.a.pool.Exec(ctx, "ANALYZE idempotency_keys"); err != nil {
		b.Fatalf("analyze: %v", err)
	}
	time.Sleep(1500 * time.Millisecond) // as in newBenchRun: let the statistics land
	r.before = settledCounters(b, r.a.pool, r.dbName)
}

// sampleSIRead polls pg_locks for this database's SIRead locks every 5 ms
// on its own one-connection pool (so it never takes a connection the
// requests need) until the returned stop is called. stop reports the maximum
// count seen per granularity, how many samples were taken and failed, and
// logs the relations involved.
//
// A sample that fails is skipped, not fatal, but never silent: a broken
// sampler would report siread-*-max = 0, which reads exactly like "no
// locks". So stop fails the run if no sample succeeded or more than 5%
// failed, and warns on any failure.
func sampleSIRead(b *testing.B, pool *pgxpool.Pool) (stop func()) {
	b.Helper()
	cfg := pool.Config()
	cfg.MaxConns = 1
	sp, err := pgxpool.NewWithConfig(b.Context(), cfg)
	if err != nil {
		b.Fatalf("sampler pool: %v", err)
	}
	maxBy := map[string]int{}      // locktype -> max count at once
	relations := map[string]bool{} // "locktype relation" seen
	var samples, failed int
	var lastErr string // errKind of the last failure, never the error text
	done, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
			}
			samples++
			now, err := siReadSample(sp, relations)
			if err != nil {
				failed++
				lastErr = errKind(err)
				continue
			}
			for lt, n := range now {
				maxBy[lt] = max(maxBy[lt], n)
			}
		}
	}()
	// Teardown runs from stop or, if the run fails first (b.Fatalf inside
	// b.Loop skips stop), from Cleanup: a leaked sampler would keep polling
	// pg_locks and perturb every benchmark after it.
	var once sync.Once
	teardown := func() {
		once.Do(func() {
			close(done)
			<-finished
			sp.Close()
		})
	}
	b.Cleanup(teardown)
	return func() {
		teardown()
		for _, lt := range []string{"tuple", "page", "relation"} {
			b.ReportMetric(float64(maxBy[lt]), "siread-"+lt+"-max")
		}
		b.ReportMetric(float64(samples), "siread-samples")
		b.ReportMetric(float64(failed), "siread-failed")
		b.Logf("SIRead locks seen on: %v", slices.Sorted(maps.Keys(relations)))
		switch {
		case samples == failed:
			b.Fatalf("SIRead sampler: none of %d samples succeeded (last error: %s); siread-*-max are meaningless", samples, lastErr)
		case failed*20 > samples:
			b.Errorf("SIRead sampler: %d of %d samples failed (last error: %s); siread-*-max undercount", failed, samples, lastErr)
		case failed > 0:
			b.Logf("WARNING: SIRead sampler: %d of %d samples failed (last error: %s); siread-*-max may undercount", failed, samples, lastErr)
		}
	}
}

// errKind names a sampler error without its text: a connection error's text
// carries host, user and database name, and bench output feeds the Baseline
// note (SPEC-002 Q10: log excerpts may reach a dissertation appendix).
func errKind(err error) string {
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr):
		return "SQLSTATE " + pgErr.Code
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline exceeded"
	default:
		return fmt.Sprintf("%T", err)
	}
}

// siReadSample reads this database's SIRead lock counts by granularity, and
// adds each "locktype relation" seen to relations. The query has its own
// deadline (gateway/CLAUDE.md: no blocking I/O without one), so a hung
// sample can't block stop.
func siReadSample(sp *pgxpool.Pool, relations map[string]bool) (map[string]int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	rows, err := sp.Query(ctx, `
		SELECT locktype, coalesce(relation::regclass::text, ''), count(*)
		FROM pg_locks
		WHERE mode = 'SIReadLock'
		  AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
		GROUP BY 1, 2`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := map[string]int{}
	for rows.Next() {
		var locktype, rel string
		var n int
		if err := rows.Scan(&locktype, &rel, &n); err != nil {
			return nil, err
		}
		now[locktype] += n
		relations[locktype+" "+rel] = true
	}
	return now, rows.Err()
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
// prefix-(k-1)) and returns their latencies.
func (r *benchRun) spread(b *testing.B, prefix string, k int) []time.Duration {
	keys := make([]string, k)
	for j := range k {
		keys[j] = prefix + "-" + strconv.Itoa(j)
	}
	return r.spreadKeys(b, keys)
}

// spreadKeys sends one concurrent create per key and returns their
// latencies. Every request must get 201: the keys are distinct, so there is
// nothing to deduplicate and anything else is a failure.
func (r *benchRun) spreadKeys(b *testing.B, keys []string) []time.Duration {
	codes, lat := make([]int, len(keys)), make([]time.Duration, len(keys))
	var wg sync.WaitGroup
	for j, key := range keys {
		wg.Go(func() { codes[j], _, lat[j] = r.post(key) })
	}
	wg.Wait()
	for j, c := range codes {
		if c != http.StatusCreated {
			b.Fatalf("spread %s: request %d got %d, want 201 (codes %v)", keys[0], j, c, codes)
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
