package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/svout/strata/gateway/internal/config"
	"github.com/svout/strata/gateway/internal/payments"
)

// specPath is the gateway's OpenAPI file (ADR-129), relative to this package.
const specPath = "../../api/openapi.json"

// TestOpenAPI_RoutesMatchMounted requires the spec's operations to be
// exactly the routes the gateway serves, so a route added in code without
// its spec entry (or the other way round) fails here (ADR-129).
//
// It walks in two levels. chi.Walk can't see through a mount whose handler
// isn't a chi router, and both APIs are mounted behind gates (wire.go), so
// the walk of compose's router yields the gates as "/prefix/*" catch-alls.
// Each prefix is then paired with the handler the test assumes is behind
// that gate, and that handler's routes are walked under the prefix. The
// pairing mirrors wireSettlement (wire.go sets the gates to Service.Handler
// and Service.Webhook) but doesn't check it: a change to what wireSettlement
// sets must be made in behind as well.
func TestOpenAPI_RoutesMatchMounted(t *testing.T) {
	t.Parallel()
	log := slog.New(slog.DiscardHandler)
	cfg := config.Config{WebhookRate: config.DefaultWebhookRate, WebhookBurst: config.DefaultWebhookBurst}
	pool := lazyPool(t, "8")
	m, err := compose(cfg, pool, log)
	if err != nil {
		t.Fatal(err)
	}
	svc := payments.New(pool)
	behind := map[string]http.Handler{
		"/v1/payments":      svc.Handler(log),
		"/v1/rail/webhooks": svc.Webhook(log),
	}

	got := map[string]bool{}
	mounts := map[string]bool{}
	walk(t, m.router, func(method, route string) {
		prefix, isMount := strings.CutSuffix(route, "/*")
		if !isMount {
			got[method+" "+route] = true
			return
		}
		if mounts[prefix] {
			return // a mount answers every method; walk it once
		}
		mounts[prefix] = true
		inner, ok := behind[prefix]
		if !ok {
			t.Errorf("mount %s has no handler in this test: add it to behind", prefix)
			return
		}
		walk(t, inner, func(method, route string) {
			got[method+" "+strings.TrimSuffix(prefix+route, "/")] = true
		})
	})
	for prefix := range behind {
		if !mounts[prefix] {
			t.Errorf("%s is not mounted any more: remove it from behind", prefix)
		}
	}

	want := specOperations(t)
	if missing, extra := diffKeys(got, want), diffKeys(want, got); len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("served but not in %s: %v\nin the spec but not served: %v", specPath, missing, extra)
	}
}

// TestOpenAPI_UnmatchedGetProblems pins the spec's claim about requests it
// doesn't document (SPEC-002 15a), on the gateway as compose builds it,
// with the gates closed, then open:
//   - a method outside GET, HEAD and POST is a 501 not_implemented on every
//     path, documented or not, gates open or closed, with no Allow;
//   - while a gate is closed, GET, HEAD or POST under its mount is a 503
//     try_again_later with Retry-After;
//   - once open, GET, HEAD or POST on a documented path that doesn't take
//     it is a 405 method_not_allowed whose Allow is exactly the spec's
//     methods there (also with a trailing slash on /v1/payments, which
//     chi's Mount routes to the same handler);
//   - GET, HEAD or POST on an undocumented path is a 404 not_found.
//
// served repeats httpx's fixed list on purpose: it is the contract, and a
// change to it must change the spec's wording and this test together. The
// spec's own methods must be in it too, or they could never be reached.
func TestOpenAPI_UnmatchedGetProblems(t *testing.T) {
	t.Parallel()
	log := slog.New(slog.DiscardHandler)
	cfg := config.Config{WebhookRate: config.DefaultWebhookRate, WebhookBurst: config.DefaultWebhookBurst}
	pool := lazyPool(t, "8")
	gw, err := compose(cfg, pool, log)
	if err != nil {
		t.Fatal(err)
	}
	served := []string{http.MethodGet, http.MethodHead, http.MethodPost}
	byPath := map[string][]string{}
	for op := range specOperations(t) {
		method, path, _ := strings.Cut(op, " ")
		if !slices.Contains(served, method) {
			t.Errorf("%s documents %s, which the gateway answers with 501", specPath, op)
		}
		byPath[path] = append(byPath[path], method)
	}
	// Two unrecognised methods, then every standard one outside served.
	unserved := []string{"PROPFIND", "FOO", http.MethodConnect}
	for _, m := range httpMethods {
		if m := strings.ToUpper(m); !slices.Contains(served, m) {
			unserved = append(unserved, m)
		}
	}
	fill := strings.NewReplacer("{id}", "0b9a3c1e-4f2d-4a7b-9c8e-1d2f3a4b5c6d", "{rail}", "fake")

	check := func(t *testing.T, method, path string, status int, code, allow string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		gw.router.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		var body struct {
			Status int    `json:"status"`
			Code   string `json:"code"`
		}
		if rec.Code != status || rec.Header().Get("Content-Type") != "application/problem+json" ||
			json.Unmarshal(rec.Body.Bytes(), &body) != nil || body.Code != code || body.Status != status {
			t.Errorf("%s %s: got %d %s %s, want %d %s", method, path, rec.Code,
				rec.Header().Get("Content-Type"), rec.Body, status, code)
		}
		if got := rec.Header().Get("Allow"); got != allow {
			t.Errorf("%s %s: Allow = %q, want %q", method, path, got, allow)
		}
		if rec.Header().Get("X-Request-Id") == "" || rec.Header().Get("X-Content-Type-Options") != "nosniff" ||
			rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s %s: missing X-Request-Id or a safety header: %v", method, path, rec.Header())
		}
		return rec
	}

	undocumented := []string{"/", "/v1", "/healthzx", "/v1/payments/a/b", "/v1/rail/webhooks", "/v1/rail/webhooks/fake/x"}
	all := slices.Clone(undocumented)
	for path := range byPath {
		all = append(all, fill.Replace(path))
	}
	// The 501 comes before the gates, so it holds while they are closed too.
	for _, path := range all {
		for _, m := range unserved {
			check(t, m, path, http.StatusNotImplemented, "not_implemented", "")
		}
	}
	// A closed gate answers before its API's router, so no 404 or 405 yet.
	for _, path := range all {
		if !strings.HasPrefix(path, "/v1/payments") && !strings.HasPrefix(path, "/v1/rail/webhooks") {
			continue
		}
		for _, m := range served {
			if rec := check(t, m, path, http.StatusServiceUnavailable, "try_again_later", ""); rec.Header().Get("Retry-After") == "" {
				t.Errorf("%s %s: 503 without Retry-After", m, path)
			}
		}
	}

	// No request here reaches a handler, so the lazy pool is never dialled.
	svc := payments.New(pool)
	gw.pay.set(svc.Handler(log))
	gw.hooks.set(svc.Webhook(log))
	for _, path := range undocumented {
		for _, m := range served {
			check(t, m, path, http.StatusNotFound, "not_found", "")
		}
	}
	for path, methods := range byPath {
		var allow []string
		for _, m := range served {
			if slices.Contains(methods, m) {
				allow = append(allow, m)
			}
		}
		paths := []string{fill.Replace(path)}
		if path == "/v1/payments" {
			paths = append(paths, path+"/")
		}
		for _, p := range paths {
			for _, m := range served {
				if !slices.Contains(methods, m) {
					check(t, m, p, http.StatusMethodNotAllowed, "method_not_allowed", strings.Join(allow, ", "))
				}
			}
		}
	}
	for _, path := range all {
		for _, m := range unserved {
			check(t, m, path, http.StatusNotImplemented, "not_implemented", "")
		}
	}
}

// walk calls fn for each method and route of h, which must be a chi router.
func walk(t *testing.T, h http.Handler, fn func(method, route string)) {
	t.Helper()
	routes, ok := h.(chi.Routes)
	if !ok {
		t.Fatalf("%T is not a chi router, so its routes can't be listed", h)
	}
	err := chi.Walk(routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		fn(method, route)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// specOperations reads the spec's "METHOD /path" set. Path parameters are
// written {name} in both OpenAPI and chi, so the paths compare as they are.
func specOperations(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("%s: %v", specPath, err)
	}
	ops := map[string]bool{}
	for path, item := range spec.Paths {
		for method := range item {
			// A path item can also hold "parameters", "summary" and the like;
			// only the HTTP methods are operations, and OpenAPI writes them
			// in lower case, so "GET" is not one.
			if slices.Contains(httpMethods, method) {
				ops[strings.ToUpper(method)+" "+path] = true
			}
		}
	}
	return ops
}

// httpMethods are the operation keys of an OpenAPI 3.1 path item.
var httpMethods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

// diffKeys returns the keys of a that b lacks, sorted.
func diffKeys(a, b map[string]bool) []string {
	var out []string
	for k := range a {
		if !b[k] {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}
