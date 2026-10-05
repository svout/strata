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
func claim(ctx context.Context, tx pgx.Tx, key Key, hash [32]byte) (Response, bool, error) {
	var (
		stored  []byte
		status  string
		code    *int
		body    []byte
		expired bool
	)
	err := tx.QueryRow(ctx, `
		SELECT request_hash, status, response_code, response_body,
		       created_at < now() - make_interval(secs => $3)
		FROM idempotency_keys
		WHERE scope = $1 AND key = $2`,
		key.Scope, key.Value, Retention.Seconds()).Scan(&stored, &status, &code, &body, &expired)

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		_, err = tx.Exec(ctx, `
			INSERT INTO idempotency_keys (scope, key, request_hash, status)
			VALUES ($1, $2, $3, 'in_progress')`,
			key.Scope, key.Value, hash[:])
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
			SET request_hash = $3, status = 'in_progress', response_code = NULL,
			    response_body = NULL, created_at = now()
			WHERE scope = $1 AND key = $2`,
			key.Scope, key.Value, hash[:])
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
	tag, err := tx.Exec(ctx, `
		UPDATE idempotency_keys
		SET status = 'completed', response_code = $4, response_body = $5
		WHERE scope = $1 AND key = $2 AND request_hash = $3 AND status = 'in_progress'`,
		key.Scope, key.Value, hash[:], resp.Status, resp.Body)
	if err != nil {
		return fmt.Errorf("idem: complete key: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("idem: complete: key was not claimed for this request in this transaction")
	}
	return nil
}
