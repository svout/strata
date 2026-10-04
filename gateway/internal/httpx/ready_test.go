package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func ok(context.Context) error { return nil }

func getReadyz(t *testing.T, log *slog.Logger, checks ...ReadyCheck) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	NewRouter(log, checks...).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	var body map[string]any
	if rec.Code != http.StatusNotFound {
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
	}
	return rec, body
}

func TestReadyz_AllOK(t *testing.T) {
	rec, body := getReadyz(t, NewLogger(io.Discard, slog.LevelInfo),
		ReadyCheck{"postgres", ok}, ReadyCheck{"redis", ok})

	if rec.Code != http.StatusOK || body["status"] != "ready" {
		t.Fatalf("got %d %v, want 200 ready", rec.Code, body)
	}
	checks := body["checks"].(map[string]any)
	if checks["postgres"] != "ok" || checks["redis"] != "ok" {
		t.Fatalf("checks = %v", checks)
	}
	if rec.Header().Get("Content-Type") != "application/json" || rec.Header().Get(RequestIDHeader) == "" {
		t.Fatalf("missing JSON content type or request ID: %v", rec.Header())
	}
}

func TestReadyz_OneFailingNamesIt(t *testing.T) {
	rec, body := getReadyz(t, NewLogger(io.Discard, slog.LevelInfo),
		ReadyCheck{"postgres", ok},
		ReadyCheck{"redis", func(context.Context) error { return errors.New("connection refused") }})

	if rec.Code != http.StatusServiceUnavailable || body["status"] != "not_ready" {
		t.Fatalf("got %d %v, want 503 not_ready", rec.Code, body)
	}
	checks := body["checks"].(map[string]any)
	if checks["postgres"] != "ok" || checks["redis"] != "fail" {
		t.Fatalf("checks = %v", checks)
	}
}

func TestReadyz_HangingCheckTimesOut(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	start := time.Now()
	rec, body := getReadyz(t, NewLogger(io.Discard, slog.LevelInfo),
		ReadyCheck{"postgres", func(context.Context) error { <-block; return nil }}) // ignores ctx

	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("took %v, want about %v", elapsed, readyTimeout)
	}
	if rec.Code != http.StatusServiceUnavailable || body["checks"].(map[string]any)["postgres"] != "timeout" {
		t.Fatalf("got %d %v, want 503 with postgres timeout", rec.Code, body)
	}
}

func TestReadyz_PanickingCheckFails(t *testing.T) {
	rec, body := getReadyz(t, NewLogger(io.Discard, slog.LevelInfo),
		ReadyCheck{"redis", func(context.Context) error { panic("boom") }})

	if rec.Code != http.StatusServiceUnavailable || body["checks"].(map[string]any)["redis"] != "fail" {
		t.Fatalf("got %d %v, want 503 with redis fail", rec.Code, body)
	}
}

func TestReadyz_ErrorTextNeverLeaks(t *testing.T) {
	const secret = "postgres://agentgate:hunter2@db.internal:5432/ledger"
	var buf bytes.Buffer
	rec, _ := getReadyz(t, NewLogger(&buf, slog.LevelInfo),
		ReadyCheck{"postgres", func(context.Context) error { return errors.New("dial " + secret + ": refused") }})

	for where, text := range map[string]string{"response": rec.Body.String(), "log": buf.String()} {
		if strings.Contains(text, "hunter2") || strings.Contains(text, "db.internal") {
			t.Errorf("error text leaked into the %s: %s", where, text)
		}
	}
	if !strings.Contains(buf.String(), `"check":"postgres"`) || !strings.Contains(buf.String(), `"reason":"fail"`) {
		t.Errorf("warn log lacks check/reason:\n%s", buf.String())
	}
}

func TestReadyz_NotMountedWithoutChecks(t *testing.T) {
	rec, _ := getReadyz(t, NewLogger(io.Discard, slog.LevelInfo))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when no checks are configured", rec.Code)
	}
}

// fakeClock is a settable clock for the /readyz cache.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// countingCheck counts its runs; it blocks until release is closed (if
// non-nil) or its ctx ends, and then returns err.
type countingCheck struct {
	calls   atomic.Int32
	started chan struct{} // receives once per run
	release chan struct{}
	err     error
}

func newCountingCheck(release chan struct{}, err error) *countingCheck {
	return &countingCheck{started: make(chan struct{}, 100), release: release, err: err}
}

func (c *countingCheck) Check(ctx context.Context) error {
	c.calls.Add(1)
	c.started <- struct{}{}
	if c.release != nil {
		select {
		case <-c.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return c.err
}

// serveCached sends one /readyz request through a readyCache's handler.
func serveCached(c *readyCache, ctx context.Context) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	c.handler(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil).WithContext(ctx))
	return rec
}

func TestReadyz_ConcurrentCallersShareOneRun(t *testing.T) {
	release := make(chan struct{})
	check := newCountingCheck(release, nil)
	cache := newReadyCache(NewLogger(io.Discard, slog.LevelInfo),
		[]ReadyCheck{{"postgres", check.Check}}, (&fakeClock{t: time.Unix(0, 0)}).Now)

	const n = 50
	codes := make(chan int, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- serveCached(cache, context.Background()).Code
		}()
	}
	<-check.started
	// Give the other callers time to join the in-flight run. Any that arrive
	// after release hit the cache instead (the fake clock never moves), so
	// the assertion below holds either way.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	close(codes)

	for code := range codes {
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
	}
	if got := check.calls.Load(); got != 1 {
		t.Fatalf("check ran %d times for %d concurrent callers, want 1", got, n)
	}
}

func TestReadyz_CachedWithinTTLThenRerun(t *testing.T) {
	check := newCountingCheck(nil, nil)
	clock := &fakeClock{t: time.Unix(0, 0)}
	cache := newReadyCache(NewLogger(io.Discard, slog.LevelInfo),
		[]ReadyCheck{{"postgres", check.Check}}, clock.Now)

	serveCached(cache, context.Background())
	clock.Advance(readyCacheTTL - time.Millisecond)
	if rec := serveCached(cache, context.Background()); rec.Code != http.StatusOK {
		t.Fatalf("cached status = %d, want 200", rec.Code)
	}
	if got := check.calls.Load(); got != 1 {
		t.Fatalf("check ran %d times within the TTL, want 1", got)
	}

	clock.Advance(time.Millisecond)
	serveCached(cache, context.Background())
	if got := check.calls.Load(); got != 2 {
		t.Fatalf("check ran %d times after the TTL, want 2", got)
	}
}

func TestReadyz_Cached503StaysWithinTTL(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	flaky := func(context.Context) error {
		if fail.Load() {
			return errors.New("connection refused")
		}
		return nil
	}
	clock := &fakeClock{t: time.Unix(0, 0)}
	cache := newReadyCache(NewLogger(io.Discard, slog.LevelInfo),
		[]ReadyCheck{{"redis", flaky}}, clock.Now)

	if rec := serveCached(cache, context.Background()); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("first status = %d, want 503", rec.Code)
	}
	fail.Store(false) // the dependency recovers, but the cached 503 holds
	clock.Advance(readyCacheTTL / 2)
	rec := serveCached(cache, context.Background())
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"redis":"fail"`) {
		t.Fatalf("within TTL got %d %s, want cached 503 with redis fail", rec.Code, rec.Body)
	}
	clock.Advance(readyCacheTTL)
	if rec := serveCached(cache, context.Background()); rec.Code != http.StatusOK {
		t.Fatalf("after TTL status = %d, want 200", rec.Code)
	}
}

func TestReadyz_FailureLoggedOncePerRun(t *testing.T) {
	var buf bytes.Buffer
	clock := &fakeClock{t: time.Unix(0, 0)}
	cache := newReadyCache(NewLogger(&buf, slog.LevelInfo),
		[]ReadyCheck{{"redis", func(context.Context) error { return errors.New("refused") }}}, clock.Now)

	for range 5 {
		serveCached(cache, context.Background())
	}
	if got := strings.Count(buf.String(), "readiness check failed"); got != 1 {
		t.Fatalf("logged %d failures for one run, want 1:\n%s", got, buf.String())
	}
	clock.Advance(readyCacheTTL)
	serveCached(cache, context.Background())
	if got := strings.Count(buf.String(), "readiness check failed"); got != 2 {
		t.Fatalf("logged %d failures for two runs, want 2:\n%s", got, buf.String())
	}
}

func TestReadyz_CancelledCallerDoesNotTimeOutOthers(t *testing.T) {
	release := make(chan struct{})
	check := newCountingCheck(release, nil) // returns ctx.Err() if its ctx ends
	cache := newReadyCache(NewLogger(io.Discard, slog.LevelInfo),
		[]ReadyCheck{{"postgres", check.Check}}, (&fakeClock{t: time.Unix(0, 0)}).Now)

	// The first caller starts the run, then disconnects.
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		serveCached(cache, firstCtx)
	}()
	<-check.started
	cancelFirst()
	select {
	case <-firstDone: // a cancelled waiter returns early
	case <-time.After(time.Second):
		t.Fatal("cancelled caller did not return")
	}

	// A second caller joins (or, if it arrives late, reads the cache). Had the
	// run used the first caller's context, the check would already have
	// ended as "timeout".
	second := make(chan *httptest.ResponseRecorder, 1)
	go func() { second <- serveCached(cache, context.Background()) }()
	close(release)
	rec := <-second

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"postgres":"ok"`) {
		t.Fatalf("second caller got %d %s, want 200 with postgres ok", rec.Code, rec.Body)
	}
	if got := check.calls.Load(); got != 1 {
		t.Fatalf("check ran %d times, want 1", got)
	}
}
