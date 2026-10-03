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
)

// Config holds the gateway's runtime settings. It contains the database DSN,
// so it is never logged.
type Config struct {
	Addr        string     // listen address, from GATEWAY_ADDR
	LogLevel    slog.Level // from LOG_LEVEL
	DatabaseURL string     // PostgreSQL DSN, from DATABASE_URL (required)
	RedisAddr   string     // host:port, from REDIS_ADDR
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

	return cfg, nil
}
