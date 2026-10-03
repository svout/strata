package httpx

import (
	"context"
	"log/slog"
	"net/http"
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

// readyz runs every check in parallel and answers 200 {"status":"ready"} or
// 503 {"status":"not_ready"} with "ok", "fail" or "timeout" per check. Error
// text never reaches the response or the log: driver errors can carry the
// database user, host or DSN.
func readyz(log *slog.Logger, checks []ReadyCheck) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		type result struct{ name, status string }
		results := make(chan result, len(checks))
		for _, c := range checks {
			go func() {
				results <- result{c.Name, runCheck(r.Context(), c)}
			}()
		}

		statuses := make(map[string]string, len(checks))
		ready := true
		for range checks {
			res := <-results
			statuses[res.name] = res.status
			if res.status != checkOK {
				ready = false
				log.LogAttrs(r.Context(), slog.LevelWarn, "readiness check failed",
					slog.String("check", res.name), slog.String("reason", res.status))
			}
		}

		if !ready {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "not_ready", "checks": statuses})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ready", "checks": statuses})
	}
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
