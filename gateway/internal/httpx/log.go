// Package httpx holds the gateway's HTTP plumbing: router, middleware and logging.
package httpx

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"unicode"
)

// The repo rule is "log IDs and digests, never secrets, tokens, mandate bodies
// or card data" (CLAUDE.md, SPEC-002 AC8). Redaction is done by a handler
// wrapper rather than ReplaceAttr, because ReplaceAttr never sees group keys or
// WithGroup names.
//
// Key rule: the key is split into lower-case words (on non-alphanumerics and
// camelCase boundaries).
//   - Hard terms are always redacted: a joined-word substring in
//     hardSubstrings, or a word in hardWords.
//   - Soft terms (softWords, softPairs) are redacted unless the key's last word
//     is a verdict or identifier in keepSuffixes, so "signature_valid",
//     "token_count" and "mandate_id" are logged while "signature",
//     "auth_token" and "apiKey" are not.
//
// Free-text messages are not inspected: never put secrets in a message.
var (
	hardSubstrings = []string{"secret", "passw", "authorization", "credential", "cookie", "privatekey"}
	hardWords      = set("pan", "cvc", "cvv", "pin", "private")
	softWords      = set("token", "tokens", "signature", "sig", "auth", "mandate", "body", "payload", "card", "apikey", "signingkey", "hmackey")
	softPairs      = set("api key", "signing key", "hmac key")
	keepSuffixes   = set("valid", "alg", "count", "result", "ok", "len", "type", "id", "digest", "hash")
)

const redacted = "[REDACTED]"

// NewLogger returns a JSON logger at lvl that redacts sensitive attributes.
func NewLogger(w io.Writer, lvl slog.Level) *slog.Logger {
	return slog.New(&redactHandler{inner: slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl})})
}

// redactHandler sanitises every attribute before passing it on. Once a group
// with a sensitive name has been opened (WithGroup), everything under it is
// redacted.
type redactHandler struct {
	inner     slog.Handler
	redactAll bool
}

func (h *redactHandler) Enabled(ctx context.Context, lvl slog.Level) bool {
	return h.inner.Enabled(ctx, lvl)
}

func (h *redactHandler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(sanitize(a, h.redactAll))
		return true
	})
	return h.inner.Handle(ctx, out)
}

func (h *redactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clean := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		clean[i] = sanitize(a, h.redactAll)
	}
	return &redactHandler{inner: h.inner.WithAttrs(clean), redactAll: h.redactAll}
}

func (h *redactHandler) WithGroup(name string) slog.Handler {
	return &redactHandler{inner: h.inner.WithGroup(name), redactAll: h.redactAll || isSensitive(name)}
}

// sanitize redacts a by key, walks groups, and never emits non-scalar Any
// values: maps, structs and slices log as "[OMITTED <type>]". A type that
// must be logged should implement slog.LogValuer, which resolves to a group and
// is then redacted key by key. Errors log their message.
func sanitize(a slog.Attr, redactAll bool) slog.Attr {
	a.Value = a.Value.Resolve()
	if redactAll || isSensitive(a.Key) {
		return slog.String(a.Key, redacted)
	}
	switch a.Value.Kind() {
	case slog.KindGroup:
		members := a.Value.Group()
		clean := make([]slog.Attr, len(members))
		for i, m := range members {
			clean[i] = sanitize(m, false)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(clean...)}
	case slog.KindAny:
		if err, ok := a.Value.Any().(error); ok {
			return slog.String(a.Key, err.Error())
		}
		return slog.String(a.Key, fmt.Sprintf("[OMITTED %T]", a.Value.Any()))
	default:
		return a
	}
}

func isSensitive(key string) bool {
	words := splitWords(key)
	if len(words) == 0 {
		return false
	}
	joined := strings.Join(words, "")
	for _, s := range hardSubstrings {
		if strings.Contains(joined, s) {
			return true
		}
	}
	soft := false
	for i, w := range words {
		if hardWords[w] {
			return true
		}
		if softWords[w] || (i > 0 && softPairs[words[i-1]+" "+w]) {
			soft = true
		}
	}
	return soft && !keepSuffixes[words[len(words)-1]]
}

// splitWords lower-cases key and splits it on non-alphanumerics and camelCase
// boundaries: "stripeAPIKey" → [stripe api key], "client-token" → [client token].
func splitWords(key string) []string {
	var words []string
	var cur []rune
	rs := []rune(key)
	flush := func() {
		if len(cur) > 0 {
			words = append(words, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	for i, r := range rs {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush()
			continue
		}
		if unicode.IsUpper(r) && len(cur) > 0 {
			prev := rs[i-1]
			nextLower := i+1 < len(rs) && unicode.IsLower(rs[i+1])
			if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower) {
				flush()
			}
		}
		cur = append(cur, r)
	}
	flush()
	return words
}

func set(words ...string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}
