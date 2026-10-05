package payments

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/svout/agentgate/gateway/internal/httpx"
	"github.com/svout/agentgate/gateway/internal/testutil"
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
		req.Header.Set(IdempotencyKeyHeader, key)
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

	// Subtask 7 checks the key but stores nothing (subtask 8 does).
	var keys int
	if err := a.pool.QueryRow(t.Context(), "SELECT count(*) FROM idempotency_keys").Scan(&keys); err != nil || keys != 0 {
		t.Fatalf("idempotency_keys rows = %d (%v), want 0", keys, err)
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
