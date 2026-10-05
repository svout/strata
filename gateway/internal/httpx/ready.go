package httpx

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// ReadyCheck is one dependency /readyz probes, e.g. a PostgreSQL or Redis ping.
// Check must honour ctx; if it doesn't, the handler still stops waiting at
// the timeout.
type ReadyCheck struct {
	Name  string
	Check func(context.Context) error
}

// readyTimeout bounds each check, so /readyz answers within about a second
// even if a dependency hangs.
const readyTimeout = time.Second

const (
	checkOK      = "ok"
	checkFail    = "fail"
	checkTimeout = "timeout"
)

// readyCacheTTL is how long a /readyz result is reused. The gateway and its
// API share one PostgreSQL pool, so a burst of probes must not each take a
// connection to ping: at most one real check runs per TTL, however many
// callers there are.
const readyCacheTTL = time.Second

// readyz answers 200 {"status":"ready"} or 503 {"status":"not_ready"} with
// "ok", "fail" or "timeout" per check. Results (503s included) are cached for
// readyCacheTTL, and concurrent callers share one in-flight run. Error text
// never reaches the response or the log: driver errors can carry the database
// user, host or DSN.
func readyz(log *slog.Logger, checks []ReadyCheck) http.HandlerFunc {
	return newReadyCache(log, checks, time.Now).handler
}

// readyResult is one finished run, ready to be written to any caller.
// statuses is never modified after the run, so callers may share it.
type readyResult struct {
	code     int
	statuses map[string]string
}

// readyCache runs the checks at most once per ttl. It uses a mutex and one
// "done" channel rather than x/sync/singleflight, to avoid a dependency:
// the first caller after expiry starts a run, later callers wait on the same
// channel, and everyone reads the stored result once it is closed.
type readyCache struct {
	log    *slog.Logger
	checks []ReadyCheck
	ttl    time.Duration
	now    func() time.Time // injectable for tests

	mu       sync.Mutex
	last     *readyResult  // nil until the first run finishes
	lastAt   time.Time     // when last was stored
	inflight chan struct{} // non-nil while a run is going; closed when it ends
}

func newReadyCache(log *slog.Logger, checks []ReadyCheck, now func() time.Time) *readyCache {
	return &readyCache{log: log, checks: checks, ttl: readyCacheTTL, now: now}
}

func (c *readyCache) handler(w http.ResponseWriter, r *http.Request) {
	res, ok := c.get(r.Context())
	if !ok {
		return // the caller went away before the run finished; nobody to answer
	}
	status := "ready"
	if res.code != http.StatusOK {
		status = "not_ready"
	}
	WriteJSON(w, res.code, map[string]any{"status": status, "checks": res.statuses})
}

// get returns a fresh cached result, or joins (or starts) the in-flight run
// and waits for it. ok is false only if ctx ends first.
func (c *readyCache) get(ctx context.Context) (_ readyResult, ok bool) {
	c.mu.Lock()
	if c.last != nil && c.now().Sub(c.lastAt) < c.ttl {
		res := *c.last
		c.mu.Unlock()
		return res, true
	}
	if c.inflight == nil {
		c.inflight = make(chan struct{})
		// The run is detached from this caller: if this client disconnects,
		// the others waiting on it must not all get "timeout". Each check is
		// still bounded by readyTimeout inside runCheck.
		go c.run(context.WithoutCancel(ctx), c.inflight)
	}
	done := c.inflight
	c.mu.Unlock()

	select {
	case <-done:
		c.mu.Lock()
		res := *c.last
		c.mu.Unlock()
		return res, true
	case <-ctx.Done():
		return readyResult{}, false
	}
}

// run executes every check in parallel, logs each failure once (here, not
// per cached hit), stores the result and wakes the waiters.
func (c *readyCache) run(ctx context.Context, done chan struct{}) {
	type result struct{ name, status string }
	results := make(chan result, len(c.checks))
	for _, ch := range c.checks {
		go func() {
			results <- result{ch.Name, runCheck(ctx, ch)}
		}()
	}

	statuses := make(map[string]string, len(c.checks))
	code := http.StatusOK
	for range c.checks {
		res := <-results
		statuses[res.name] = res.status
		if res.status != checkOK {
			code = http.StatusServiceUnavailable
			c.log.LogAttrs(ctx, slog.LevelWarn, "readiness check failed",
				slog.String("check", res.name), slog.String("reason", res.status))
		}
	}

	c.mu.Lock()
	c.last = &readyResult{code: code, statuses: statuses}
	c.lastAt = c.now()
	c.inflight = nil
	c.mu.Unlock()
	close(done)
}

// runCheck returns checkOK, checkFail or checkTimeout. A panicking check counts
// as a failure rather than crashing the process (it runs outside the
// request goroutine, so Recoverer can't catch it).
func runCheck(parent context.Context, c ReadyCheck) string {
	ctx, cancel := context.WithTimeout(parent, readyTimeout)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		defer func() {
			if recover() != nil {
				done <- errCheckPanicked
			}
		}()
		done <- c.Check(ctx)
	}()

	select {
	case err := <-done:
		if err != nil {
			return checkFail
		}
		return checkOK
	case <-ctx.Done():
		return checkTimeout
	}
}

type checkPanic struct{}

func (checkPanic) Error() string { return "readiness check panicked" }

var errCheckPanicked error = checkPanic{}
