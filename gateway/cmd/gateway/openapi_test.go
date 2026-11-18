package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
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
