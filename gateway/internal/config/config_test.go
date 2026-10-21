package config

import (
	"errors"
	"log/slog"
	"testing"
)

const testDSN = "postgres://u:p@localhost:5432/db"

// env returns a getenv over m, with DATABASE_URL set unless m overrides it.
func env(m map[string]string) func(string) string {
	return func(k string) string {
		if v, ok := m[k]; ok {
			return v
		}
		if k == "DATABASE_URL" {
			return testDSN
		}
		return ""
	}
}

func TestLoad_DefaultsAndOverrides(t *testing.T) {
	base := Config{Addr: ":8080", LogLevel: slog.LevelInfo, DatabaseURL: testDSN, RedisAddr: "localhost:6379"}
	with := func(f func(*Config)) Config { c := base; f(&c); return c }

	tests := []struct {
		name string
		env  map[string]string
		want Config
	}{
		{"defaults", nil, base},
		{"addr override", map[string]string{"GATEWAY_ADDR": ":9090"}, with(func(c *Config) { c.Addr = ":9090" })},
		{"level lower case", map[string]string{"LOG_LEVEL": "debug"}, with(func(c *Config) { c.LogLevel = slog.LevelDebug })},
		{"level upper case", map[string]string{"LOG_LEVEL": "WARN"}, with(func(c *Config) { c.LogLevel = slog.LevelWarn })},
		{"redis override", map[string]string{"REDIS_ADDR": "redis:6379"}, with(func(c *Config) { c.RedisAddr = "redis:6379" })},
		{"fake rail allowed", map[string]string{"GATEWAY_ALLOW_FAKE_RAIL": "1"}, with(func(c *Config) { c.AllowFakeRail = true })},
		{"env recorded", map[string]string{"STRATA_ENV": Production}, with(func(c *Config) { c.Env = Production })},
		// Only the exact value is production, so the fake is allowed here.
		{"env not exactly production", map[string]string{"GATEWAY_ALLOW_FAKE_RAIL": "1", "STRATA_ENV": "Production"},
			with(func(c *Config) { c.AllowFakeRail, c.Env = true, "Production" })},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Load(env(tt.env))
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("Load() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestLoad_InvalidLogLevel(t *testing.T) {
	_, err := Load(env(map[string]string{"LOG_LEVEL": "loud"}))
	if !errors.Is(err, ErrInvalidLogLevel) {
		t.Fatalf("Load() error = %v, want ErrInvalidLogLevel", err)
	}
}

func TestLoad_DatabaseURLRequired(t *testing.T) {
	_, err := Load(env(map[string]string{"DATABASE_URL": ""}))
	if !errors.Is(err, ErrMissingDatabaseURL) {
		t.Fatalf("Load() error = %v, want ErrMissingDatabaseURL", err)
	}
}

func TestLoad_FakeRailFlag(t *testing.T) {
	for _, v := range []string{"0", "true", "yes", " 1"} {
		if _, err := Load(env(map[string]string{"GATEWAY_ALLOW_FAKE_RAIL": v})); !errors.Is(err, ErrInvalidFakeRailFlag) {
			t.Errorf("GATEWAY_ALLOW_FAKE_RAIL=%q: Load() error = %v, want ErrInvalidFakeRailFlag", v, err)
		}
	}
}

func TestLoad_FakeRailRefusedInProduction(t *testing.T) {
	_, err := Load(env(map[string]string{"GATEWAY_ALLOW_FAKE_RAIL": "1", "STRATA_ENV": Production}))
	if !errors.Is(err, ErrFakeRailInProduction) {
		t.Fatalf("Load() error = %v, want ErrFakeRailInProduction", err)
	}
	// Production without the flag is fine: the internal rail.
	if cfg, err := Load(env(map[string]string{"STRATA_ENV": Production})); err != nil || cfg.AllowFakeRail {
		t.Fatalf("Load() = %+v, %v; want the internal rail", cfg, err)
	}
}
