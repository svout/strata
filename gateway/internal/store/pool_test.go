package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// These tests need no database: OpenPool never connects.

func TestOpenPool_MalformedDSNHidesPassword(t *testing.T) {
	const password = "hunter2"
	pool, err := OpenPool(context.Background(), "postgres://agentgate:"+password+"@[bad")
	if pool != nil {
		pool.Close()
		t.Fatal("got a pool for a malformed DSN")
	}
	if !errors.Is(err, ErrInvalidDSN) {
		t.Fatalf("err = %v, want ErrInvalidDSN", err)
	}
	if strings.Contains(err.Error(), password) {
		t.Fatalf("error text leaks the password: %q", err.Error())
	}
}

func TestOpenPool_UnreachableHostIsLazy(t *testing.T) {
	// 192.0.2.0/24 is TEST-NET-1 (RFC 5737): never routable. A pool is still
	// returned because nothing is dialled until first use.
	pool, err := OpenPool(context.Background(), "postgres://agentgate:hunter2@192.0.2.1:5432/ledger?connect_timeout=1")
	if err != nil {
		t.Fatalf("OpenPool: %v", err)
	}
	if pool == nil {
		t.Fatal("pool is nil")
	}
	pool.Close()
}
