// Command fakepsp plays the fake PSP's side of an asynchronous payment, for
// the chaos runs (SPEC-002 11b, AC5). Dev only: it refuses
// STRATA_ENV=production, and the gateway image doesn't contain it (the
// Dockerfile builds ./cmd/gateway only).
//
//	fakepsp pend    --ref pay_<uuid>
//	fakepsp resolve --ref pay_<uuid> --outcome settled|declined [--reason r]
//
// pend records the reference as pending at the fake, as a PSP that accepted
// the payment but hasn't finished it would. resolve finishes a pending one
// (the fake's own Resolve), as that PSP would before sending its webhook.
// Both go through package rail's fake, never SQL of their own (ADR-117), on
// the database in DATABASE_URL. Nothing here applies a migration.
//
// Exit codes: 0 done, 1 failed, 2 usage error, 3 not pending (already
// settled or declined at the fake), 4 the fake has no record of the ref.
package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/svout/strata/gateway/internal/config"
	"github.com/svout/strata/gateway/internal/rail"
	"github.com/svout/strata/gateway/internal/store"
)

const usage = `usage: fakepsp pend --ref pay_<uuid>
       fakepsp resolve --ref pay_<uuid> --outcome settled|declined [--reason r]

Plays the fake PSP for a chaos run, on the database in DATABASE_URL.
Refused under STRATA_ENV=production.
`

// runTimeout bounds a whole run: connecting, then at most three statements
// (pend's read, insert and, if another writer won the insert, re-read).
const runTimeout = 30 * time.Second

const (
	exitOK         = 0
	exitFailed     = 1
	exitUsage      = 2
	exitNotPending = 3
	exitUnknownRef = 4
)

// The fake's Settle validates a whole request, but the fake records only the
// reference and its outcome (fake_rail_operations has no amount or party
// columns), so pend fills the rest with fixed placeholders. Reading the
// payment's real parties would mean reading the payments table from here,
// which only package payments may do (ADR-117).
var (
	placeholderPayer = uuid.MustParse("00000000-0000-4000-8000-0000000fa4e1")
	placeholderPayee = uuid.MustParse("00000000-0000-4000-8000-0000000fa4e2")
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// command is a parsed command line.
type command struct {
	name    string // "pend" or "resolve"
	ref     string
	outcome rail.Outcome
	reason  string
}

// run is main without the process globals, so tests can drive it. Every
// message is fixed text or a reference: the DSN, and a driver's error about
// it, may contain the password.
func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	cmd, ok := parse(args, stderr)
	if !ok {
		printTo(stderr, usage)
		return exitUsage
	}

	cfg, err := config.Load(getenv)
	if err != nil {
		printTo(stderr, "fakepsp: "+err.Error()+"\n")
		return exitFailed
	}
	// Checked before anything connects. config.Load refuses production only
	// together with GATEWAY_ALLOW_FAKE_RAIL=1, which is the gateway's flag,
	// not this tool's, so the check is made here as well.
	if cfg.Env == config.Production {
		printTo(stderr, "fakepsp: refused under STRATA_ENV=production\n")
		return exitFailed
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()

	pool, err := store.OpenPool(ctx, cfg.DatabaseURL)
	if errors.Is(err, store.ErrInvalidDSN) {
		printTo(stderr, "fakepsp: invalid DATABASE_URL\n")
		return exitFailed
	}
	if err != nil {
		printTo(stderr, "fakepsp: could not set up the PostgreSQL pool\n")
		return exitFailed
	}
	defer pool.Close()

	if cmd.name == "pend" {
		return pend(ctx, rail.NewFake(pool, rail.Step{Outcome: rail.Pending}), cmd.ref, stdout, stderr)
	}
	return resolve(ctx, rail.NewFake(pool), cmd, stdout, stderr)
}

// parse reads the command line; false means a usage error (flag has
// already said what was wrong).
func parse(args []string, stderr io.Writer) (command, bool) {
	if len(args) == 0 || (args[0] != "pend" && args[0] != "resolve") {
		return command{}, false
	}
	cmd := command{name: args[0]}
	fs := flag.NewFlagSet("fakepsp "+cmd.name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&cmd.ref, "ref", "", "the rail reference, pay_<uuid>")
	var outcome string
	if cmd.name == "resolve" {
		fs.StringVar(&outcome, "outcome", "", "settled or declined")
		fs.StringVar(&cmd.reason, "reason", "", "the decline reason (declined only)")
	}
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		return command{}, false
	}
	if _, err := rail.ParseRef(cmd.ref); err != nil {
		return command{}, false
	}
	if cmd.name == "resolve" {
		cmd.outcome = rail.Outcome(outcome)
		if cmd.outcome != rail.Settled && cmd.outcome != rail.Declined {
			return command{}, false
		}
		if cmd.reason != "" && cmd.outcome != rail.Declined {
			return command{}, false
		}
	}
	return cmd, true
}

// pend records ref as pending through the fake's Settle with a Pending
// step. A reference the fake already has takes no step and returns its
// record: pending again is success, anything else is not pending.
func pend(ctx context.Context, fake *rail.Fake, ref string, stdout, stderr io.Writer) int {
	id, _ := rail.ParseRef(ref) // parse has checked it
	res, err := fake.Settle(ctx, rail.Request{
		PaymentID: id, Ref: ref, Amount: 1, Currency: "GBP",
		Payer: placeholderPayer, Payee: placeholderPayee,
	})
	if err != nil {
		return failed(err, stderr)
	}
	if res.Outcome != rail.Pending {
		printTo(stderr, "fakepsp: "+ref+" is already "+string(res.Outcome)+" at the fake, not pending\n")
		return exitNotPending
	}
	printTo(stdout, "fakepsp: "+ref+" pending\n")
	return exitOK
}

// resolve finishes a pending reference with the fake's own Resolve.
func resolve(ctx context.Context, fake *rail.Fake, cmd command, stdout, stderr io.Writer) int {
	err := fake.Resolve(ctx, cmd.ref, cmd.outcome, cmd.reason)
	switch {
	case errors.Is(err, rail.ErrNotPending):
		printTo(stderr, "fakepsp: "+cmd.ref+" is not pending at the fake\n")
		return exitNotPending
	case errors.Is(err, rail.ErrUnknownRef):
		printTo(stderr, "fakepsp: the fake has no record of "+cmd.ref+"\n")
		return exitUnknownRef
	case err != nil:
		return failed(err, stderr)
	}
	printTo(stdout, "fakepsp: "+cmd.ref+" "+string(cmd.outcome)+"\n")
	return exitOK
}

// failed reports an error from the fake. Its store errors are a fixed
// sentinel (rail.ErrFakeStore keeps the driver's text out of Error); any
// other error gets a fixed message too, so no driver text reaches the
// terminal whatever the fake returns.
func failed(err error, stderr io.Writer) int {
	if errors.Is(err, rail.ErrFakeStore) {
		printTo(stderr, "fakepsp: the fake PSP's store is unavailable\n")
	} else {
		printTo(stderr, "fakepsp: the fake PSP call failed\n")
	}
	return exitFailed
}

// printTo writes msg. A failed write has nowhere better to be reported, and
// the exit code already carries the outcome.
func printTo(w io.Writer, msg string) {
	_, _ = io.WriteString(w, msg)
}
