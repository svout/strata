package rail

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
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
	// Delay is waited before anything is applied (after the fake has found
	// no record of the reference); if the context ends first, Settle returns
	// its error and nothing is applied.
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
	// money moved but the caller only learns it through Status. It applies
	// even when another writer's record wins the insert: that caller moved
	// nothing, but an unknown outcome is still safe, since Status returns the
	// record that stands (as a real PSP may lose the reply to a duplicate).
	LoseResponse bool
}

// Fake is a deterministic PSP for tests and the chaos runs (SPEC-002
// subtasks 9 and 11).
//
// Each Settle for a reference with no recorded outcome takes the next Step
// of its script; once the script runs out it settles at once. A recorded
// reference returns its recorded outcome and takes no step. Concurrent
// Settle calls for one reference share a single step: the first runs it and
// the others wait and return exactly what it returned (like a PSP answering
// a duplicate in-flight request). So the same script and the same calls give
// the same results however the calls interleave, and at most one outcome is
// ever recorded per reference.
//
// What is kept where (subtask 10a-ii-c). The record of each reference (its
// outcome and how many times money moved) is in PostgreSQL, in the fake's
// own table fake_rail_operations (migration 00006, SQL in store.go). That is
// the PSP's memory, and it has to outlive the gateway: if a restarted
// gateway's fake forgot a reference, recovery would see ErrUnknownRef, call
// Settle again, and the fake would pay twice, while the ledger's unique
// constraint hid it (the gateway posts payer_<id> once whatever the rail
// does). The script and the in-flight calls stay in memory: they are the
// test's instructions to this one process, not facts about the PSP, and a
// fresh Fake after a restart starts with a fresh script.
//
// Across processes (two gateways, or a gateway and its restart, on one
// database) the first writer wins: the insert is ON CONFLICT DO NOTHING and
// the loser returns the row that stands, so money still moves once.
type Fake struct {
	pool *pgxpool.Pool

	mu       sync.Mutex
	script   []Step
	inflight map[string]*fakeCall
}

// fakeCall is a Settle in progress; waiters receive its result.
type fakeCall struct {
	done chan struct{}
	res  Result
	err  error
}

// NewFake returns a fake PSP that keeps its records in pool's
// fake_rail_operations table and answers with steps, in order. Fakes on the
// same database share their records (as one PSP would), not their scripts.
// There is deliberately no in-memory variant: one that forgets references
// on restart is the bug this type exists to rule out.
func NewFake(pool *pgxpool.Pool, steps ...Step) *Fake {
	return &Fake{
		pool:     pool,
		script:   steps,
		inflight: map[string]*fakeCall{},
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

// Settle implements Adapter. A database error is ErrFakeStore: an unknown
// outcome, as a real PSP's transport failure would be.
func (f *Fake) Settle(ctx context.Context, req Request) (Result, error) {
	if err := validate(req); err != nil {
		return Result{}, err
	}
	// In-flight sharing comes first and needs no I/O: a duplicate of a call
	// in progress in this process waits for it. The database read happens
	// outside the mutex, so settles of different references don't queue
	// behind each other's round trips.
	f.mu.Lock()
	if c, ok := f.inflight[req.Ref]; ok {
		f.mu.Unlock()
		select {
		case <-c.done:
			return c.res, c.err
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}
	c := &fakeCall{done: make(chan struct{})}
	f.inflight[req.Ref] = c
	f.mu.Unlock()

	c.res, c.err = f.settle(ctx, req.Ref)
	f.mu.Lock()
	delete(f.inflight, req.Ref)
	f.mu.Unlock()
	close(c.done)
	return c.res, c.err
}

// settle answers one Settle for ref; only the call that registered the
// in-flight entry runs it.
func (f *Fake) settle(ctx context.Context, ref string) (Result, error) {
	// A recorded reference (by this process, an earlier one, or another
	// gateway) returns its outcome and takes no step.
	if res, found, err := getFakeOperation(ctx, f.pool, ref); err != nil || found {
		return res, err
	}
	// The step is taken only now, after the read, so a call that fails
	// during the read (cancelled context, database down) leaves the script
	// as it was. A failure after this point (the Delay, a scripted Err, or
	// the insert) has used the step up.
	step := f.next()
	if err := sleep(ctx, step.Delay); err != nil {
		return Result{}, err
	}
	if step.Err != nil {
		return Result{}, step.Err
	}
	// insertFakeOperation returns the record that stands: ours, or another
	// process's that won the race, in which case the loser reports that
	// outcome and moved nothing.
	res, err := insertFakeOperation(ctx, f.pool, ref, result(step.Outcome, step.Reason))
	if err != nil {
		return Result{}, err
	}
	if step.LoseResponse {
		return Result{}, ErrResponseLost
	}
	return res, nil
}

// next takes the next step of the script, or an immediate Settled once it
// has run out.
func (f *Fake) next() Step {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.script) == 0 {
		return Step{Outcome: Settled}
	}
	step := f.script[0]
	f.script = f.script[1:]
	return step
}

// Status implements Adapter. A database error is ErrFakeStore, never
// ErrUnknownRef: "couldn't ask" is not "never heard of it".
func (f *Fake) Status(ctx context.Context, ref string) (Result, error) {
	res, found, err := getFakeOperation(ctx, f.pool, ref)
	if err != nil {
		return Result{}, err
	}
	if !found {
		return Result{}, ErrUnknownRef
	}
	return res, nil
}

// Resolve finishes a pending operation with outcome (Settled or Declined),
// as an asynchronous PSP's webhook would. Only a pending operation changes
// (ErrNotPending otherwise, ErrUnknownRef if there is none), and the check
// is made by the database in the same statement as the update, so it holds
// across processes.
func (f *Fake) Resolve(ctx context.Context, ref string, outcome Outcome, reason string) error {
	if outcome != Settled && outcome != Declined {
		return ErrInvalidRequest
	}
	return resolveFakeOperation(ctx, f.pool, ref, result(outcome, reason))
}

// Moves is how many times money moved for ref (0 if the fake has no record
// of it). It reads a counter that each application increments rather than
// deriving it from the recorded outcome, so a double application would show
// as 2; tests count effects with it.
func (f *Fake) Moves(ctx context.Context, ref string) (int, error) {
	return fakeMoves(ctx, f.pool, ref)
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
