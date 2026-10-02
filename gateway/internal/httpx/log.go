// Package httpx holds the gateway's HTTP plumbing: router, middleware and logging.
package httpx

import (
	"io"
	"log/slog"
	"strings"
)

// The repo rule is "log IDs and digests, never secrets, tokens, mandate bodies
// or card data" (CLAUDE.md, SPEC-002 AC8). A key is redacted if it contains one
// of sensitiveSubstrings, or if one of its words (split on _ - . and spaces)
// is in sensitiveWords. Short words are matched whole so "span_id" or
// "company" are not caught by "pan". Free-text messages are not inspected:
// never put secrets in a message.
var (
	sensitiveSubstrings = []string{"secret", "token", "password", "passwd", "authorization", "apikey", "api_key", "private_key", "signature"}
	sensitiveWords      = map[string]bool{"card": true, "pan": true, "cvc": true, "cvv": true, "pin": true, "auth": true}
)

const redacted = "[REDACTED]"

// NewLogger returns a JSON logger at lvl that redacts sensitive attributes.
func NewLogger(w io.Writer, lvl slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:       lvl,
		ReplaceAttr: redact,
	}))
}

func redact(_ []string, a slog.Attr) slog.Attr {
	if isSensitive(a.Key) {
		return slog.String(a.Key, redacted)
	}
	return a
}

func isSensitive(key string) bool {
	k := strings.ToLower(key)
	for _, s := range sensitiveSubstrings {
		if strings.Contains(k, s) {
			return true
		}
	}
	words := strings.FieldsFunc(k, func(r rune) bool { return r == '_' || r == '-' || r == '.' || r == ' ' })
	for _, w := range words {
		if sensitiveWords[w] {
			return true
		}
	}
	return false
}
