package httpx

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"testing/slogtest"

	"github.com/go-chi/chi/v5"
)

func TestHealthz_ReturnsOK(t *testing.T) {
	h := NewRouter(NewLogger(io.Discard, slog.LevelInfo))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("body = %v, want status ok", body)
	}
}

func TestRequestLog_OneLinePerRequestWithoutHeaders(t *testing.T) {
	var buf bytes.Buffer
	h := NewRouter(NewLogger(&buf, slog.LevelInfo))

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Authorization", "Bearer sk_test_should_not_appear")
	h.ServeHTTP(httptest.NewRecorder(), req)

	out := buf.String()
	if strings.Count(out, "\n") != 1 {
		t.Fatalf("want exactly one log line, got:\n%s", out)
	}
	if strings.Contains(out, "sk_test_should_not_appear") {
		t.Fatalf("request log leaked a header value:\n%s", out)
	}
	for _, want := range []string{`"method":"GET"`, `"path":"/healthz"`, `"status":200`, `"request_id"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("log line missing %s:\n%s", want, out)
		}
	}
}

// SPEC-002 AC8: a known secret never reaches the log output.
func TestLogger_RedactsSecrets(t *testing.T) {
	const secret = "sk_test_51Hsecretvalue"
	var buf bytes.Buffer
	log := NewLogger(&buf, slog.LevelInfo)

	log.Info("calling rail",
		slog.String("api_key", secret),
		slog.String("Authorization", "Bearer "+secret),
		slog.String("stripe_webhook_secret", secret),
		slog.String("payment_id", "pay_123"),
	)

	out := buf.String()
	if strings.Contains(out, secret) {
		t.Fatalf("secret leaked into log:\n%s", out)
	}
	if strings.Count(out, redacted) != 3 {
		t.Fatalf("want 3 redacted values, got:\n%s", out)
	}
	if !strings.Contains(out, `"payment_id":"pay_123"`) {
		t.Fatalf("non-sensitive attribute was dropped:\n%s", out)
	}
}

func TestIsSensitive(t *testing.T) {
	for key, want := range map[string]bool{
		// redacted
		"api_key": true, "apiKey": true, "stripeAPIKey": true, "Authorization": true,
		"stripe_webhook_secret": true, "client_secret_len": true, // hard terms ignore keep-suffixes
		"card_number": true, "pan": true, "cvv": true, "client-token": true, "auth": true, "auth_token": true,
		"signature": true, "signing_key": true, "hmac_key": true, "credential": true, "cookie": true,
		"body": true, "payload": true, "mandate": true, "private_key": true, "password": true,
		"dsn": true, "pg_dsn": true, "database_url": true, "DATABASE_URL": true, "databaseURL": true,
		// logged
		"signature_valid": false, "signature_alg": false, "token_count": false, "auth_result": false,
		"mandate_id": false, "payload_digest": false, "signing_key_id": false,
		"span_id": false, "company": false, "payment_id": false, "amount": false,
		"request_id": false, "idempotency_key": false, "": false,

		// open question 9 (7a): new hard terms ignore keep-suffixes
		"cardholder": true, "cardholder_name": true, "cardholderId": true,
		"bearer": true, "bearer_token": true, "bearerToken": true, "jwt": true, "id_jwt": true, "jwt_alg": true,
		"otp": true, "otp_code": true, "accesskey": true, "access_key": true, "awsAccessKeyId": true,
		// card data stays redacted, even partly
		"pan_last4": true, "card_last4": true, "cardLast4": true,
		// session is soft
		"session": true, "session_token": true, "sessionKey": true, "sessions": true,
		"session_id": false, "sessionId": false, "session_count": false,
		// plural and digit forms
		"signatures": true, "apiKeys": true, "api_keys": true, "cvv2": true, "CVV2": true, "tokens": true,
		"bodies": true, "cards": true, "pins": true, "jwts": true, "otps": true, "secrets": true,
		"auth_tokens": true, "token_2": true, "sig1": true,
		"signatures_valid": false, "token_counts": false, "mandate_ids": false, "payload_hashes": false,
		// leaks found at 7a verify: one-word compounds and missing terms
		"totp_code": true, "totp": true, "hotp_secret": true, "passcode": true, "user_passcode": true,
		"sessionkey": true, "pinblock": true, "pin_block": true, "track2": true, "track1_data": true,
		"trackData": true, "trackdata": true,
		"sessionkey_id": false, "track_id": false, "tracking_number": false,
	} {
		if got := isSensitive(key); got != want {
			t.Errorf("isSensitive(%q) = %v, want %v", key, got, want)
		}
	}
}

// Keys the gateway logs routinely, plus ordinary words that end in s or
// contain a short term, must survive the plural, digit and substring rules.
func TestIsSensitive_MustNotRedact(t *testing.T) {
	for _, key := range []string{
		"request_id", "requestId", "payment_id", "paymentID", "amount", "amount_minor", "status", "statuses",
		"state", "from_state", "currency", "passes", "processes", "process", "address", "addresses",
		"rail", "rails", "account_id", "accounts", "entries", "ledger_entry_id", "attempts", "retries",
		"duration", "method", "path", "checks", "idempotency_key", "span_id", "company", "spans",
		"options", "footprint", "pinned", "opinion", "panel", "expansion", "pending", "sha256", "v2",
		"session_id", "mandate_id", "signature_valid", "token_count", "types", "ids", "is_ok",
		"tracking_number", "tracks_id", "topic", "spinner", "opt_in", "author", "authority",
	} {
		if isSensitive(key) {
			t.Errorf("isSensitive(%q) = true, want false", key)
		}
	}
}

func TestLogger_RedactsGroupKeysAndWithGroup(t *testing.T) {
	const secret = "s3cr3t-value"
	var buf bytes.Buffer
	log := NewLogger(&buf, slog.LevelInfo)

	log.Info("grouped",
		slog.Group("auth", slog.String("user", secret)),                      // sensitive group key
		slog.Group("rail", slog.String("api_key", secret), slog.Int("n", 1)), // sensitive member
	)
	log.WithGroup("credential").Info("with group", slog.String("user", secret))
	log.With(slog.String("signing_key", secret)).Info("with attrs")

	out := buf.String()
	if strings.Contains(out, secret) {
		t.Fatalf("secret leaked:\n%s", out)
	}
	if !strings.Contains(out, `"n":1`) {
		t.Fatalf("non-sensitive group member dropped:\n%s", out)
	}
}

type card struct{ Number, Holder string }

type maskedCard struct{ last4 string }

func (c maskedCard) LogValue() slog.Value {
	return slog.GroupValue(slog.String("last4", c.last4), slog.String("pan", "4111111111111111"))
}

func TestLogger_OmitsNonScalarAny(t *testing.T) {
	const secret = "4111111111111111"
	var buf bytes.Buffer
	log := NewLogger(&buf, slog.LevelInfo)

	log.Info("any",
		slog.Any("cfg", map[string]any{"token": secret}),
		slog.Any("c", card{Number: secret, Holder: "A"}),
		slog.Any("masked", maskedCard{last4: "1111"}),
		slog.Any("err", errors.New("rail timeout")),
	)

	out := buf.String()
	if strings.Contains(out, secret) {
		t.Fatalf("secret leaked:\n%s", out)
	}
	for _, want := range []string{
		`"cfg":"[OMITTED map[string]interface {}]"`,
		`"c":"[OMITTED httpx.card]"`,
		`"masked":{"last4":"1111","pan":"[REDACTED]"}`,
		`"err":"rail timeout"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in:\n%s", want, out)
		}
	}
}

type (
	paymentID string
	state     string
	minor     int64
	attempt   uint8
	settled   bool
	rate      float64
)

// A Stringer that must never be called: named scalars log their underlying value.
type loudID string

func (loudID) String() string { return "via-stringer" }

func TestLogger_NamedScalarsLogUnderlyingKind(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(&buf, slog.LevelInfo)

	log.Info("named",
		slog.Any("payment_id", paymentID("pay_123")),
		slog.Any("state", state("settling")),
		slog.Any("amount", minor(-1500)),
		slog.Any("attempt", attempt(3)),
		slog.Any("settled", settled(true)),
		slog.Any("rate", rate(0.5)),
		slog.Any("loud", loudID("raw")),
		slog.Any("session", paymentID("sess_secret")), // redaction by key comes first
	)

	out := buf.String()
	for _, want := range []string{
		`"payment_id":"pay_123"`, `"state":"settling"`, `"amount":-1500`, `"attempt":3`,
		`"settled":true`, `"rate":0.5`, `"loud":"raw"`, `"session":"[REDACTED]"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in:\n%s", want, out)
		}
	}
	for _, bad := range []string{"OMITTED", "via-stringer", "sess_secret"} {
		if strings.Contains(out, bad) {
			t.Errorf("unexpected %s in:\n%s", bad, out)
		}
	}
}

func TestWriteJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, http.StatusCreated, map[string]int{"n": 1})

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"n":1}` {
		t.Fatalf("body = %s", got)
	}
	assertSafetyHeaders(t, rec)
}

// A value json can't encode must not go out as the intended status with an
// empty body: it becomes a fixed 500 problem that quotes nothing from v.
func TestWriteJSON_EncodeFailureIs500Problem(t *testing.T) {
	for name, v := range map[string]any{
		"NaN":     math.NaN(),
		"channel": map[string]any{"c": make(chan int)},
	} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			WriteJSON(rec, http.StatusCreated, v)

			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
				t.Fatalf("Content-Type = %q, want application/problem+json", ct)
			}
			var body map[string]any
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if body["code"] != "internal_error" || body["status"] != float64(500) || len(body) != 4 {
				t.Fatalf("body = %v, want the fixed internal_error problem", body)
			}
			assertSafetyHeaders(t, rec)
		})
	}
}

func assertSafetyHeaders(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

func TestWriteProblem(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteProblem(rec, http.StatusNotFound, "payment_not_found", "Payment not found")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", ct)
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	// Exactly these members: no "detail", "instance" or anything carrying error text.
	want := map[string]any{"type": "about:blank", "title": "Payment not found", "status": float64(404), "code": "payment_not_found"}
	if len(body) != len(want) {
		t.Fatalf("body = %v, want exactly %v", body, want)
	}
	for k, v := range want {
		if body[k] != v {
			t.Errorf("body[%q] = %v, want %v", k, body[k], v)
		}
	}
	assertSafetyHeaders(t, rec)
}

// 7b mounts the payment API on NewRouter; mounted routes must get the same
// request ID, request log and panic recovery as the router's own.
func TestNewRouter_MountedSubrouterGetsMiddleware(t *testing.T) {
	var buf bytes.Buffer
	r := NewRouter(NewLogger(&buf, slog.LevelInfo))
	sub := chi.NewRouter()
	sub.Get("/{id}", func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, http.StatusOK, map[string]string{"request_id": RequestID(r.Context())})
	})
	sub.Get("/boom/x", func(http.ResponseWriter, *http.Request) { panic("boom") })
	r.Mount("/v1/things", sub)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/things/abc", nil))
	id := rec.Header().Get(RequestIDHeader)
	if rec.Code != http.StatusOK || !generatedID.MatchString(id) {
		t.Fatalf("status = %d, %s = %q; want 200 and a generated id", rec.Code, RequestIDHeader, id)
	}
	if !strings.Contains(rec.Body.String(), id) {
		t.Fatalf("handler did not see the request id in its context: %s", rec.Body.String())
	}
	if !strings.Contains(buf.String(), `"path":"/v1/things/abc","status":200`) {
		t.Fatalf("mounted request not logged:\n%s", buf.String())
	}

	buf.Reset()
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/things/boom/x", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("panic: status = %d, want 500", rec.Code)
	}
	if !strings.Contains(buf.String(), `"status":500`) {
		t.Fatalf("recovered panic not logged as 500:\n%s", buf.String())
	}
}

// AC8: a panic becomes a 500 internal_error problem; the log has the request
// ID and stack under a fixed message, and the panic value appears nowhere: not
// in the log, the body or stderr (where chi's Recoverer used to print it).
func TestRecoverer_HidesPanicValue(t *testing.T) {
	const secret = "sk_live_recoverer_4242"
	for name, v := range map[string]any{
		"string": secret,
		"error":  errors.New("dial postgres://app:" + secret + "@db"),
		"struct": struct{ Card string }{secret},
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			r := NewRouter(NewLogger(&buf, slog.LevelInfo))
			r.Get("/boom", func(http.ResponseWriter, *http.Request) { panic(v) })

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/boom", nil)
			req.Header.Set(RequestIDHeader, "req-panic-1")
			stderr := captureStderr(t, func() { r.ServeHTTP(rec, req) })

			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
				t.Fatalf("Content-Type = %q, want application/problem+json", ct)
			}
			if got := rec.Body.String(); got != string(internalErrorBody) {
				t.Fatalf("body = %s, want the fixed internal_error problem", got)
			}
			assertSafetyHeaders(t, rec)

			out := buf.String()
			for _, want := range []string{`"msg":"handler panicked"`, `"request_id":"req-panic-1"`, `"stack":"goroutine `, `"status":500`} {
				if !strings.Contains(out, want) {
					t.Errorf("log missing %s:\n%s", want, out)
				}
			}
			for where, got := range map[string]string{"log": out, "body": rec.Body.String(), "stderr": stderr} {
				if strings.Contains(got, secret) {
					t.Errorf("panic value leaked to %s:\n%s", where, got)
				}
			}
			if stderr != "" {
				t.Errorf("recoverer wrote to stderr:\n%s", stderr)
			}
		})
	}
}

// net/http uses http.ErrAbortHandler to abort a response silently; the
// recoverer must hand it back rather than turn it into a 500.
func TestRecoverer_RepanicsErrAbortHandler(t *testing.T) {
	var buf bytes.Buffer
	r := NewRouter(NewLogger(&buf, slog.LevelInfo))
	r.Get("/abort", func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })

	defer func() {
		if got := recover(); got != http.ErrAbortHandler { //nolint:errorlint // identity, as net/http checks
			t.Fatalf("recovered %v, want http.ErrAbortHandler re-panicked", got)
		}
		if strings.Contains(buf.String(), "handler panicked") {
			t.Errorf("abort logged as a panic:\n%s", buf.String())
		}
	}()
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/abort", nil))
	t.Fatal("ServeHTTP returned; want ErrAbortHandler re-panicked")
}

// A handler that already started its response can't be given a problem body:
// the response keeps what was sent and gets nothing appended. Flush and a
// bare Write count as started, though chi's writer still reports Status 0
// after a Flush.
func TestRecoverer_AfterResponseStartedAddsNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		handler  func(http.ResponseWriter)
		wantCode int
		wantBody string
	}{
		"WriteHeader": {func(w http.ResponseWriter) { w.WriteHeader(http.StatusAccepted) }, http.StatusAccepted, ""},
		"Write":       {func(w http.ResponseWriter) { _, _ = w.Write([]byte("partial")) }, http.StatusOK, "partial"},
		"Flush":       {func(w http.ResponseWriter) { _ = http.NewResponseController(w).Flush() }, http.StatusOK, ""},
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			r := NewRouter(NewLogger(&buf, slog.LevelInfo))
			r.Get("/late", func(w http.ResponseWriter, _ *http.Request) {
				tc.handler(w)
				panic("late")
			})

			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/late", nil))

			if rec.Code != tc.wantCode || rec.Body.String() != tc.wantBody {
				t.Fatalf("got %d %q, want %d %q untouched", rec.Code, rec.Body.String(), tc.wantCode, tc.wantBody)
			}
			if !strings.Contains(buf.String(), `"msg":"handler panicked"`) {
				t.Fatalf("late panic not logged:\n%s", buf.String())
			}
		})
	}
}

// captureStderr runs fn with os.Stderr redirected and returns what it wrote.
// Tests in this package don't run in parallel, so swapping the global is safe.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(rd) // drains concurrently, so a large write can't block fn
		done <- string(b)
	}()
	orig := os.Stderr
	os.Stderr = wr
	defer func() { // also runs if fn fails the test, so the reader always ends
		os.Stderr = orig
		_ = wr.Close()
	}()
	fn()
	os.Stderr = orig
	_ = wr.Close()
	out := <-done
	_ = rd.Close()
	return out
}

func TestRequestID_GeneratedWhenAbsent(t *testing.T) {
	var buf bytes.Buffer
	h := NewRouter(NewLogger(&buf, slog.LevelInfo))
	host, _ := os.Hostname()

	ids := map[string]bool{}
	for range 2 {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		id := rec.Header().Get(RequestIDHeader)
		if !generatedID.MatchString(id) {
			t.Fatalf("generated id %q: want >= 26 base32 chars", id)
		}
		if host != "" && strings.Contains(id, host) {
			t.Fatalf("id %q contains hostname", id)
		}
		if !strings.Contains(buf.String(), `"request_id":"`+id+`"`) {
			t.Fatalf("log line lacks id %s:\n%s", id, buf.String())
		}
		ids[id] = true
	}
	if len(ids) != 2 {
		t.Fatal("two requests got the same id")
	}
}

func TestRequestID_InboundValidation(t *testing.T) {
	h := NewRouter(NewLogger(io.Discard, slog.LevelInfo))
	tests := []struct {
		in   string
		keep bool
	}{
		{"abc-123_DEF.9", true},
		{strings.Repeat("a", 64), true},
		{"", false},
		{strings.Repeat("a", 65), false},
		{"has space", false},
		{"line\nbreak", false},
		{`quo"te`, false},
		{"ünïcode", false},
	}
	for _, tt := range tests {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		req.Header.Set(RequestIDHeader, tt.in)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		got := rec.Header().Get(RequestIDHeader)
		if tt.keep && got != tt.in {
			t.Errorf("inbound %q: got %q, want it kept", tt.in, got)
		}
		if !tt.keep && (got == tt.in || !generatedID.MatchString(got)) {
			t.Errorf("inbound %q: got %q, want a generated id", tt.in, got)
		}
	}
}

// rand.Text documents "at least 128 bits" of base32, not an exact length.
var generatedID = regexp.MustCompile(`^[A-Z2-7]{26,}$`)

type nilErr struct{ msg string }

func (e *nilErr) Error() string { return e.msg } // dereferences: panics on a typed nil

type panicErr struct{}

func (panicErr) Error() string { panic("boom: sk_live_secret") }

func TestLogger_BrokenErrorsDoNotPanic(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(&buf, slog.LevelInfo)

	var typedNil *nilErr
	log.Info("errs", slog.Any("nil_err", error(typedNil)), slog.Any("bad_err", panicErr{}))

	out := buf.String()
	if !strings.Contains(out, `"nil_err":"<nil>"`) {
		t.Errorf("typed-nil error not logged as <nil>:\n%s", out)
	}
	if !strings.Contains(out, `"bad_err":"[error text unavailable: Error() panicked]"`) || strings.Contains(out, "sk_live_secret") {
		t.Errorf("panicking Error() not contained:\n%s", out)
	}
}

func TestLogger_EmptyAttrsAndInlineGroups(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(&buf, slog.LevelInfo)

	log.Info("empty", slog.Attr{}, slog.Group("g"), slog.String("k", "v"))
	log.WithGroup("auth").With(slog.Group("", slog.String("a", "x"))).Info("inline", slog.Group("", slog.String("b", "y")))
	log.Info("inline-ok", slog.Group("", slog.String("c", "z"), slog.String("api_key", "s3")))

	out := buf.String()
	for _, bad := range []string{`"":`, `"g":`, "OMITTED <nil>", "s3"} {
		if strings.Contains(out, bad) {
			t.Errorf("unexpected %s in:\n%s", bad, out)
		}
	}
	for _, want := range []string{`"k":"v"`, `"auth":{"a":"[REDACTED]","b":"[REDACTED]"}`, `"c":"z"`, `"api_key":"[REDACTED]"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in:\n%s", want, out)
		}
	}
}

// The standard handler conformance suite. Attribute values used by slogtest
// are scalars, so the [OMITTED] policy for non-scalar Any doesn't interfere.
func TestRedactHandler_Slogtest(t *testing.T) {
	var buf bytes.Buffer
	h := NewLogger(&buf, slog.LevelDebug).Handler()
	results := func() []map[string]any {
		var ms []map[string]any
		for _, line := range bytes.Split(buf.Bytes(), []byte{'\n'}) {
			if len(line) == 0 {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal(line, &m); err != nil {
				t.Fatal(err)
			}
			ms = append(ms, m)
		}
		return ms
	}
	if err := slogtest.TestHandler(h, results); err != nil {
		t.Fatal(err)
	}
}
