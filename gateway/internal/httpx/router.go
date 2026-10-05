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

// NewRouter builds the gateway's HTTP handler. It serves /healthz (liveness),
// and /readyz when readiness checks are given.
//
// It returns the concrete *chi.Mux so APIs are mounted on it
// (r.Mount("/v1/payments", h)) rather than on a second router: middleware
// added here with Use applies to every route, mounted ones included, so each
// API gets the request ID, request log and panic recovery for free.
func NewRouter(log *slog.Logger, checks ...ReadyCheck) *chi.Mux {
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
	WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// WriteJSON writes v as an application/json response with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	writeBody(w, status, "application/json", v)
}

// problem is an RFC 9457 problem details body. Type is always "about:blank",
// so Title is the HTTP status phrase or a short fixed summary; Code is the
// machine-readable error code agents branch on.
type problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Code   string `json:"code"`
}

// WriteProblem writes an RFC 9457 application/problem+json error. It takes a
// fixed code and title, not an error, on purpose: error text can carry
// internal detail (driver messages, hosts, query strings), and nothing internal
// may reach an agent (gateway/CLAUDE.md). Log the error separately if needed.
func WriteProblem(w http.ResponseWriter, status int, code, title string) {
	writeBody(w, status, "application/problem+json",
		problem{Type: "about:blank", Title: title, Status: status, Code: code})
}

// internalErrorBody is sent when a response body can't be encoded. It is a
// constant, so the fallback itself can't fail.
var internalErrorBody = []byte(`{"type":"about:blank","title":"Internal Server Error","status":500,"code":"internal_error"}` + "\n")

// writeBody marshals v before sending any header, so a value that can't be
// encoded (NaN, a channel, a failing MarshalJSON) becomes a 500 problem
// instead of the intended status with an empty body. The request log records
// the 500; the encode error itself is not logged, as it may quote the value.
// Every API response is marked nosniff and no-store: payment data must not be
// content-sniffed or cached by intermediaries.
func writeBody(w http.ResponseWriter, status int, contentType string, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		status, contentType, body = http.StatusInternalServerError, "application/problem+json", internalErrorBody
	} else {
		body = append(body, '\n') // as json.Encoder did
	}
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body) // the status line is already sent; nothing useful to do on error
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
