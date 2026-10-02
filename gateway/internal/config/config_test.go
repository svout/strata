package config

import (
	"errors"
	"log/slog"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoad_DefaultsAndOverrides(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want Config
	}{
		{"defaults", nil, Config{Addr: ":8080", LogLevel: slog.LevelInfo}},
		{"addr override", map[string]string{"GATEWAY_ADDR": ":9090"}, Config{Addr: ":9090", LogLevel: slog.LevelInfo}},
		{"level lower case", map[string]string{"LOG_LEVEL": "debug"}, Config{Addr: ":8080", LogLevel: slog.LevelDebug}},
		{"level upper case", map[string]string{"LOG_LEVEL": "WARN"}, Config{Addr: ":8080", LogLevel: slog.LevelWarn}},
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
