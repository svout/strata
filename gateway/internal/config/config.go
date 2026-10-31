// Package config loads gateway settings from the environment.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
)

var (
	// ErrInvalidLogLevel is returned when LOG_LEVEL is not one of debug, info, warn or error.
	ErrInvalidLogLevel = errors.New("invalid log level")
	// ErrMissingDatabaseURL is returned when DATABASE_URL is unset: there is
	// deliberately no default DSN in code.
	ErrMissingDatabaseURL = errors.New("DATABASE_URL is required")
	// ErrInvalidFakeRailFlag is returned when GATEWAY_ALLOW_FAKE_RAIL is set
	// to anything but "1". A typo fails startup rather than quietly running
	// the internal rail when the fake was meant (or the other way round).
	ErrInvalidFakeRailFlag = errors.New(`GATEWAY_ALLOW_FAKE_RAIL must be "1" or unset`)
	// ErrFakeRailInProduction is returned when the fake rail is allowed under
	// STRATA_ENV=production: the fake moves no real money, so a production
	// gateway on it would report payments settled that never were
	// (SPEC-002 10a-ii, decision 5).
	ErrFakeRailInProduction = errors.New("GATEWAY_ALLOW_FAKE_RAIL=1 is refused under STRATA_ENV=production")

	// The webhook settings' errors have fixed text and never quote the
	// value (SPEC-002 10b-ii-b). Whether the bound fits the database pool
	// is checked later, in cmd/gateway, which is the only place that knows
	// the pool's size (ADR-127).

	// ErrInvalidWebhookBound is returned when GATEWAY_WEBHOOK_BOUND is not a
	// whole number from 1 to 64.
	ErrInvalidWebhookBound = errors.New("GATEWAY_WEBHOOK_BOUND must be a whole number from 1 to 64")
	// ErrInvalidWebhookRate is returned when GATEWAY_WEBHOOK_RATE is not a
	// number from 1 to 1000 (requests per second per source). The text
	// says why there is a ceiling, since it is the less obvious end.
	ErrInvalidWebhookRate = errors.New("GATEWAY_WEBHOOK_RATE must be from 1 to 1000 requests per second per source: " +
		"the webhook is unauthenticated and its rate limit is what bounds how many log lines one caller can cause")
	// ErrInvalidWebhookBurst is returned when GATEWAY_WEBHOOK_BURST is not a
	// whole number from 1 to 1000.
	ErrInvalidWebhookBurst = errors.New("GATEWAY_WEBHOOK_BURST must be a whole number from 1 to 1000")
	// ErrWebhookOverflowWindow is returned when burst / rate is over 60 s.
	// The two are set separately but constrained together, so the text
	// names the relationship: an operator who only lowers one value until
	// the error goes away should know what they are satisfying.
	ErrWebhookOverflowWindow = errors.New("GATEWAY_WEBHOOK_BURST divided by GATEWAY_WEBHOOK_RATE is the rate limiter's overflow window in seconds " +
		"(how long a new source can wait for room once the source table is full) and must be at most 60: lower the burst or raise the rate")
)

// Webhook limiter defaults and limits (Ivan, SPEC-002 10b-ii).
const (
	// DefaultWebhookRate and DefaultWebhookBurst are per source: 20
	// requests per second after a burst of 40.
	DefaultWebhookRate  = 20
	DefaultWebhookBurst = 40
	// minWebhookRate keeps Retry-After at most a second per token. (What
	// keeps burst/rate, the limiter's sweep interval, from overflowing a
	// time.Duration is maxOverflowWindow.)
	minWebhookRate = 1
	// maxWebhookRate is the ceiling (Ivan, 2026-10-31). The limiter is what
	// bounds the webhook's log volume per source (a failing webhook logs
	// one Error line per request; see cmd/gateway mountAPIs), so a rate
	// config could raise without limit would void that. assumed: 1000/s
	// (not measured), well above P6's 200 rps.
	maxWebhookRate  = 1000
	maxWebhookBurst = 1000
	maxWebhookBound = 64
	// maxOverflowWindow caps burst/rate, the limiter's sweep interval: once
	// its source table is full, a legitimate new source can share the
	// overflow bucket for that long (ADR-124 second addendum). Burst 1000
	// at 1/s would make it about 17 minutes.
	maxOverflowWindow = 60
)

// Production is the STRATA_ENV value that marks a production deployment.
// The match is exact: "prod" or "Production" are not production.
const Production = "production"

// Config holds the gateway's runtime settings. It contains the database DSN,
// so it is never logged.
type Config struct {
	Addr        string     // listen address, from GATEWAY_ADDR
	LogLevel    slog.Level // from LOG_LEVEL
	DatabaseURL string     // PostgreSQL DSN, from DATABASE_URL (required)
	RedisAddr   string     // host:port, from REDIS_ADDR
	// AllowFakeRail registers the fake PSP instead of the internal rail,
	// from GATEWAY_ALLOW_FAKE_RAIL=1. Never true when Env is Production.
	AllowFakeRail bool
	Env           string // deployment environment, from STRATA_ENV (may be empty)
	// WebhookBound is GATEWAY_WEBHOOK_BOUND, the most rail webhooks handled
	// at once. 0 means unset: cmd/gateway then derives it from the pool.
	WebhookBound int
	// WebhookRate (per second) and WebhookBurst are the rail webhook's
	// per-source rate limit, from GATEWAY_WEBHOOK_RATE and _BURST.
	WebhookRate  float64
	WebhookBurst int
}

// Load reads the configuration through getenv (os.Getenv in production),
// applying defaults for anything unset.
func Load(getenv func(string) string) (Config, error) {
	cfg := Config{Addr: ":8080", LogLevel: slog.LevelInfo, RedisAddr: "localhost:6379",
		WebhookRate: DefaultWebhookRate, WebhookBurst: DefaultWebhookBurst}

	if v := getenv("GATEWAY_ADDR"); v != "" {
		cfg.Addr = v
	}

	if v := getenv("LOG_LEVEL"); v != "" {
		var lvl slog.Level
		if err := lvl.UnmarshalText([]byte(strings.ToUpper(v))); err != nil {
			return Config{}, fmt.Errorf("%w: %q", ErrInvalidLogLevel, v)
		}
		cfg.LogLevel = lvl
	}

	cfg.DatabaseURL = getenv("DATABASE_URL")
	if cfg.DatabaseURL == "" {
		return Config{}, ErrMissingDatabaseURL
	}
	if v := getenv("REDIS_ADDR"); v != "" {
		cfg.RedisAddr = v
	}

	switch getenv("GATEWAY_ALLOW_FAKE_RAIL") {
	case "":
	case "1":
		cfg.AllowFakeRail = true
	default:
		return Config{}, ErrInvalidFakeRailFlag
	}
	cfg.Env = getenv("STRATA_ENV")
	if cfg.AllowFakeRail && cfg.Env == Production {
		return Config{}, ErrFakeRailInProduction
	}

	if err := loadWebhook(getenv, &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// loadWebhook reads the rail webhook's bound and rate limit into cfg.
func loadWebhook(getenv func(string) string, cfg *Config) error {
	if v := getenv("GATEWAY_WEBHOOK_BOUND"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxWebhookBound {
			return ErrInvalidWebhookBound
		}
		cfg.WebhookBound = n
	}
	if v := getenv("GATEWAY_WEBHOOK_RATE"); v != "" {
		// ParseFloat accepts "NaN" and "Inf", and turns "1e400" into +Inf
		// with an error; !(r >= min) rejects NaN too.
		r, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsInf(r, 0) || !(r >= minWebhookRate) || r > maxWebhookRate {
			return ErrInvalidWebhookRate
		}
		cfg.WebhookRate = r
	}
	if v := getenv("GATEWAY_WEBHOOK_BURST"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxWebhookBurst {
			return ErrInvalidWebhookBurst
		}
		cfg.WebhookBurst = n
	}
	if float64(cfg.WebhookBurst)/cfg.WebhookRate > maxOverflowWindow {
		return ErrWebhookOverflowWindow
	}
	return nil
}
