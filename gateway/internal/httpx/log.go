// Package httpx holds the gateway's HTTP plumbing: router, middleware and logging.
package httpx

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"reflect"
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
//     "token_count", "mandate_id" and "session_id" are logged while
//     "signature", "auth_token", "apiKey" and "session" are not.
//   - Before word matching, trailing digits are dropped ("cvv2" → cvv,
//     "last4" → last; an all-digit word disappears) and a word also matches a
//     term when it is that term's plural ("signatures", "apiKeys", "bodies";
//     see hasTerm). Plurals are matched against the term lists rather than
//     stemmed, so ordinary words ending in s ("status", "passes",
//     "processes") are never cut down and can't collide with a term.
//   - Card data is never loggable, not even partly: "pan_last4" and
//     "card_last4" are redacted (ADR-105), and "cardholder" is hard.
//
// Free-text messages are not inspected: never put secrets in a message.
//
// Substrings catch compounds written as one word ("bearertoken",
// "awsAccessKeyId"), so only terms that don't occur inside ordinary words go
// there; "otp" is a word because it sits inside "footprint".
var (
	hardSubstrings = []string{"secret", "passw", "passcode", "authorization", "credential", "cookie", "privatekey", "databaseurl",
		"cardholder", "bearer", "jwt", "accesskey"}
	hardWords = set("pan", "cvc", "cvv", "pin", "pinblock", "private", "dsn", "otp", "totp", "hotp")
	// "track" is card track data ("track2" → track); soft, so "track_id" is
	// still logged. One-word compounds of soft terms are listed as words.
	softWords = set("token", "signature", "sig", "auth", "mandate", "body", "payload", "card", "apikey", "signingkey", "hmackey",
		"session", "sessionkey", "track", "trackdata")
	softPairs    = set("api key", "signing key", "hmac key")
	keepSuffixes = set("valid", "alg", "count", "result", "ok", "len", "type", "id", "digest", "hash")
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
		if c, ok := sanitize(a, h.redactAll); ok {
			out.AddAttrs(c)
		}
		return true
	})
	return h.inner.Handle(ctx, out)
}

func (h *redactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &redactHandler{inner: h.inner.WithAttrs(sanitizeAll(attrs, h.redactAll)), redactAll: h.redactAll}
}

func (h *redactHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h // slog.Handler contract: WithGroup("") returns the receiver
	}
	return &redactHandler{inner: h.inner.WithGroup(name), redactAll: h.redactAll || isSensitive(name)}
}

// sanitize redacts a by key, walks groups, and never emits non-scalar Any
// values: maps, structs and slices log as "[OMITTED <type>]". A type that
// must be logged should implement slog.LogValuer, which resolves to a group and
// is then redacted key by key. Errors log their message. Named scalars
// (type PaymentID string) log their underlying value; see scalarValue. A
// pointer, even to a named scalar, is not dereferenced and logs as OMITTED.
//
// It follows the slog.Handler contract: ok is false for an empty Attr and for
// a group with no members, and a group with an empty key is inlined, so its
// members are cleaned under the parent's redactAll rather than replaced whole.
func sanitize(a slog.Attr, redactAll bool) (_ slog.Attr, ok bool) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return a, false
	}
	if a.Key == "" && a.Value.Kind() == slog.KindGroup {
		return groupAttr("", sanitizeAll(a.Value.Group(), redactAll))
	}
	if redactAll || isSensitive(a.Key) {
		return slog.String(a.Key, redacted), true
	}
	switch a.Value.Kind() {
	case slog.KindGroup:
		return groupAttr(a.Key, sanitizeAll(a.Value.Group(), false))
	case slog.KindAny:
		if err, isErr := a.Value.Any().(error); isErr {
			return slog.String(a.Key, errorText(err)), true
		}
		if v, isScalar := scalarValue(a.Value.Any()); isScalar {
			return slog.Attr{Key: a.Key, Value: v}, true
		}
		return slog.String(a.Key, fmt.Sprintf("[OMITTED %T]", a.Value.Any())), true
	default:
		return a, true
	}
}

func sanitizeAll(attrs []slog.Attr, redactAll bool) []slog.Attr {
	clean := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		if c, ok := sanitize(a, redactAll); ok {
			clean = append(clean, c)
		}
	}
	return clean
}

func groupAttr(key string, members []slog.Attr) (slog.Attr, bool) {
	if len(members) == 0 {
		return slog.Attr{}, false
	}
	return slog.Attr{Key: key, Value: slog.GroupValue(members...)}, true
}

// scalarValue converts a value of a named scalar type (type PaymentID string,
// type State string, named ints, uints, floats and bools) to its underlying
// kind. slog.AnyValue already does this for the unnamed types, so only named
// ones arrive here as KindAny. The value is read with reflect, never through
// String() or fmt: a Stringer may mask, expand or panic, and the logged value
// should be exactly what is stored. Errors are handled before this, so a named
// string that implements error still logs its message.
func scalarValue(v any) (slog.Value, bool) {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.String:
		return slog.StringValue(rv.String()), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return slog.Int64Value(rv.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64: // not Uintptr: an address, no domain meaning
		return slog.Uint64Value(rv.Uint()), true
	case reflect.Float32, reflect.Float64:
		return slog.Float64Value(rv.Float()), true
	case reflect.Bool:
		return slog.BoolValue(rv.Bool()), true
	default:
		return slog.Value{}, false
	}
}

// errorText returns err.Error() without letting a broken error take the
// logger down: a typed-nil pointer logs "<nil>" (as slog's own handlers do),
// and any other panic in Error() logs a fixed marker, never the panic value.
func errorText(err error) (s string) {
	defer func() {
		if recover() != nil {
			if v := reflect.ValueOf(err); v.Kind() == reflect.Pointer && v.IsNil() {
				s = "<nil>"
			} else {
				s = "[error text unavailable: Error() panicked]"
			}
		}
	}()
	return err.Error()
}

func isSensitive(key string) bool {
	words := normalizeWords(splitWords(key))
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
		if hasTerm(hardWords, w) {
			return true
		}
		if hasTerm(softWords, w) || (i > 0 && hasTerm(softPairs, words[i-1]+" "+w)) {
			soft = true
		}
	}
	return soft && !hasTerm(keepSuffixes, words[len(words)-1])
}

// normalizeWords drops trailing digits from each word ("cvv2" → cvv) and
// removes words that were only digits ("token_2" → [token]).
func normalizeWords(words []string) []string {
	out := words[:0]
	for _, w := range words {
		if w = strings.TrimRightFunc(w, unicode.IsDigit); w != "" {
			out = append(out, w)
		}
	}
	return out
}

// hasTerm reports whether w is in terms, either as is or as the plural of a
// term (+s, +es, y → ies). It only strips a suffix when what remains is a
// term, so "status" or "processes" can't match unless "statu" or "process"
// were terms. For a pair ("api keys") the suffix is on the last word.
func hasTerm(terms map[string]bool, w string) bool {
	if terms[w] {
		return true
	}
	if base, ok := strings.CutSuffix(w, "ies"); ok && terms[base+"y"] {
		return true
	}
	if base, ok := strings.CutSuffix(w, "es"); ok && terms[base] {
		return true
	}
	base, ok := strings.CutSuffix(w, "s")
	return ok && terms[base]
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
