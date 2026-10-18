package rail

import (
	"context"
	"errors"
	"sync"
	"time"
)

// FakeName is the fake PSP's name.
const FakeName = "fake"

// ErrResponseLost is the fake's transport failure after the operation was
// applied: the money moved, but the caller can't know (Step.LoseResponse).
var ErrResponseLost = errors.New("rail: fake PSP response lost")

// ErrNotPending means Resolve was called for an operation that isn't pending.
var ErrNotPending = errors.New("rail: operation is not pending")

// DefaultDeclineReason is recorded for a Declined step or Resolve without a
// reason, so a decline always carries one.
const DefaultDeclineReason = "declined"

// Step scripts how the fake answers one Settle that reaches it.
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
	// Reason is recorded with a Declined outcome (DefaultDeclineReason if
	// empty).
	Reason string
	// LoseResponse applies the outcome, then returns ErrResponseLost: the
	// money moved but the caller only learns it through Status.
	LoseResponse bool
}

// Fake is a deterministic in-process PSP for tests and the chaos runs
// (SPEC-002 subtasks 9 and 11).
//
// Each Settle for a reference with no recorded outcome takes the next Step
// of its script; once the script runs out it settles at once. A recorded
// reference returns its recorded outcome and takes no step. Concurrent
// Settle calls for one reference share a single step: the first runs it and
// the others wait and return exactly what it returned (like a PSP answering
// a duplicate in-flight request). So the same script and the same calls give
// the same results however the calls interleave, and at most one outcome is
// ever recorded per reference.
type Fake struct {
	mu       sync.Mutex
	script   []Step
	records  map[string]Result
	inflight map[string]*fakeCall
	moves    map[string]int
}

// fakeCall is a Settle in progress; waiters receive its result.
type fakeCall struct {
	done chan struct{}
	res  Result
	err  error
}

// NewFake returns a fake PSP that answers with steps, in order.
func NewFake(steps ...Step) *Fake {
	return &Fake{
		script:   steps,
		records:  map[string]Result{},
		inflight: map[string]*fakeCall{},
		moves:    map[string]int{},
	}
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
	if c, ok := f.inflight[req.Ref]; ok {
		f.mu.Unlock()
		select {
		case <-c.done:
			return c.res, c.err
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}
	step := Step{Outcome: Settled}
	if len(f.script) > 0 {
		step, f.script = f.script[0], f.script[1:]
	}
	c := &fakeCall{done: make(chan struct{})}
	f.inflight[req.Ref] = c
	f.mu.Unlock()

	c.res, c.err = f.run(ctx, req.Ref, step)
	f.mu.Lock()
	delete(f.inflight, req.Ref)
	f.mu.Unlock()
	close(c.done)
	return c.res, c.err
}

// run performs one step for ref; only the call that took the step runs it.
func (f *Fake) run(ctx context.Context, ref string, step Step) (Result, error) {
	if err := sleep(ctx, step.Delay); err != nil {
		return Result{}, err
	}
	if step.Err != nil {
		return Result{}, step.Err
	}
	res := result(step.Outcome, step.Reason)
	f.mu.Lock()
	f.records[ref] = res
	if res.Outcome == Settled {
		f.moves[ref]++
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
	f.records[ref] = result(outcome, reason)
	if outcome == Settled {
		f.moves[ref]++
	}
	return nil
}

// Moves is how many times money moved for ref. It counts every application
// rather than reading the recorded outcome, so a double application would
// show as 2; tests count effects with it.
func (f *Fake) Moves(ref string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.moves[ref]
}

// result is the recorded form of an outcome: Settled by default, and a
// reason only (and always) with Declined.
func result(outcome Outcome, reason string) Result {
	switch outcome {
	case "":
		outcome = Settled
	case Declined:
		if reason == "" {
			reason = DefaultDeclineReason
		}
		return Result{Outcome: Declined, Reason: reason}
	}
	return Result{Outcome: outcome}
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
