// Package config loads gateway settings from the environment.
package config

import (
	"errors"
	"fmt"
	"log/slog"
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
}

// Load reads the configuration through getenv (os.Getenv in production),
// applying defaults for anything unset.
func Load(getenv func(string) string) (Config, error) {
	cfg := Config{Addr: ":8080", LogLevel: slog.LevelInfo, RedisAddr: "localhost:6379"}

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

	return cfg, nil
}
