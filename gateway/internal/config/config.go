// Package config loads gateway settings from the environment.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// ErrInvalidLogLevel is returned when LOG_LEVEL is not one of debug, info, warn or error.
var ErrInvalidLogLevel = errors.New("invalid log level")

// Config holds the gateway's runtime settings.
type Config struct {
	Addr     string     // listen address, from GATEWAY_ADDR
	LogLevel slog.Level // from LOG_LEVEL
}

// Load reads the configuration through getenv (os.Getenv in production),
// applying defaults for anything unset.
func Load(getenv func(string) string) (Config, error) {
	cfg := Config{Addr: ":8080", LogLevel: slog.LevelInfo}

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

	return cfg, nil
}
