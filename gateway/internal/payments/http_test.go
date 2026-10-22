package payments

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	"github.com/redis/go-redis/v9"

	"github.com/svout/strata/gateway/internal/httpx"
	"github.com/svout/strata/gateway/internal/idem"
	"github.com/svout/strata/gateway/internal/ledger"
	"github.com/svout/strata/gateway/internal/rail"
	"github.com/svout/strata/gateway/internal/store"
	"github.com/svout/strata/gateway/internal/testutil"
)

// testAPI mounts the handler on the real httpx router, as cmd/gateway does.
type testAPI struct {
	t      testing.TB
	pool   *pgxpool.Pool
	router *chi.Mux
	log    *bytes.Buffer
	payer  uuid.UUID
	payee  uuid.UUID
}

func newTestAPI(t testing.TB) *testAPI {
	t.Helper()
	a, _ := newTestAPIWith(t, nil)
	return a
}

// newTestAPIWith is newTestAPI with the Redis in-flight lock on rdb, as
// cmd/gateway wires it; a nil rdb means no lock. Payments settle on the
// internal rail.
func newTestAPIWith(t testing.TB, rdb *redis.Client) (*testAPI, *idem.RedisLocker) {
	t.Helper()
	var buf bytes.Buffer
	a, locker := newTestAPILogTo(t, rdb, &buf, internalRail)
	a.log = &buf
	return a, locker
}

// railFor builds the rail a test API settles on; nil means no rail.
type railFor func(testing.TB, *pgxpool.Pool) rail.Adapter

func internalRail(t testing.TB, pool *pgxpool.Pool) rail.Adapter {
	return rail.NewInternal(pool, clearingFor(t, pool, rail.InternalName))
}

// clearingFor looks up the clearing account of the rail named name, as
// cmd/gateway does at startup (ADR-125).
func clearingFor(t testing.TB, pool *pgxpool.Pool, name string) ledger.ClearingAccountID {
	t.Helper()
	id, err := ledger.ClearingAccount(t.Context(), pool, name)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// newTestAPILogTo is newTestAPIWith logging to w, settling on newRail (nil:
// no rail, payments stay created); a.log is nil. Benchmarks pass io.Discard,
// so an ever-growing buffer isn't part of what they time.
func newTestAPILogTo(t testing.TB, rdb *redis.Client, w io.Writer, newRail railFor) (*testAPI, *idem.RedisLocker) {
	t.Helper()
	pool := testutil.Postgres(t)
	log := httpx.NewLogger(w, slog.LevelInfo)
	var (
		opts   = []Option{WithLogger(log)}
		locker *idem.RedisLocker
	)
	if rdb != nil {
		locker = idem.NewRedisLocker(rdb, log)
		opts = append(opts, WithLocker(locker))
	}
	if newRail != nil {
		r := newRail(t, pool)
		opts = append(opts, WithRail(r, clearingFor(t, pool, r.Name())))
	}
	r := httpx.NewRouter(log)
	r.Mount("/v1/payments", New(pool, opts...).Handler(log))
	return &testAPI{t: t, pool: pool, router: r,
		payer: newAccount(t.Context(), t, pool), payee: newAccount(t.Context(), t, pool)}, locker
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
	// The 201 is the snapshot at creation: settling, with its rail reference
	// (the outbox row). The rail runs after it is stored.
	want := map[string]any{"state": "settling", "amount": 1250.0, "currency": "GBP", "rail": "internal",
		"payer_account_id": a.payer.String(), "payee_account_id": a.payee.String(),
		"mandate_id": mandate.String(), "rail_ref": "pay_" + id, "failure_reason": nil}
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
	if len(created) != 12 {
		t.Errorf("response has %d members, want 12: %v", len(created), created)
	}

	get := a.do(http.MethodGet, "/v1/payments/"+id, "", "")
	var got map[string]any
	if get.Code != http.StatusOK || json.Unmarshal(get.Body.Bytes(), &got) != nil {
		t.Fatalf("GET = %d: %s", get.Code, get.Body)
	}
	// GET is the current state: settled by the time POST returned.
	for k, v := range created {
		switch k {
		case "state":
			v = "settled"
		case "updated_at":
			continue
		}
		if got[k] != v {
			t.Errorf("GET %s = %v, want %v", k, got[k], v)
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
	for _, want := range []string{`"msg":"payment created"`, `"payment_id":"` + p.ID.String() + `"`, `"amount":1250`,
		// The settlement log (subtask 10a-i) carries the id as text too.
		`"msg":"payment settled","payment_id":"` + p.ID.String() + `"`} {
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
// exactly one payment and one key, and all 100 get the same response: on
// PostgreSQL alone, with the Redis lock, and with Redis down (fail open).
func TestHTTP_AC3_ConcurrentSameKeyExactlyOnce(t *testing.T) {
	t.Parallel()
	const n = 100
	for name, tc := range map[string]struct {
		rdb  func(testing.TB) *redis.Client
		down bool
	}{
		"postgres only": {rdb: func(testing.TB) *redis.Client { return nil }},
		// Under a loaded -race run some Redis calls exceed 50 ms and fail
		// open, so this case doesn't assert Degraded = 0;
		// TestHTTP_LockWaitAndTimeout proves the lock makes requests wait.
		"redis lock": {rdb: testutil.Redis},
		"redis down": {rdb: downRedis, down: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			a, locker := newTestAPIWith(t, tc.rdb(t))
			concurrentSameKey(t, a, n)
			if tc.down && locker.Degraded() != n {
				t.Fatalf("Degraded = %d, want %d", locker.Degraded(), n)
			}
		})
	}
}

// downRedis is a client whose server is unreachable.
func downRedis(t testing.TB) *redis.Client {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", ContextTimeoutEnabled: true})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func concurrentSameKey(t *testing.T, a *testAPI, n int) {
	t.Helper()
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
	// AC3's ledger clause: the payment settled once, so two two-leg
	// postings: the payer's (payer -> clearing) and the internal rail's
	// (clearing -> payee), ADR-123. Replays never call the rail.
	var entries int
	var state string
	if err := a.pool.QueryRow(ctx, "SELECT count(*) FROM ledger_entries").Scan(&entries); err != nil {
		t.Fatal(err)
	}
	if err := a.pool.QueryRow(ctx, "SELECT state FROM payments").Scan(&state); err != nil {
		t.Fatal(err)
	}
	if entries != 4 || state != string(Settled) {
		t.Fatalf("ledger entries = %d, state = %s; want 4 and settled", entries, state)
	}
	if c, r := strings.Count(a.log.String(), `"msg":"payment created"`), strings.Count(a.log.String(), `"msg":"payment replayed"`); c != 1 || r != n-1 {
		t.Fatalf("log: %d created, %d replayed; want 1 and %d", c, r, n-1)
	}
}

// A request whose key is locked waits for the holder and then runs; one whose
// wait outlasts its deadline gets 503 request_timeout and creates nothing.
func TestHTTP_LockWaitAndTimeout(t *testing.T) {
	t.Parallel()
	a, locker := newTestAPIWith(t, testutil.Redis(t))
	key := idem.Key{Scope: idem.AnonymousCaller, Value: "k-locked"}
	release, err := locker.Lock(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}

	short, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	req := httptest.NewRequestWithContext(short, http.MethodPost, "/v1/payments", strings.NewReader(a.body("")))
	req.Header.Set(idem.Header, key.Value)
	timedOut := httptest.NewRecorder()
	a.router.ServeHTTP(timedOut, req)
	assertProblem(t, timedOut, http.StatusServiceUnavailable, "request_timeout")
	if timedOut.Header().Get("Retry-After") != "1" {
		t.Fatalf("Retry-After = %q, want 1", timedOut.Header().Get("Retry-After"))
	}

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- a.do(http.MethodPost, "/v1/payments", key.Value, a.body("")) }()
	select {
	case rec := <-done:
		t.Fatalf("request finished (%d) while its key was locked", rec.Code)
	case <-time.After(150 * time.Millisecond):
	}
	var payments int
	if err := a.pool.QueryRow(t.Context(), "SELECT count(*) FROM payments").Scan(&payments); err != nil || payments != 0 {
		t.Fatalf("payments while locked = %d (%v), want 0", payments, err)
	}
	release()
	select {
	case rec := <-done:
		if rec.Code != http.StatusCreated {
			t.Fatalf("after release = %d: %s, want 201", rec.Code, rec.Body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("request did not finish after the lock was released")
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

// 8b-i should 4: a request that fails after taking the lock releases it, or a
// retry with the same key would wait out the 10 s TTL. invalid_amount fails before the lock is taken; it is a guard row
// in case validation ever moves after the lock.
func TestHTTP_LockReleasedOnFailure(t *testing.T) {
	t.Parallel()
	const key = "k-release"
	cases := []struct {
		name   string
		status int
		code   string
		send   func(t *testing.T, a *testAPI) *httptest.ResponseRecorder
	}{
		{"409 key reused with another body", http.StatusConflict, "idempotency_key_mismatch",
			func(t *testing.T, a *testAPI) *httptest.ResponseRecorder {
				if rec := a.do(http.MethodPost, "/v1/payments", key, a.body("")); rec.Code != http.StatusCreated {
					t.Fatalf("first = %d: %s, want 201", rec.Code, rec.Body)
				}
				return a.do(http.MethodPost, "/v1/payments", key, strings.Replace(a.body(""), "1250", "1251", 1))
			}},
		{"422 unknown account", http.StatusUnprocessableEntity, "account_not_found",
			func(_ *testing.T, a *testAPI) *httptest.ResponseRecorder {
				return a.do(http.MethodPost, "/v1/payments", key, strings.Replace(a.body(""), a.payee.String(), uuid.NewString(), 1))
			}},
		{"422 invalid amount", http.StatusUnprocessableEntity, "invalid_amount",
			func(_ *testing.T, a *testAPI) *httptest.ResponseRecorder {
				return a.do(http.MethodPost, "/v1/payments", key, strings.Replace(a.body(""), "1250", "0", 1))
			}},
		// The table lock stalls the transaction after the Redis lock is taken,
		// so the deadline passes inside it.
		{"503 deadline inside the transaction", http.StatusServiceUnavailable, "request_timeout",
			func(t *testing.T, a *testAPI) *httptest.ResponseRecorder {
				tx, err := a.pool.Begin(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
				if _, err := tx.Exec(t.Context(), "LOCK TABLE idempotency_keys IN ACCESS EXCLUSIVE MODE"); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
				defer cancel()
				req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/payments", strings.NewReader(a.body("")))
				req.Header.Set(idem.Header, key)
				rec := httptest.NewRecorder()
				a.router.ServeHTTP(rec, req)
				return rec
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rdb := testutil.Redis(t) // a database of its own: any key left is this request's lock
			a, _ := newTestAPIWith(t, rdb)
			assertProblem(t, tc.send(t, a), tc.status, tc.code)
			if keys := rdb.Keys(t.Context(), "*").Val(); len(keys) != 0 {
				t.Fatalf("lock left in Redis after %d: %v", tc.status, keys)
			}
		})
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
