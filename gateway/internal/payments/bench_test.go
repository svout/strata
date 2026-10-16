package payments

import (
	"math"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/svout/strata/gateway/internal/testutil"
)

// contendedK is how many concurrent requests share one key in the contended
// variants. It stays below the test pool's 8 connections times a small
// factor, so the pool cap doesn't dominate both modes (8b-ii plan).
const contendedK = 10

// BenchmarkCreatePayment measures POST /v1/payments through the real router
// on the testcontainers PostgreSQL (and Redis), for the baseline SPEC-002
// records for P6 (run with `make bench`, never under -race). Each variant
// reports p50/p99 latency in ms; the Redis variants also report the lock's
// degraded counter.
//
//   - postgres, redis: one request per iteration, each with a new key.
//   - contended-postgres, contended-redis: contendedK concurrent requests with
//     the same key per iteration, timed until all return. This is the
//     evidence ADR-119's review trigger asks for: whether the Redis lock
//     helps over PostgreSQL alone.
func BenchmarkCreatePayment(b *testing.B) {
	noRedis := func(testing.TB) *redis.Client { return nil }
	for _, mode := range []struct {
		name string
		rdb  func(testing.TB) *redis.Client
	}{
		{"postgres", noRedis},
		{"redis", testutil.Redis},
	} {
		b.Run(mode.name, func(b *testing.B) { benchUncontended(b, mode.rdb) })
		b.Run("contended-"+mode.name, func(b *testing.B) { benchContended(b, mode.rdb) })
	}
}

func benchUncontended(b *testing.B, rdb func(testing.TB) *redis.Client) {
	a, locker := newTestAPIWith(b, rdb(b))
	body := a.body("")
	lat := make([]time.Duration, 0, b.N)
	for b.Loop() {
		start := time.Now()
		rec := a.do(http.MethodPost, "/v1/payments", "bench-"+strconv.Itoa(len(lat)), body)
		lat = append(lat, time.Since(start))
		if rec.Code != http.StatusCreated {
			b.Fatalf("request %d = %d: %s", len(lat), rec.Code, rec.Body)
		}
	}
	b.StopTimer()
	assertPayments(b, a, len(lat))
	reportLatency(b, lat)
	if locker != nil {
		b.ReportMetric(float64(locker.Degraded()), "degraded")
	}
}

func benchContended(b *testing.B, rdb func(testing.TB) *redis.Client) {
	a, locker := newTestAPIWith(b, rdb(b))
	body := a.body("")
	lat := make([]time.Duration, 0, b.N)
	for b.Loop() {
		key := "bench-contended-" + strconv.Itoa(len(lat))
		codes := make([]int, contendedK)
		bodies := make([]string, contendedK)
		var wg sync.WaitGroup
		start := time.Now()
		for j := range contendedK {
			wg.Go(func() {
				rec := a.do(http.MethodPost, "/v1/payments", key, body)
				codes[j], bodies[j] = rec.Code, rec.Body.String()
			})
		}
		wg.Wait()
		lat = append(lat, time.Since(start))
		for j := range contendedK {
			if codes[j] != http.StatusCreated || bodies[j] != bodies[0] {
				b.Fatalf("batch %d response %d = %d %s, want the same 201 as response 0", len(lat), j, codes[j], bodies[j])
			}
		}
	}
	b.StopTimer()
	assertPayments(b, a, len(lat)) // exactly one per batch
	reportLatency(b, lat)
	if locker != nil {
		b.ReportMetric(float64(locker.Degraded()), "degraded")
	}
}

func assertPayments(b *testing.B, a *testAPI, want int) {
	b.Helper()
	var got int
	if err := a.pool.QueryRow(b.Context(), "SELECT count(*) FROM payments").Scan(&got); err != nil || got != want {
		b.Fatalf("payments = %d (%v), want %d", got, err, want)
	}
}

// reportLatency reports the nearest-rank p50 and p99 of lat, in ms.
func reportLatency(b *testing.B, lat []time.Duration) {
	b.Helper()
	slices.Sort(lat)
	at := func(p float64) float64 {
		i := max(int(math.Ceil(p*float64(len(lat))))-1, 0)
		return float64(lat[i]) / float64(time.Millisecond)
	}
	b.ReportMetric(at(0.50), "p50-ms")
	b.ReportMetric(at(0.99), "p99-ms")
}
