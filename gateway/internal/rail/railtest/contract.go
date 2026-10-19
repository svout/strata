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

// Harness describes one rail under test. New, Request and Effects are
// required; each optional hook enables the tests for a behaviour the rail
// can produce, and those tests skip when it is nil.
type Harness struct {
	// New returns a fresh adapter for one test.
	New func(t *testing.T) rail.Adapter
	// Request returns a valid request the adapter has never seen, with a new
	// reference.
	Request func(t *testing.T) rail.Request
	// Effects returns how many times money moved for ref (0 or 1 for a
	// correct rail): postings for the internal rail, applications for the
	// fake. It must count applications, not read a flag, so a double
	// application shows as 2.
	Effects func(t *testing.T, ref string) int

	// Decline returns a request a will decline. Nil for a rail that never
	// declines (the internal rail).
	Decline func(t *testing.T, a rail.Adapter) rail.Request
	// Pend returns a request a will leave Pending. Nil for a synchronous rail.
	Pend func(t *testing.T, a rail.Adapter) rail.Request
	// Resolve finishes a pending operation on a with outcome (Settled or
	// Declined), as the rail's asynchronous notification would. Required
	// when Pend is set.
	Resolve func(t *testing.T, a rail.Adapter, ref string, outcome rail.Outcome)
	// LoseResponse returns a request whose Settle a will apply but answer
	// with an error, as a connection lost after the rail acted would. Nil if
	// the harness can't arrange that.
	LoseResponse func(t *testing.T, a rail.Adapter) rail.Request
	// Unreachable returns an adapter for the same rail as a whose calls fail
	// before reaching the rail (a refused connection, a closed pool), and a
	// valid request for it. Retried on a, the request must settle. Nil if
	// the harness can't arrange that.
	Unreachable func(t *testing.T, a rail.Adapter) (rail.Adapter, rail.Request)
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

	// The central claim (ADR-120): however many calls race on one reference,
	// money moves once, and the reference ends Settled.
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
		settled := 0
		for i := range concurrent {
			// An error is allowed (an unknown outcome); a claimed result must
			// be the one outcome, and at least one call must claim it.
			switch {
			case errs[i] != nil:
			case results[i].Outcome == rail.Settled:
				settled++
			default:
				t.Errorf("Settle %d = %+v, want Settled", i, results[i])
			}
		}
		if settled == 0 {
			t.Fatalf("no concurrent Settle returned Settled (errors: %v)", errs)
		}
		if res, err := a.Status(t.Context(), req.Ref); err != nil || res.Outcome != rail.Settled {
			t.Fatalf("Status after the race = %+v, %v; want Settled", res, err)
		}
		if res, err := a.Settle(t.Context(), req); err != nil || res.Outcome != rail.Settled {
			t.Fatalf("Settle after the race = %+v, %v; want Settled", res, err)
		}
		if n := h.Effects(t, req.Ref); n != 1 {
			t.Fatalf("effects = %d after %d concurrent settles and a repeat, want 1", n, concurrent)
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
		if res, err := a.Settle(ctx, req); err == nil || errors.Is(err, rail.ErrInvalidRequest) {
			t.Fatalf("Settle on a cancelled context = %+v, %v; want an unknown-outcome error, not ErrInvalidRequest", res, err)
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
			"zero amount":   func(r *rail.Request) { r.Amount = 0 },
			"empty ref":     func(r *rail.Request) { r.Ref = "" },
			"no currency":   func(r *rail.Request) { r.Currency = "" },
			"same accounts": func(r *rail.Request) { r.Payee = r.Payer },
		} {
			req := h.Request(t)
			change(&req)
			// Deterministic: the repeat fails the same way, and nothing moves.
			for range 2 {
				if _, err := a.Settle(t.Context(), req); !errors.Is(err, rail.ErrInvalidRequest) {
					t.Errorf("%s: Settle = %v, want ErrInvalidRequest", name, err)
				}
			}
			if req.Ref != "" {
				if n := h.Effects(t, req.Ref); n != 0 {
					t.Errorf("%s: effects = %d, want 0", name, n)
				}
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

	// The asynchronous path (Stripe): pending moves nothing and repeats
	// stay pending until the rail resolves it; then the outcome is final.
	t.Run("pending stays pending until resolved", func(t *testing.T) {
		if h.Pend == nil {
			t.Skip("this rail is synchronous")
		}
		for _, final := range []rail.Outcome{rail.Settled, rail.Declined} {
			a := h.New(t)
			req := h.Pend(t, a)
			for i := range 2 { // the repeat stays pending and moves nothing
				if res, err := a.Settle(t.Context(), req); err != nil || res.Outcome != rail.Pending {
					t.Fatalf("Settle %d = %+v, %v; want Pending", i, res, err)
				}
			}
			if res, err := a.Status(t.Context(), req.Ref); err != nil || res.Outcome != rail.Pending {
				t.Fatalf("Status before resolving = %+v, %v; want Pending", res, err)
			}
			if n := h.Effects(t, req.Ref); n != 0 {
				t.Fatalf("effects while pending = %d, want 0", n)
			}
			h.Resolve(t, a, req.Ref, final)
			want := 0
			if final == rail.Settled {
				want = 1
			}
			for _, get := range []func() (rail.Result, error){
				func() (rail.Result, error) { return a.Status(t.Context(), req.Ref) },
				func() (rail.Result, error) { return a.Settle(t.Context(), req) },
			} {
				if res, err := get(); err != nil || res.Outcome != final || (final == rail.Declined && res.Reason == "") {
					t.Fatalf("after resolving %s: %+v, %v", final, res, err)
				}
			}
			if n := h.Effects(t, req.Ref); n != want {
				t.Fatalf("effects after resolving %s = %d, want %d", final, n, want)
			}
		}
	})

	// ErrInvalidRequest is final: payments fails the payment and never
	// retries it (subtask 10a-i). So an adapter may return it only for a
	// definitive rejection, never when the call didn't reach the rail or the
	// answer was ambiguous; that would turn a network blip into a permanently
	// failed payment, the class of bug ADR-120 exists to prevent. The
	// cancelled-context and lost-response cases above and below check the
	// same rule.
	t.Run("unreachable rail is an unknown outcome, not ErrInvalidRequest", func(t *testing.T) {
		if h.Unreachable == nil {
			t.Skip("the harness can't make this rail unreachable")
		}
		a := h.New(t)
		down, req := h.Unreachable(t, a)
		if res, err := down.Settle(t.Context(), req); err == nil || errors.Is(err, rail.ErrInvalidRequest) {
			t.Fatalf("Settle on an unreachable rail = %+v, %v; want an unknown-outcome error, not ErrInvalidRequest", res, err)
		}
		if _, err := down.Status(t.Context(), req.Ref); err == nil || errors.Is(err, rail.ErrUnknownRef) {
			t.Fatalf("Status on an unreachable rail = %v; want an error that isn't ErrUnknownRef (the rail wasn't asked)", err)
		}
		if n := h.Effects(t, req.Ref); n != 0 {
			t.Fatalf("effects = %d, want 0", n)
		}
		if res, err := a.Settle(t.Context(), req); err != nil || res.Outcome != rail.Settled {
			t.Fatalf("retry on the reachable rail = %+v, %v; want Settled", res, err)
		}
		if n := h.Effects(t, req.Ref); n != 1 {
			t.Fatalf("effects after the retry = %d, want 1", n)
		}
	})

	// Unknown is not failure (ADR-120): the rail acted but the answer was
	// lost. Settle errors, Status says Settled, and a retry moves nothing.
	t.Run("lost response is resolved by status", func(t *testing.T) {
		if h.LoseResponse == nil {
			t.Skip("the harness can't lose a response for this rail")
		}
		a := h.New(t)
		req := h.LoseResponse(t, a)
		if _, err := a.Settle(t.Context(), req); err == nil || errors.Is(err, rail.ErrInvalidRequest) {
			t.Fatalf("Settle = %v, want an unknown-outcome error", err)
		}
		if res, err := a.Status(t.Context(), req.Ref); err != nil || res.Outcome != rail.Settled {
			t.Fatalf("Status = %+v, %v; want Settled", res, err)
		}
		if res, err := a.Settle(t.Context(), req); err != nil || res.Outcome != rail.Settled {
			t.Fatalf("retry = %+v, %v; want Settled", res, err)
		}
		if n := h.Effects(t, req.Ref); n != 1 {
			t.Fatalf("effects = %d, want 1", n)
		}
	})
}
