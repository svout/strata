package httpx

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
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
// API gets the request ID, request log, panic recovery and request deadline
// for free.
func NewRouter(log *slog.Logger, checks ...ReadyCheck) *chi.Mux {
	return newRouter(log, requestTimeout, checks...)
}

// requestTimeout bounds every request's context, so each query or call made
// with r.Context() has a deadline (gateway/CLAUDE.md). It is half the
// server's WriteTimeout (10 s), so a timeout response can still be written.
const requestTimeout = 5 * time.Second

// newRouter is NewRouter with the timeout as a parameter, for tests.
func newRouter(log *slog.Logger, timeout time.Duration, checks ...ReadyCheck) *chi.Mux {
	r := NewSubRouter()
	r.Use(requestID)
	r.Use(requestLog(log))
	r.Use(recoverer(log))
	r.Use(withTimeout(timeout))
	r.Use(knownMethods)

	r.Get("/healthz", healthz)
	if len(checks) > 0 {
		r.Get("/readyz", readyz(log, checks))
	}
	return r
}

// NewSubRouter returns a router for an API mounted on NewRouter's (as
// payments.Handler is), and is what NewRouter itself starts from. It
// answers an unmatched path with 404 not_found and an unserved method with
// 405 method_not_allowed, both problems, instead of chi's plain-text 404
// and empty 405 (SPEC-002 15a). Each API's router must be built with it,
// not with chi.NewRouter: chi hands a parent's handlers down to a mounted
// router only when the mounted handler is itself a *chi.Mux, and the
// gateway's mounts are wrapped (the gates, the rate limiter). Even a
// directly mounted *chi.Mux would get the parent's 405 handler, which
// looks up the parent's routes, not its own.
//
// It also answers HEAD as GET on a path that has a GET route and no HEAD
// route (SPEC-002 17, RFC 9110 §9.3.2). getHead is added first, so on
// NewRouter's own router it runs outside the request ID, the log and the
// recoverer. It only looks up routes and rewrites the routing method,
// writing nothing, so nothing it does needs the request ID or the log, and
// the lookups are the ones chi itself makes on every request.
func NewSubRouter() *chi.Mux {
	r := chi.NewRouter()
	r.Use(getHead(r))
	r.NotFound(notFound)
	r.MethodNotAllowed(methodNotAllowed(r))
	return r
}

func notFound(w http.ResponseWriter, _ *http.Request) {
	WriteProblem(w, http.StatusNotFound, "not_found", "Not Found")
}

// servedMethods are the methods the gateway knows: the ones it routes (GET,
// POST) and HEAD, which RFC 9110 §9.1 requires a general-purpose server to
// support and getHead answers as GET. It is a fixed list, not inferred
// from the routes (SPEC-002 15a): a route for any other method would never
// be reached, and
// TestOpenAPI_UnmatchedGetProblems fails if the spec documents one.
// OPTIONS is left out on purpose: the API is server to server, so there is
// no browser preflight to answer.
var servedMethods = []string{http.MethodGet, http.MethodHead, http.MethodPost}

// knownMethods answers any method outside servedMethods with 501
// not_implemented, before any route lookup, so the answer is the same on
// every path (RFC 9110 §15.6.2: the server doesn't support the method for
// any resource). A 405 would be wrong for these: it means the method is
// known but not supported by this resource, and it must carry an Allow that
// the root can't compute through a wrapped mount (chi sends a method it
// doesn't route, like PROPFIND, to the root's 405 handler, where a mount's
// catch-all matches every method).
func knownMethods(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !slices.Contains(servedMethods, r.Method) {
			WriteProblem(w, http.StatusNotImplemented, "not_implemented", "Not Implemented")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// getHead routes a HEAD request as GET when mx has a GET route for the path
// and no HEAD route of its own; net/http then drops the body and keeps
// GET's headers. chi's middleware.GetHead is not used: it looks the path up
// in rctx.Routes, which chi sets only at the root router and a mount never
// resets, so it always consults the root's routes. On the root it then
// leaves HEAD /v1/payments/{id} a 405 (the mount's catch-all matches HEAD);
// in a sub-router it checks the mount-relative path against the root, so
// it ignores the sub-router's own HEAD routes and works only while no root
// route happens to match. Looking up mx itself, as methodNotAllowed does,
// is right at every level. A path with no GET route is left alone, so it
// gets its own 404 or 405.
func getHead(mx *chi.Mux) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodHead {
				path := routePath(r)
				if mx.Find(chi.NewRouteContext(), http.MethodHead, path) == "" &&
					mx.Find(chi.NewRouteContext(), http.MethodGet, path) != "" {
					// routeHTTP routes on RouteMethod when it is set; a parent
					// router may already have set it to HEAD, so set it here.
					chi.RouteContext(r.Context()).RouteMethod = http.MethodGet
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// routePath is the path chi routes on: RoutePath inside a mount, else the
// URL's, and "/" for an empty one, as in chi's routeHTTP.
func routePath(r *http.Request) string {
	if rctx := chi.RouteContext(r.Context()); rctx != nil && rctx.RoutePath != "" {
		return rctx.RoutePath
	}
	if r.URL.RawPath != "" {
		return r.URL.RawPath
	}
	if r.URL.Path == "" {
		return "/"
	}
	return r.URL.Path
}

// methodNotAllowed answers 405 with Allow listing the servedMethods mx
// serves on the request's path, HEAD included wherever GET is (getHead
// answers it). chi computes that list for its own 405 but doesn't pass it
// to a custom handler, so it is found again here, one Find per method; that
// runs only on a 405. An empty Allow is legal (RFC 9110 §10.2.1) but tells
// the client nothing, and SPEC-002 15a rules it out, so if none is found (a
// route registered for a method outside servedMethods, which knownMethods
// makes unreachable) the answer is a 404.
func methodNotAllowed(mx *chi.Mux) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := routePath(r)
		served := func(m string) bool { return mx.Find(chi.NewRouteContext(), m, path) != "" }
		var allow []string
		for _, m := range servedMethods {
			if served(m) || (m == http.MethodHead && served(http.MethodGet)) {
				allow = append(allow, m)
			}
		}
		if len(allow) == 0 {
			notFound(w, r)
			return
		}
		w.Header().Set("Allow", strings.Join(allow, ", "))
		WriteProblem(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method Not Allowed")
	}
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

// recoverer turns a handler panic into a 500 internal_error problem. It logs a
// fixed message with the request ID and stack, never the panic value: that can
// be anything the handler held (a card number, a DSN), and chi's Recoverer
// printed it to stderr, past the redacting logger. http.ErrAbortHandler is
// re-panicked so net/http aborts the response as it intends.
//
// It sits inside requestLog, so the request log records the 500. If the
// handler already started the response (a status, a byte, a flush or a
// hijack), nothing more is written: a problem body appended to a partial
// response would corrupt it. startedWriter tracks that itself, because chi's
// writer leaves Status at 0 after a bare Flush or Hijack.
//
// The stack shows frame arguments only as hex words, never dereferenced, so
// strings and errors (the panic value included) can't appear in it.
func recoverer(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sw := &startedWriter{ResponseWriter: w}
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				if rec == http.ErrAbortHandler { //nolint:errorlint // net/http compares by identity too
					panic(rec)
				}
				log.LogAttrs(r.Context(), slog.LevelError, "handler panicked",
					slog.String("request_id", RequestID(r.Context())),
					slog.String("stack", string(debug.Stack())),
				)
				if !sw.started {
					WriteProblem(w, http.StatusInternalServerError, "internal_error", "Internal Server Error")
				}
			}()
			next.ServeHTTP(sw, r)
		})
	}
}

// startedWriter records whether the response has started. It implements
// Flusher and Hijacker itself so handlers keep both, and Unwrap so
// http.ResponseController reaches the underlying writer.
type startedWriter struct {
	http.ResponseWriter
	started bool
}

func (w *startedWriter) WriteHeader(code int) {
	if code >= 200 || code == http.StatusSwitchingProtocols { // other 1xx are interim
		w.started = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *startedWriter) Write(b []byte) (int, error) {
	w.started = true
	return w.ResponseWriter.Write(b)
}

func (w *startedWriter) Flush() {
	w.started = true
	_ = http.NewResponseController(w.ResponseWriter).Flush() // like http.Flusher: no error to report
}

func (w *startedWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.started = true
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

func (w *startedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// withTimeout gives the request context a deadline. It writes nothing: a
// handler sees the context error from its own calls and renders its own
// problem. (chi's middleware.Timeout instead writes a bodyless 504 after the
// handler returns, even over a response the handler already sent.)
func withTimeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
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
