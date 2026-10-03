package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func ok(context.Context) error { return nil }

func getReadyz(t *testing.T, log *slog.Logger, checks ...ReadyCheck) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	NewRouter(log, checks...).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	var body map[string]any
	if rec.Code != http.StatusNotFound {
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
	}
	return rec, body
}

func TestReadyz_AllOK(t *testing.T) {
	rec, body := getReadyz(t, NewLogger(io.Discard, slog.LevelInfo),
		ReadyCheck{"postgres", ok}, ReadyCheck{"redis", ok})

	if rec.Code != http.StatusOK || body["status"] != "ready" {
		t.Fatalf("got %d %v, want 200 ready", rec.Code, body)
	}
	checks := body["checks"].(map[string]any)
	if checks["postgres"] != "ok" || checks["redis"] != "ok" {
		t.Fatalf("checks = %v", checks)
	}
	if rec.Header().Get("Content-Type") != "application/json" || rec.Header().Get(RequestIDHeader) == "" {
		t.Fatalf("missing JSON content type or request ID: %v", rec.Header())
	}
}

func TestReadyz_OneFailingNamesIt(t *testing.T) {
	rec, body := getReadyz(t, NewLogger(io.Discard, slog.LevelInfo),
		ReadyCheck{"postgres", ok},
		ReadyCheck{"redis", func(context.Context) error { return errors.New("connection refused") }})

	if rec.Code != http.StatusServiceUnavailable || body["status"] != "not_ready" {
		t.Fatalf("got %d %v, want 503 not_ready", rec.Code, body)
	}
	checks := body["checks"].(map[string]any)
	if checks["postgres"] != "ok" || checks["redis"] != "fail" {
		t.Fatalf("checks = %v", checks)
	}
}

func TestReadyz_HangingCheckTimesOut(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	start := time.Now()
	rec, body := getReadyz(t, NewLogger(io.Discard, slog.LevelInfo),
		ReadyCheck{"postgres", func(context.Context) error { <-block; return nil }}) // ignores ctx

	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("took %v, want about %v", elapsed, readyTimeout)
	}
	if rec.Code != http.StatusServiceUnavailable || body["checks"].(map[string]any)["postgres"] != "timeout" {
		t.Fatalf("got %d %v, want 503 with postgres timeout", rec.Code, body)
	}
}

func TestReadyz_PanickingCheckFails(t *testing.T) {
	rec, body := getReadyz(t, NewLogger(io.Discard, slog.LevelInfo),
		ReadyCheck{"redis", func(context.Context) error { panic("boom") }})

	if rec.Code != http.StatusServiceUnavailable || body["checks"].(map[string]any)["redis"] != "fail" {
		t.Fatalf("got %d %v, want 503 with redis fail", rec.Code, body)
	}
}

func TestReadyz_ErrorTextNeverLeaks(t *testing.T) {
	const secret = "postgres://agentgate:hunter2@db.internal:5432/ledger"
	var buf bytes.Buffer
	rec, _ := getReadyz(t, NewLogger(&buf, slog.LevelInfo),
		ReadyCheck{"postgres", func(context.Context) error { return errors.New("dial " + secret + ": refused") }})

	for where, text := range map[string]string{"response": rec.Body.String(), "log": buf.String()} {
		if strings.Contains(text, "hunter2") || strings.Contains(text, "db.internal") {
			t.Errorf("error text leaked into the %s: %s", where, text)
		}
	}
	if !strings.Contains(buf.String(), `"check":"postgres"`) || !strings.Contains(buf.String(), `"reason":"fail"`) {
		t.Errorf("warn log lacks check/reason:\n%s", buf.String())
	}
}

func TestReadyz_NotMountedWithoutChecks(t *testing.T) {
	rec, _ := getReadyz(t, NewLogger(io.Discard, slog.LevelInfo))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when no checks are configured", rec.Code)
	}
}
