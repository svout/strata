package httpx

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
		"api_key": true, "Authorization": true, "stripe_webhook_secret": true,
		"card_number": true, "pan": true, "client-token": true,
		"span_id": false, "company": false, "payment_id": false, "amount": false, "request_id": false,
	} {
		if got := isSensitive(key); got != want {
			t.Errorf("isSensitive(%q) = %v, want %v", key, got, want)
		}
	}
}
