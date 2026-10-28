package idem

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// All of this package's SQL (ADR-117). It touches idempotency_keys only.

// claim reads the key first, then inserts or overwrites it. Reading first
// matters under SERIALIZABLE: when two transactions race to insert the same
// new key, the one that read "no row" and then hits the other's committed
// insert gets a serialization failure (40001), which store.Serializable
// retries, rather than a plain unique violation it would not.
//
// Every statement finds the row by key_hash, the primary key (ADR-126), and
// never by (scope, key): a lookup by (scope, key) has no index to use since
// migration 00007, so it would scan the table and take a predicate lock on
// all of it, which brings back the conflicts the hash removed.
//
// The extra "scope = ... AND key = ..." on each lookup is load-bearing, not
// redundant. If two keys ever had the same SHA-256, the filter makes the
// second key's read miss, so its insert hits the first key's primary key.
// That insert fails: with 23505, which store.Serializable does not retry, or,
// under concurrent load, possibly with a serialization failure (40001) first.
// store.Serializable retries a 40001, but every retry's read misses the same
// way, so the request still ends in 23505 or, after the last attempt,
// ErrRetryLimit. Every path is a failed request; none replays. Without the
// filter the read would find the other key's row and replay that caller's
// stored response. A forced SHA-256 collision is not a practical attack, but
// this keeps it a failure rather than a disclosure (ADR-126, Q11-ii design).
func claim(ctx context.Context, tx pgx.Tx, key Key, hash [32]byte) (Response, bool, error) {
	var (
		stored  []byte
		status  string
		code    *int
		body    []byte
		expired bool
	)
	kh := keyHash(key)
	err := tx.QueryRow(ctx, `
		SELECT request_hash, status, response_code, response_body,
		       created_at < now() - make_interval(secs => $4)
		FROM idempotency_keys
		WHERE key_hash = $1 AND scope = $2 AND key = $3`,
		kh[:], key.Scope, key.Value, Retention.Seconds()).Scan(&stored, &status, &code, &body, &expired)

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		_, err = tx.Exec(ctx, `
			INSERT INTO idempotency_keys (key_hash, scope, key, request_hash, status)
			VALUES ($1, $2, $3, $4, 'in_progress')`,
			kh[:], key.Scope, key.Value, hash[:])
		if err != nil {
			return Response{}, false, fmt.Errorf("idem: insert key: %w", err)
		}
		return Response{}, false, nil
	case err != nil:
		return Response{}, false, fmt.Errorf("idem: read key: %w", err)
	case expired:
		// Past retention the key is free again: overwrite it as a new claim.
		_, err = tx.Exec(ctx, `
			UPDATE idempotency_keys
			SET request_hash = $4, status = 'in_progress', response_code = NULL,
			    response_body = NULL, created_at = now()
			WHERE key_hash = $1 AND scope = $2 AND key = $3`,
			kh[:], key.Scope, key.Value, hash[:])
		if err != nil {
			return Response{}, false, fmt.Errorf("idem: reclaim expired key: %w", err)
		}
		return Response{}, false, nil
	case !bytes.Equal(stored, hash[:]):
		return Response{}, false, ErrMismatch
	case status != "completed" || code == nil:
		return Response{}, false, errClaimedNotCompleted
	}
	return Response{Status: *code, Body: body}, true, nil
}

func complete(ctx context.Context, tx pgx.Tx, key Key, hash [32]byte, resp Response) error {
	kh := keyHash(key) // by key_hash, with the same load-bearing filter as claim
	tag, err := tx.Exec(ctx, `
		UPDATE idempotency_keys
		SET status = 'completed', response_code = $5, response_body = $6
		WHERE key_hash = $1 AND scope = $2 AND key = $3 AND request_hash = $4 AND status = 'in_progress'`,
		kh[:], key.Scope, key.Value, hash[:], resp.Status, resp.Body)
	if err != nil {
		return fmt.Errorf("idem: complete key: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("idem: complete: key was not claimed for this request in this transaction")
	}
	return nil
}
