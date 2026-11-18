package config

import (
	"errors"
	"log/slog"
	"net/netip"
	"reflect"
	"strconv"
	"strings"
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
	base := Config{Addr: ":8080", LogLevel: slog.LevelInfo, DatabaseURL: testDSN, RedisAddr: "localhost:6379",
		WebhookRate: 20, WebhookBurst: 40}
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
		{"webhook bound", map[string]string{"GATEWAY_WEBHOOK_BOUND": "64"}, with(func(c *Config) { c.WebhookBound = 64 })},
		{"webhook rate and burst", map[string]string{"GATEWAY_WEBHOOK_RATE": "2.5", "GATEWAY_WEBHOOK_BURST": "150"},
			with(func(c *Config) { c.WebhookRate, c.WebhookBurst = 2.5, 150 })},
		{"webhook rate at the ceiling", map[string]string{"GATEWAY_WEBHOOK_RATE": "1000"}, with(func(c *Config) { c.WebhookRate = 1000 })},
		// burst/rate = 60 s exactly is allowed.
		{"overflow window at the cap", map[string]string{"GATEWAY_WEBHOOK_RATE": "1", "GATEWAY_WEBHOOK_BURST": "60"},
			with(func(c *Config) { c.WebhookRate, c.WebhookBurst = 1, 60 })},
		{"trusted proxies", map[string]string{"GATEWAY_TRUSTED_PROXIES": "10.0.0.0/8, 192.0.2.7/32,2001:db8::/48"},
			with(func(c *Config) {
				c.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"),
					netip.MustParsePrefix("192.0.2.7/32"), netip.MustParsePrefix("2001:db8::/48")}
			})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Load(env(tt.env))
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
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

// Bad webhook settings fail Load with the fixed-text sentinel itself, never
// a wrapped error quoting the value (it could be anything an operator
// pasted).
func TestLoad_InvalidWebhookSettings(t *testing.T) {
	tests := []struct {
		env  map[string]string
		want error
	}{
		{map[string]string{"GATEWAY_WEBHOOK_BOUND": "0"}, ErrInvalidWebhookBound},
		{map[string]string{"GATEWAY_WEBHOOK_BOUND": "-1"}, ErrInvalidWebhookBound},
		{map[string]string{"GATEWAY_WEBHOOK_BOUND": "65"}, ErrInvalidWebhookBound},
		{map[string]string{"GATEWAY_WEBHOOK_BOUND": "8.5"}, ErrInvalidWebhookBound},
		{map[string]string{"GATEWAY_WEBHOOK_BOUND": "eight"}, ErrInvalidWebhookBound},
		{map[string]string{"GATEWAY_WEBHOOK_RATE": "NaN"}, ErrInvalidWebhookRate},
		{map[string]string{"GATEWAY_WEBHOOK_RATE": "Inf"}, ErrInvalidWebhookRate},
		{map[string]string{"GATEWAY_WEBHOOK_RATE": "-Inf"}, ErrInvalidWebhookRate},
		{map[string]string{"GATEWAY_WEBHOOK_RATE": "1e400"}, ErrInvalidWebhookRate},
		{map[string]string{"GATEWAY_WEBHOOK_RATE": "0"}, ErrInvalidWebhookRate},
		{map[string]string{"GATEWAY_WEBHOOK_RATE": "-1"}, ErrInvalidWebhookRate},
		{map[string]string{"GATEWAY_WEBHOOK_RATE": "0.99"}, ErrInvalidWebhookRate},
		{map[string]string{"GATEWAY_WEBHOOK_RATE": "fast"}, ErrInvalidWebhookRate},
		// The ceiling: a floor-only check is half a validation.
		{map[string]string{"GATEWAY_WEBHOOK_RATE": "1000.5"}, ErrInvalidWebhookRate},
		{map[string]string{"GATEWAY_WEBHOOK_RATE": "1e300"}, ErrInvalidWebhookRate},
		{map[string]string{"GATEWAY_WEBHOOK_BURST": "0"}, ErrInvalidWebhookBurst},
		{map[string]string{"GATEWAY_WEBHOOK_BURST": "1001"}, ErrInvalidWebhookBurst},
		{map[string]string{"GATEWAY_WEBHOOK_BURST": "4e1"}, ErrInvalidWebhookBurst},
		// Each value valid alone, too much together: 1000/1 s and 61/1 s.
		{map[string]string{"GATEWAY_WEBHOOK_RATE": "1", "GATEWAY_WEBHOOK_BURST": "1000"}, ErrWebhookOverflowWindow},
		{map[string]string{"GATEWAY_WEBHOOK_RATE": "1", "GATEWAY_WEBHOOK_BURST": "61"}, ErrWebhookOverflowWindow},
		// The default burst of 40 with a rate under 40/60 would trip it, but
		// the rate floor of 1 already refuses that.
	}
	for _, tt := range tests {
		_, err := Load(env(tt.env))
		// The exact sentinel, unwrapped: nothing from the input is added.
		if err != tt.want { //nolint:errorlint // identity is the point
			t.Errorf("%v: Load() error = %v, want exactly %v", tt.env, err, tt.want)
		}
	}
}

// The window error says what the two values must satisfy, not just that
// one of them is wrong.
func TestErrWebhookOverflowWindow_NamesTheRelationship(t *testing.T) {
	msg := ErrWebhookOverflowWindow.Error()
	for _, want := range []string{"GATEWAY_WEBHOOK_BURST divided by GATEWAY_WEBHOOK_RATE", "overflow window", "at most 60"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error text %q doesn't say %q", msg, want)
		}
	}
}

// The cap is inclusive: exactly maxTrustedProxies entries load (the
// "too many" case below is one over).
func TestLoad_TrustedProxiesAtCap(t *testing.T) {
	cfg, err := Load(env(map[string]string{"GATEWAY_TRUSTED_PROXIES": strings.TrimSuffix(strings.Repeat("10.0.0.0/8,", maxTrustedProxies), ",")}))
	if err != nil {
		t.Fatalf("Load() with %d entries: %v, want it accepted", maxTrustedProxies, err)
	}
	if len(cfg.TrustedProxies) != maxTrustedProxies {
		t.Fatalf("got %d prefixes, want %d", len(cfg.TrustedProxies), maxTrustedProxies)
	}
}

// A bad trusted-proxy list fails Load with the sentinel and, at most, the
// entry's position: never the value, which run logs verbatim.
func TestLoad_InvalidTrustedProxies(t *testing.T) {
	tests := []struct {
		name, v string
		entry   int // 0: no position (the list as a whole)
	}{
		{"bare IPv4", "10.0.0.1", 1},
		{"bare IPv6", "2001:db8::1", 1},
		{"host bits set", "10.0.0.1/8", 1},
		{"IPv4 /0", "0.0.0.0/0", 1},
		{"IPv6 /0", "::/0", 1},
		{"zone", "fe80::%eth0/64", 1},
		{"IPv4-mapped", "::ffff:10.0.0.0/104", 1},
		{"trailing comma", "10.0.0.0/8,", 2},
		{"empty middle entry", "10.0.0.0/8,,192.0.2.0/24", 2},
		{"only whitespace", " ", 1},
		{"third entry bad", "10.0.0.0/8,192.0.2.0/24,leaksecret-192.0.2.9", 3},
		{"bits out of range", "10.0.0.0/33", 1},
		{"too many", strings.Repeat("10.0.0.0/8,", 32) + "10.0.0.0/8", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(env(map[string]string{"GATEWAY_TRUSTED_PROXIES": tt.v}))
			if !errors.Is(err, ErrInvalidTrustedProxies) {
				t.Fatalf("Load() error = %v, want ErrInvalidTrustedProxies", err)
			}
			want := ErrInvalidTrustedProxies.Error()
			if tt.entry > 0 {
				want += " (entry " + strconv.Itoa(tt.entry) + ")"
			}
			if err.Error() != want {
				t.Errorf("error = %q, want %q (fixed text and the position only)", err, want)
			}
		})
	}
}
