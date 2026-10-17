// Package railtest is the contract every rail.Adapter must pass (SPEC-002
// AC6, ADR-120). A rail's tests call Run with a Harness; a new rail (Stripe,
// subtask 12) reuses the same suite.
package railtest

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/svout/strata/gateway/internal/rail"
)

// Harness describes one rail under test.
type Harness struct {
	// New returns a fresh adapter for one test.
	New func(t *testing.T) rail.Adapter
	// Request returns a valid request the adapter has never seen, with a new
	// reference.
	Request func(t *testing.T) rail.Request
	// Effects returns how many times money moved for ref (0 or 1 for a
	// correct rail): postings for the internal rail, settled records for the
	// fake. It is how the suite checks "one effect", independently of what
	// Settle returns.
	Effects func(t *testing.T, ref string) int
	// Decline, if set, returns a request this adapter will decline. A rail
	// that never declines (the internal rail) leaves it nil and the decline
	// tests skip.
	Decline func(t *testing.T, a rail.Adapter) rail.Request
}

// concurrent is how many Settle calls race on one reference.
const concurrent = 16

// Run runs the contract against h.
func Run(t *testing.T, h Harness) {
	t.Run("settle moves money once", func(t *testing.T) {
		a, req := h.New(t), h.Request(t)
		res, err := a.Settle(t.Context(), req)
		if err != nil || res.Outcome != rail.Settled {
			t.Fatalf("Settle = %+v, %v; want Settled", res, err)
		}
		if n := h.Effects(t, req.Ref); n != 1 {
			t.Fatalf("effects = %d, want 1", n)
		}
	})

	t.Run("same reference again returns the same result and moves nothing", func(t *testing.T) {
		a, req := h.New(t), h.Request(t)
		first, err := a.Settle(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		again, err := a.Settle(t.Context(), req)
		if err != nil || again != first {
			t.Fatalf("second Settle = %+v, %v; want %+v", again, err, first)
		}
		if n := h.Effects(t, req.Ref); n != 1 {
			t.Fatalf("effects = %d, want 1", n)
		}
	})

	t.Run("concurrent settles with one reference move money once", func(t *testing.T) {
		a, req := h.New(t), h.Request(t)
		results := make([]rail.Result, concurrent)
		errs := make([]error, concurrent)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range concurrent {
			wg.Go(func() {
				<-start
				results[i], errs[i] = a.Settle(t.Context(), req)
			})
		}
		close(start)
		wg.Wait()
		for i := range concurrent {
			// An error is allowed (unknown outcome); a claimed result must be
			// the one outcome.
			if errs[i] == nil && results[i].Outcome != rail.Settled {
				t.Errorf("Settle %d = %+v, want Settled", i, results[i])
			}
		}
		if n := h.Effects(t, req.Ref); n != 1 {
			t.Fatalf("effects = %d after %d concurrent settles, want 1", n, concurrent)
		}
	})

	t.Run("status reports a settled reference", func(t *testing.T) {
		a, req := h.New(t), h.Request(t)
		settled, err := a.Settle(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := a.Status(t.Context(), req.Ref); err != nil || got != settled {
			t.Fatalf("Status = %+v, %v; want %+v", got, err, settled)
		}
	})

	t.Run("status of an unknown reference is ErrUnknownRef", func(t *testing.T) {
		a, req := h.New(t), h.Request(t)
		if _, err := a.Status(t.Context(), req.Ref); !errors.Is(err, rail.ErrUnknownRef) {
			t.Fatalf("Status = %v, want ErrUnknownRef", err)
		}
	})

	t.Run("cancelled context claims nothing and moves nothing", func(t *testing.T) {
		a, req := h.New(t), h.Request(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if res, err := a.Settle(ctx, req); err == nil {
			t.Fatalf("Settle on a cancelled context = %+v, want an error", res)
		}
		if n := h.Effects(t, req.Ref); n != 0 {
			t.Fatalf("effects = %d, want 0", n)
		}
		// Nothing took effect, so the same reference settles normally.
		if res, err := a.Settle(t.Context(), req); err != nil || res.Outcome != rail.Settled {
			t.Fatalf("retry = %+v, %v; want Settled", res, err)
		}
	})

	t.Run("invalid request is ErrInvalidRequest", func(t *testing.T) {
		a := h.New(t)
		for name, change := range map[string]func(*rail.Request){
			"zero amount":    func(r *rail.Request) { r.Amount = 0 },
			"empty ref":      func(r *rail.Request) { r.Ref = "" },
			"other currency": func(r *rail.Request) { r.Currency = "EUR" },
			"same accounts":  func(r *rail.Request) { r.Payee = r.Payer },
		} {
			req := h.Request(t)
			change(&req)
			if _, err := a.Settle(t.Context(), req); !errors.Is(err, rail.ErrInvalidRequest) {
				t.Errorf("%s: Settle = %v, want ErrInvalidRequest", name, err)
			}
		}
	})

	t.Run("declined moves nothing and stays declined", func(t *testing.T) {
		if h.Decline == nil {
			t.Skip("this rail never declines")
		}
		a := h.New(t)
		req := h.Decline(t, a)
		first, err := a.Settle(t.Context(), req)
		if err != nil || first.Outcome != rail.Declined || first.Reason == "" {
			t.Fatalf("Settle = %+v, %v; want Declined with a reason", first, err)
		}
		if again, err := a.Settle(t.Context(), req); err != nil || again != first {
			t.Fatalf("second Settle = %+v, %v; want %+v", again, err, first)
		}
		if got, err := a.Status(t.Context(), req.Ref); err != nil || got != first {
			t.Fatalf("Status = %+v, %v; want %+v", got, err, first)
		}
		if n := h.Effects(t, req.Ref); n != 0 {
			t.Fatalf("effects = %d, want 0", n)
		}
	})
}
