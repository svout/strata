package rail

import (
	"context"
	"errors"
	"sync"
	"time"
)

// FakeName is the fake PSP's name.
const FakeName = "fake"

// ErrResponseLost is the fake's transport failure after it has applied the
// operation: the money moved, but the caller can't know (Step.LoseResponse).
var ErrResponseLost = errors.New("rail: fake PSP response lost")

// ErrNotPending means Resolve was called for an operation that isn't pending.
var ErrNotPending = errors.New("rail: operation is not pending")

// Step scripts how the fake answers one Settle call that reaches it.
type Step struct {
	// Delay is waited before anything happens; if the context ends first,
	// Settle returns its error and nothing is applied.
	Delay time.Duration
	// Err, if set, is returned after Delay without applying anything: a
	// failure before the PSP acted.
	Err error
	// Outcome is applied and recorded for the reference (default Settled).
	// Pending stays pending until Resolve.
	Outcome Outcome
	// Reason is recorded with a Declined outcome.
	Reason string
	// LoseResponse applies the outcome, then returns ErrResponseLost: the
	// money moved but the caller only learns it through Status.
	LoseResponse bool
}

// Fake is a deterministic in-process PSP for tests and the chaos runs
// (SPEC-002 subtasks 9 and 11). Each Settle call for a reference it hasn't
// recorded takes the next Step of its script; once the script runs out it
// settles at once. A recorded reference returns its recorded outcome and
// takes no step, so the same script and the same calls always give the same
// results. It is safe for concurrent use; at most one outcome is ever
// recorded per reference.
type Fake struct {
	mu      sync.Mutex
	script  []Step
	records map[string]Result
}

// NewFake returns a fake PSP that answers with steps, in order.
func NewFake(steps ...Step) *Fake {
	return &Fake{script: steps, records: map[string]Result{}}
}

// Push appends steps to the script.
func (f *Fake) Push(steps ...Step) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.script = append(f.script, steps...)
}

// Name implements Adapter.
func (*Fake) Name() string { return FakeName }

// Settle implements Adapter.
func (f *Fake) Settle(ctx context.Context, req Request) (Result, error) {
	if err := validate(req); err != nil {
		return Result{}, err
	}
	f.mu.Lock()
	if res, ok := f.records[req.Ref]; ok {
		f.mu.Unlock()
		return res, nil
	}
	step := Step{Outcome: Settled}
	if len(f.script) > 0 {
		step, f.script = f.script[0], f.script[1:]
	}
	f.mu.Unlock()

	if err := sleep(ctx, step.Delay); err != nil {
		return Result{}, err
	}
	if step.Err != nil {
		return Result{}, step.Err
	}
	res := Result{Outcome: step.Outcome, Reason: step.Reason}
	if res.Outcome == "" {
		res.Outcome = Settled
	}
	if res.Outcome != Declined {
		res.Reason = ""
	}
	f.mu.Lock()
	if earlier, ok := f.records[req.Ref]; ok {
		res = earlier // a concurrent call applied it first
	} else {
		f.records[req.Ref] = res
	}
	f.mu.Unlock()
	if step.LoseResponse {
		return Result{}, ErrResponseLost
	}
	return res, nil
}

// Status implements Adapter.
func (f *Fake) Status(_ context.Context, ref string) (Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	res, ok := f.records[ref]
	if !ok {
		return Result{}, ErrUnknownRef
	}
	return res, nil
}

// Resolve finishes a pending operation with outcome (Settled or Declined),
// as an asynchronous PSP's webhook would.
func (f *Fake) Resolve(ref string, outcome Outcome, reason string) error {
	if outcome != Settled && outcome != Declined {
		return ErrInvalidRequest
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	res, ok := f.records[ref]
	if !ok {
		return ErrUnknownRef
	}
	if res.Outcome != Pending {
		return ErrNotPending
	}
	if outcome != Declined {
		reason = ""
	}
	f.records[ref] = Result{Outcome: outcome, Reason: reason}
	return nil
}

// Moved reports whether money moved for ref: its recorded outcome is
// Settled. Tests count effects with it.
func (f *Fake) Moved(ref string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.records[ref].Outcome == Settled
}

// sleep waits d or until ctx ends, whichever is first; a context that has
// already ended returns its error even for d = 0.
func sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
