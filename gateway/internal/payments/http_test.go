package payments

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/svout/strata/gateway/internal/httpx"
	"github.com/svout/strata/gateway/internal/idem"
	"github.com/svout/strata/gateway/internal/store"
	"github.com/svout/strata/gateway/internal/testutil"
)

// testAPI mounts the handler on the real httpx router, as cmd/gateway does.
type testAPI struct {
	t      *testing.T
	pool   *pgxpool.Pool
	router *chi.Mux
	log    *bytes.Buffer
	payer  uuid.UUID
	payee  uuid.UUID
}

func newTestAPI(t *testing.T) *testAPI {
	t.Helper()
	pool := testutil.Postgres(t)
	var buf bytes.Buffer
	log := httpx.NewLogger(&buf, slog.LevelInfo)
	r := httpx.NewRouter(log)
	r.Mount("/v1/payments", New(pool).Handler(log))
	return &testAPI{t: t, pool: pool, router: r, log: &buf,
		payer: newAccount(t.Context(), t, pool), payee: newAccount(t.Context(), t, pool)}
}

func (a *testAPI) do(method, path, key, body string, header ...string) *httptest.ResponseRecorder {
	a.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set(idem.Header, key)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	a.router.ServeHTTP(rec, req)
	return rec
}

func (a *testAPI) body(fields string) string {
	return `{"amount":1250,"currency":"GBP","payer_account_id":"` + a.payer.String() +
		`","payee_account_id":"` + a.payee.String() + `"` + fields + `}`
}

func TestHTTP_CreateThenGet(t *testing.T) {
	t.Parallel()
	a := newTestAPI(t)
	mandate := uuid.New()

	rec := a.do(http.MethodPost, "/v1/payments", "key-1", a.body(`,"mandate_id":"`+mandate.String()+`"`))
	if rec.Code != http.StatusCreated || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("POST = %d %s: %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	id, _ := created["id"].(string)
	if loc := rec.Header().Get("Location"); loc != "/v1/payments/"+id {
		t.Fatalf("Location = %q, want /v1/payments/%s", loc, id)
	}
	want := map[string]any{"state": "created", "amount": 1250.0, "currency": "GBP", "rail": "internal",
		"payer_account_id": a.payer.String(), "payee_account_id": a.payee.String(),
		"mandate_id": mandate.String(), "rail_ref": nil}
	for k, v := range want {
		if created[k] != v {
			t.Errorf("%s = %v, want %v", k, created[k], v)
		}
	}
	for _, k := range []string{"created_at", "updated_at"} {
		if s, _ := created[k].(string); s == "" {
			t.Errorf("%s missing", k)
		}
	}
	if len(created) != 11 {
		t.Errorf("response has %d members, want 11: %v", len(created), created)
	}

	get := a.do(http.MethodGet, "/v1/payments/"+id, "", "")
	var got map[string]any
	if get.Code != http.StatusOK || json.Unmarshal(get.Body.Bytes(), &got) != nil {
		t.Fatalf("GET = %d: %s", get.Code, get.Body)
	}
	for k, v := range created {
		if got[k] != v {
			t.Errorf("GET %s = %v, POST gave %v", k, got[k], v)
		}
	}

	// The key is stored, completed, with the exact 201 response (ADR-118).
	var (
		status string
		code   int
		body   []byte
	)
	err := a.pool.QueryRow(t.Context(), "SELECT status, response_code, response_body FROM idempotency_keys WHERE key = 'key-1'").Scan(&status, &code, &body)
	if err != nil || status != "completed" || code != http.StatusCreated || string(body)+"\n" != rec.Body.String() {
		t.Fatalf("stored key = %s %d %s (%v), want completed 201 with the response body", status, code, body, err)
	}
}

func TestHTTP_Errors(t *testing.T) {
	t.Parallel()
	a := newTestAPI(t)
	long := strings.Repeat("k", 256)

	for name, tc := range map[string]struct {
		method, path, key, body string
		status                  int
		code                    string
	}{
		"no key":           {"POST", "/v1/payments", "", a.body(""), 400, "idempotency_key_required"},
		"key too long":     {"POST", "/v1/payments", long, a.body(""), 400, "idempotency_key_required"},
		"not json":         {"POST", "/v1/payments", "k", "amount=1", 400, "invalid_request"},
		"unknown field":    {"POST", "/v1/payments", "k", a.body(`,"fee":1`), 400, "invalid_request"},
		"trailing data":    {"POST", "/v1/payments", "k", a.body("") + `{}`, 400, "invalid_request"},
		"trailing brace":   {"POST", "/v1/payments", "k", a.body("") + `}`, 400, "invalid_request"},
		"trailing bracket": {"POST", "/v1/payments", "k", a.body("") + `]`, 400, "invalid_request"},
		"padded too large": {"POST", "/v1/payments", "k", a.body("") + strings.Repeat(" ", maxBodyBytes), 413, "request_too_large"},
		"only currency":    {"POST", "/v1/payments", "k", `{"currency":"GBP"}`, 400, "invalid_request"},
		"bad account uuid": {"POST", "/v1/payments", "k", `{"amount":1,"currency":"GBP","payer_account_id":"x","payee_account_id":"y"}`, 400, "invalid_request"},
		"missing accounts": {"POST", "/v1/payments", "k", `{"amount":1,"currency":"GBP"}`, 400, "invalid_request"},
		"fractional":       {"POST", "/v1/payments", "k", `{"amount":1.5}`, 400, "invalid_request"},
		"too large":        {"POST", "/v1/payments", "k", `{"currency":"` + strings.Repeat("G", maxBodyBytes) + `"}`, 413, "request_too_large"},
		"zero amount":      {"POST", "/v1/payments", "k", strings.Replace(a.body(""), "1250", "0", 1), 422, "invalid_amount"},
		"currency":         {"POST", "/v1/payments", "k", strings.Replace(a.body(""), "GBP", "USD", 1), 422, "unsupported_currency"},
		"same account":     {"POST", "/v1/payments", "k", strings.Replace(a.body(""), a.payee.String(), a.payer.String(), 1), 422, "same_account"},
		"unknown account":  {"POST", "/v1/payments", "k", strings.Replace(a.body(""), a.payee.String(), uuid.NewString(), 1), 422, "account_not_found"},
		"bad id":           {"GET", "/v1/payments/not-a-uuid", "", "", 400, "invalid_payment_id"},
		"unknown id":       {"GET", "/v1/payments/" + uuid.NewString(), "", "", 404, "payment_not_found"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := a.do(tc.method, tc.path, tc.key, tc.body)
			assertProblem(t, rec, tc.status, tc.code)
		})
	}

	t.Run("255-char key accepted", func(t *testing.T) {
		if rec := a.do("POST", "/v1/payments", long[:255], a.body("")); rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body)
		}
	})
}

// A database failure is a fixed 500 problem; the error is logged, not sent.
func TestHTTP_DatabaseFailure(t *testing.T) {
	t.Parallel()
	a := newTestAPI(t)
	a.pool.Close()

	rec := a.do("POST", "/v1/payments", "k", a.body(""))
	assertProblem(t, rec, http.StatusInternalServerError, "internal_error")
	if !strings.Contains(a.log.String(), `"msg":"payment request failed"`) {
		t.Fatalf("failure not logged:\n%s", a.log)
	}
}

// AC8: the payment id is logged as text (uuid.UUID alone logs as OMITTED),
// and secrets a caller sends never reach the log or a response.
func TestHTTP_LogsIDAsTextAndNoSecrets(t *testing.T) {
	t.Parallel()
	a := newTestAPI(t)
	const secret = "sk_live_payments_4242"

	rec := a.do("POST", "/v1/payments", "idem-"+secret, a.body(""), "Authorization", "Bearer "+secret)
	var p Payment
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &p) != nil {
		t.Fatalf("POST = %d: %s", rec.Code, rec.Body)
	}
	bad := a.do("POST", "/v1/payments", "k", a.body(`,"card_number":"`+secret+`"`), "Authorization", "Bearer "+secret)

	out := a.log.String()
	for _, want := range []string{`"msg":"payment created"`, `"payment_id":"` + p.ID.String() + `"`, `"amount":1250`} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, "OMITTED") {
		t.Errorf("log has an OMITTED value:\n%s", out)
	}
	for where, s := range map[string]string{"log": out, "error body": bad.Body.String(), "created body": rec.Body.String()} {
		if strings.Contains(s, secret) {
			t.Errorf("secret leaked to %s:\n%s", where, s)
		}
	}
}

// A request whose deadline has passed gets the timeout problem, not a 500.
func TestHTTP_RequestTimeout(t *testing.T) {
	t.Parallel()
	a := newTestAPI(t)
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()

	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/payments/"+uuid.NewString(), nil)
	rec := httptest.NewRecorder()
	a.router.ServeHTTP(rec, req)
	assertProblem(t, rec, http.StatusServiceUnavailable, "request_timeout")
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want \"1\"", got)
	}
	if !strings.Contains(a.log.String(), `"msg":"payment request timed out"`) {
		t.Fatalf("timeout not logged:\n%s", a.log)
	}
}

// A client that disconnected (request context cancelled) is not a server
// error: nothing is written and nothing is logged at Error.
func TestHTTP_ClientCancelled(t *testing.T) {
	t.Parallel()
	a := newTestAPI(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/payments/"+uuid.NewString(), nil)
	rec := httptest.NewRecorder()
	a.router.ServeHTTP(rec, req)
	if rec.Body.Len() != 0 || rec.Header().Get("Content-Type") != "" {
		t.Fatalf("wrote a response to a gone client: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(a.log.String(), `"level":"ERROR"`) {
		t.Fatalf("cancellation logged as an error:\n%s", a.log)
	}
	if !strings.Contains(a.log.String(), `"status":0`) {
		t.Fatalf("request log should record status 0:\n%s", a.log)
	}
}

// AC3: 100 concurrent requests with the same Idempotency-Key and body create
// exactly one payment and one key, and all 100 get the same response.
func TestHTTP_AC3_ConcurrentSameKeyExactlyOnce(t *testing.T) {
	t.Parallel()
	a := newTestAPI(t)
	const n = 100
	body := a.body("")

	type result struct {
		code     int
		body     string
		location string
	}
	results := make([]result, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			rec := a.do(http.MethodPost, "/v1/payments", "ac3-key", body)
			results[i] = result{rec.Code, rec.Body.String(), rec.Header().Get("Location")}
		})
	}
	close(start)
	wg.Wait()

	for i, r := range results {
		if r.code != http.StatusCreated || r != results[0] {
			t.Fatalf("response %d = %+v, want the same 201 as response 0 (%+v)", i, r, results[0])
		}
	}
	var payments, keys int
	ctx := t.Context()
	if err := a.pool.QueryRow(ctx, "SELECT count(*) FROM payments").Scan(&payments); err != nil {
		t.Fatal(err)
	}
	if err := a.pool.QueryRow(ctx, "SELECT count(*) FROM idempotency_keys").Scan(&keys); err != nil {
		t.Fatal(err)
	}
	if payments != 1 || keys != 1 {
		t.Fatalf("payments = %d, keys = %d; want exactly 1 each", payments, keys)
	}
	if c, r := strings.Count(a.log.String(), `"msg":"payment created"`), strings.Count(a.log.String(), `"msg":"payment replayed"`); c != 1 || r != n-1 {
		t.Fatalf("log: %d created, %d replayed; want 1 and %d", c, r, n-1)
	}
}

// AC3: the same key with a different body is 409, and creates nothing.
func TestHTTP_SameKeyDifferentBody(t *testing.T) {
	t.Parallel()
	a := newTestAPI(t)
	first := a.do(http.MethodPost, "/v1/payments", "k-mismatch", a.body(""))
	if first.Code != http.StatusCreated {
		t.Fatalf("first = %d: %s", first.Code, first.Body)
	}
	changed := strings.Replace(a.body(""), "1250", "1251", 1)
	assertProblem(t, a.do(http.MethodPost, "/v1/payments", "k-mismatch", changed), http.StatusConflict, "idempotency_key_mismatch")

	// Same body with different whitespace and member order is the same request.
	reordered := `{ "payee_account_id":"` + a.payee.String() + `", "currency":"GBP", "amount":1250, "payer_account_id":"` + a.payer.String() + `" }`
	replay := a.do(http.MethodPost, "/v1/payments", "k-mismatch", reordered)
	if replay.Code != http.StatusCreated || replay.Body.String() != first.Body.String() {
		t.Fatalf("reordered body: %d %s, want the replayed 201", replay.Code, replay.Body)
	}
	var payments int
	if err := a.pool.QueryRow(t.Context(), "SELECT count(*) FROM payments").Scan(&payments); err != nil || payments != 1 {
		t.Fatalf("payments = %d (%v), want 1", payments, err)
	}
}

// Only a 201 is stored: a failed request leaves the key free, so a corrected
// retry with the same key succeeds.
func TestHTTP_FailureIsNotStored(t *testing.T) {
	t.Parallel()
	a := newTestAPI(t)
	unknown := strings.Replace(a.body(""), a.payee.String(), uuid.NewString(), 1)
	assertProblem(t, a.do(http.MethodPost, "/v1/payments", "k-retry", unknown), http.StatusUnprocessableEntity, "account_not_found")
	assertProblem(t, a.do(http.MethodPost, "/v1/payments", "k-retry", strings.Replace(a.body(""), "1250", "0", 1)), http.StatusUnprocessableEntity, "invalid_amount")

	var keys int
	if err := a.pool.QueryRow(t.Context(), "SELECT count(*) FROM idempotency_keys").Scan(&keys); err != nil || keys != 0 {
		t.Fatalf("keys after failures = %d (%v), want 0", keys, err)
	}
	if rec := a.do(http.MethodPost, "/v1/payments", "k-retry", a.body("")); rec.Code != http.StatusCreated {
		t.Fatalf("corrected retry = %d: %s, want 201", rec.Code, rec.Body)
	}
}

// Decision 6: a replay is byte-identical and logged at Info with the payment
// id and the key's digest, never the key.
func TestHTTP_ReplayLogged(t *testing.T) {
	t.Parallel()
	a := newTestAPI(t)
	const key = "replay-sk_live_keytext"
	first := a.do(http.MethodPost, "/v1/payments", key, a.body(""))
	again := a.do(http.MethodPost, "/v1/payments", key, a.body(""))
	if again.Code != first.Code || again.Body.String() != first.Body.String() || again.Header().Get("Location") != first.Header().Get("Location") {
		t.Fatalf("replay differs: %d %s vs %d %s", again.Code, again.Body, first.Code, first.Body)
	}
	var p Payment
	if err := json.Unmarshal(first.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	digest := idem.Key{Value: key}.Digest()
	out := a.log.String()
	if !strings.Contains(out, `"msg":"payment replayed","payment_id":"`+p.ID.String()+`","amount":1250,"idempotency_key_digest":"`+digest+`"`) {
		t.Fatalf("replay log line missing:\n%s", out)
	}
	if strings.Contains(out, "sk_live_keytext") {
		t.Fatalf("raw key logged:\n%s", out)
	}
}

// fail maps service errors that are hard to provoke through a real database:
// retry exhaustion (decided by Ivan: 503 try_again_later with Retry-After) and
// a wrapped mismatch.
func TestFail_Mapping(t *testing.T) {
	for name, tc := range map[string]struct {
		err        error
		status     int
		code       string
		retryAfter string
	}{
		"retry limit": {fmt.Errorf("store: 5 attempts: %w", store.ErrRetryLimit), http.StatusServiceUnavailable, "try_again_later", "1"},
		"mismatch":    {fmt.Errorf("wrapped: %w", idem.ErrMismatch), http.StatusConflict, "idempotency_key_mismatch", ""},
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			h := handler{log: httpx.NewLogger(&buf, slog.LevelInfo)}
			rec := httptest.NewRecorder()
			h.fail(rec, httptest.NewRequest(http.MethodPost, "/v1/payments", nil), tc.err)
			assertProblem(t, rec, tc.status, tc.code)
			if got := rec.Header().Get("Retry-After"); got != tc.retryAfter {
				t.Errorf("Retry-After = %q, want %q", got, tc.retryAfter)
			}
			if strings.Contains(buf.String(), "store: 5 attempts") {
				t.Errorf("error text reached the log at the mapped level:\n%s", buf.String())
			}
		})
	}
}

func assertProblem(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	var body struct {
		Status int    `json:"status"`
		Code   string `json:"code"`
	}
	if rec.Code != status || rec.Header().Get("Content-Type") != "application/problem+json" ||
		json.Unmarshal(rec.Body.Bytes(), &body) != nil || body.Code != code || body.Status != status {
		t.Fatalf("got %d %s %s, want %d %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body, status, code)
	}
}
