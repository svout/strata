package httpx

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"testing/slogtest"
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
		// logged
		"signature_valid": false, "signature_alg": false, "token_count": false, "auth_result": false,
		"mandate_id": false, "payload_digest": false, "signing_key_id": false,
		"span_id": false, "company": false, "payment_id": false, "amount": false,
		"request_id": false, "idempotency_key": false, "": false,
	} {
		if got := isSensitive(key); got != want {
			t.Errorf("isSensitive(%q) = %v, want %v", key, got, want)
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
