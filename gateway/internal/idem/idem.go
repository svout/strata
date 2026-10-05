// Package idem owns the idempotency_keys table: it claims an
// Idempotency-Key for a request and stores the response, so a retry of the
// same request gets the same answer instead of a second effect.
//
// The record commits in the same transaction as the effect it protects
// (ADR-118): the caller runs one store.Serializable transaction and calls
// Claim, performs the effect, then Complete. Either the key and the effect
// both commit or neither does, so a committed key is always complete and a
// crash can never leave a key that is claimed but unanswerable. Two racing
// duplicates both try to insert the key; the loser's transaction fails with a
// serialization failure, store.Serializable runs it again, and the second run
// finds the completed key and replays it.
//
// Like ledger, this is a data-layer package (ADR-117): all its SQL is in
// store.go, it touches idempotency_keys only, and its functions take the
// caller's pgx.Tx.
package idem

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Header is the request header that carries the key.
const Header = "Idempotency-Key"

// MaxKeyLen is the longest key accepted, in bytes (the table's CHECK counts
// characters, so every key that passes here also passes there).
const MaxKeyLen = 255

// Retention is how long a completed key is honoured. A claim on an older key
// overwrites it inside the claiming transaction, so correctness never depends
// on a sweeper deleting old rows (ADR-118; the sweeper is deferred).
const Retention = 24 * time.Hour

// Caller identifies who sent a request. In P1 there is no authentication, so
// every request has the same placeholder caller; from P2 it is the agent ID
// taken from the verified mandate (ADR-118). It is both the key's scope and
// part of the request hash.
const AnonymousCaller = ""

// Key is an Idempotency-Key within its scope (the caller).
type Key struct {
	Scope string
	Value string
}

// Digest is a short, stable stand-in for the key in logs: the first 16 bytes
// of SHA-256(scope, value), hex encoded. The key itself is caller-chosen text
// and is never logged.
func (k Key) Digest() string {
	sum := sha256.Sum256([]byte(k.Scope + "\x00" + k.Value))
	return hex.EncodeToString(sum[:16])
}

// ErrMissingKey means the header is absent or longer than MaxKeyLen.
var ErrMissingKey = errors.New("idem: Idempotency-Key header is missing or too long")

// ParseKey validates the header value and scopes it to caller.
func ParseKey(caller, header string) (Key, error) {
	if header == "" || len(header) > MaxKeyLen {
		return Key{}, ErrMissingKey
	}
	return Key{Scope: caller, Value: header}, nil
}

// Request is what the hash covers. Body must be canonical: the decoded
// request re-marshalled, so whitespace and member order don't count.
type Request struct {
	Caller string
	Method string
	Path   string
	Body   []byte
}

// Hash is SHA-256 over the caller, method, path and canonical body, so a key
// reused by another caller or on another endpoint never matches (ADR-118).
// Each field is length-prefixed, so no two different requests share an
// encoding.
func Hash(r Request) [32]byte {
	h := sha256.New()
	for _, f := range [][]byte{[]byte(r.Caller), []byte(r.Method), []byte(r.Path), r.Body} {
		h.Write(binary.BigEndian.AppendUint64(nil, uint64(len(f))))
		h.Write(f)
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

// Response is a stored answer to replay.
type Response struct {
	Status int
	Body   []byte
}

// ErrMismatch means the key was already used for a different request.
var ErrMismatch = errors.New("idem: Idempotency-Key was used for a different request")

// errClaimedNotCompleted would mean a committed key with no response, which
// one-transaction use makes impossible; it is reported, never replayed.
var errClaimedNotCompleted = errors.New("idem: key is claimed but has no response")

// Claim claims key for a request with hash, inside tx.
//
// If the key is new (or expired), it is claimed and Claim returns
// (Response{}, false, nil): the caller performs the effect and calls
// Complete in the same tx. If the key holds a completed response for the same
// hash, Claim returns it with true: the caller replays it and does nothing
// else. A different hash is ErrMismatch.
func Claim(ctx context.Context, tx pgx.Tx, key Key, hash [32]byte) (Response, bool, error) {
	return claim(ctx, tx, key, hash)
}

// Complete stores the response for the key and hash claimed in the same tx.
func Complete(ctx context.Context, tx pgx.Tx, key Key, hash [32]byte, resp Response) error {
	return complete(ctx, tx, key, hash, resp)
}
