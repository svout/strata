package httpx

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// NewRouter builds the gateway's HTTP handler. Later subtasks mount the
// payment API on it. It serves /healthz (liveness), and /readyz when
// readiness checks are given.
func NewRouter(log *slog.Logger, checks ...ReadyCheck) http.Handler {
	r := chi.NewRouter()
	r.Use(requestID)
	r.Use(requestLog(log))
	r.Use(middleware.Recoverer)

	r.Get("/healthz", healthz)
	if len(checks) > 0 {
		r.Get("/readyz", readyz(log, checks))
	}
	return r
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // the status line is already sent; nothing useful to do on error
}

// requestLog logs one line per request: method, path, status, duration and
// request ID. It never logs headers or bodies.
func requestLog(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)
			log.LogAttrs(r.Context(), slog.LevelInfo, "request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", ww.Status()),
				slog.Duration("duration", time.Since(start)),
				slog.String("request_id", RequestID(r.Context())),
			)
		})
	}
}

// RequestIDHeader carries the request ID in and out.
const RequestIDHeader = "X-Request-Id"

// validRequestID bounds what an inbound X-Request-Id may contain, so a caller
// can't inject log lines or oversized values. Anything else is replaced.
var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

type requestIDKey struct{}

// requestID keeps a well-formed inbound X-Request-Id or generates a random
// one (128 bits, no hostname or counter), stores it in the context and echoes
// it in the response header.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if !validRequestID.MatchString(id) {
			id = rand.Text()
		}
		w.Header().Set(RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

// RequestID returns the request ID stored by the router, or "" outside a request.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}
