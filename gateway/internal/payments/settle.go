package payments

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/svout/strata/gateway/internal/ledger"
	"github.com/svout/strata/gateway/internal/rail"
	"github.com/svout/strata/gateway/internal/store"
)

// Settlement (SPEC-002 subtask 10a): turning a settling payment into settled
// or failed through its rail, under the rail contract (ADR-120):
//
//   - Settled -> settled; Declined -> failed (declined).
//   - rail.ErrInvalidRequest -> failed (invalid_request), logged at Error and
//     never retried: the same request fails the same way every time, so a
//     retry is an endless loop, and a request the rail can't parse is our bug.
//   - Pending, or any other error -> the payment stays settling. An error
//     means the outcome is unknown, not that it failed; Recover asks again.
//
// Writes after a definitive answer run on a context the caller can't cancel:
// once the rail has answered, the answer is recorded even if the client has
// gone. If the write fails anyway, the payment stays settling and the next
// Recover gets the same answer from the rail's Status.

const (
	// defaultSettleTimeout bounds one rail call, and separately the write
	// that records its answer. The internal rail and the fake answer in
	// milliseconds; it is sized for a network PSP (subtask 12).
	defaultSettleTimeout = 3 * time.Second
	defaultRecoverPage   = 100
	// defaultRecoverGrace is how long a payment must have been settling
	// before Recover touches it, so a pass doesn't race the request that is
	// still settling it (two rail calls for one reference; a network PSP
	// may answer the second with a conflict). It is measured from
	// updated_at, the start of the claim transaction, so it covers that
	// transaction (bounded by the 5 s request deadline) plus the rail call
	// and the write after it (settleTimeout each): 11 s, rounded up.
	defaultRecoverGrace = 15 * time.Second

	// defaultWebhookBound is how many webhooks may be asking the rail's
	// Status at once (ADR-124's amendment: default 8, configurable).
	defaultWebhookBound = 8
	// defaultWebhookStatusTimeout bounds one webhook's Status call, and so
	// how long it holds a slot. Without it a rail that accepts the
	// connection and never answers would hold every slot until the
	// router's 5 s request deadline, and keep doing so for as long as it
	// hangs: every other webhook gets 503 meanwhile. It is shorter than
	// defaultSettleTimeout because Status only reads (assumed: a PSP
	// answers a status read well within 2 s; revisit with Stripe in 12).
	defaultWebhookStatusTimeout = 2 * time.Second
)

// outcome is what one settle attempt left the payment as.
type outcome int

const (
	outcomeSettled outcome = iota
	outcomeFailed
	outcomePending // the rail is still working on it
	outcomeUnknown // no definitive answer; Recover asks again
)

// RecoverStats counts, for one Recover pass, how each payment it listed
// ended up when the pass looked at it: Settled and Failed count payments the
// pass found in that final state after acting, including ones another
// worker (a concurrent pass or the creating request) finished first, so the
// counts across concurrent passes can add up to more than the backlog.
// Unknown also counts a payment whose recorded outcome contradicts the
// rail's answer (logged at Error; the first outcome is kept).
// They are not "payments this pass recovered"; that number needs metrics
// (SPEC-002 subtask 13) and must not be quoted from these counters.
type RecoverStats struct {
	Settled, Failed, Pending, Unknown int
}

// settle calls the rail for a payment this request has just created. The
// call runs on a context the client can't cancel, with its own deadline, so
// a client that disconnects can't leave the money half-moved: the rail call
// and the write after it finish (or time out) either way.
func (s *Service) settle(ctx context.Context, p Payment) outcome {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.settleTimeout)
	defer cancel()
	res, err := s.rail.Settle(ctx, request(p))
	return s.apply(ctx, p, res, err)
}

// Recover settles every payment left settling on this Service's rail for
// longer than the grace period (defaultRecoverGrace): after
// a crash between the outbox commit and the rail's answer, a lost response,
// or a pending operation that has since finished (AC4). For each it asks the
// rail's Status; a reference the rail has never seen didn't take effect, so
// it is settled again with the same reference (ADR-120), and anything else
// is applied as settle's answer would be. Payments that are still pending or
// get no answer stay settling for the next pass.
//
// Several Recover passes (or a pass and a live request) may race on one
// payment: the rail moves money once per reference, and the forward-only
// transition lets only one of them record the outcome. Recover returns an
// error only when it can't read the backlog or ctx ends.
func (s *Service) Recover(ctx context.Context) (RecoverStats, error) {
	var st RecoverStats
	if s.rail == nil {
		return st, ErrNoRail
	}
	for after := (settlingCursor{}); ; {
		page, err := listSettling(ctx, s.pool, s.rail.Name(), s.recoverGrace, after, s.recoverPage)
		if err != nil {
			return st, err
		}
		for _, p := range page {
			// Checked per payment, not per page: once ctx ends, every
			// further rail call would fail and be logged as unknown.
			if err := ctx.Err(); err != nil {
				return st, err
			}
			switch s.resolve(ctx, p) {
			case outcomeSettled:
				st.Settled++
			case outcomeFailed:
				st.Failed++
			case outcomePending:
				st.Pending++
			case outcomeUnknown:
				st.Unknown++
			}
			after = settlingCursor{updatedAt: p.UpdatedAt, id: p.ID}
		}
		if len(page) < s.recoverPage {
			break
		}
	}
	s.log.LogAttrs(ctx, slog.LevelInfo, "settlement recovery finished",
		slog.String("rail", s.rail.Name()),
		slog.Int("settled", st.Settled), slog.Int("failed", st.Failed),
		slog.Int("pending", st.Pending), slog.Int("unknown", st.Unknown))
	return st, nil
}

// resolve finds out what happened to one settling payment. Unlike settle,
// the rail call honours ctx: a Recover stopped at shutdown leaves the
// payment for the next pass.
func (s *Service) resolve(ctx context.Context, p Payment) outcome {
	cctx, cancel := context.WithTimeout(ctx, s.settleTimeout)
	defer cancel()
	res, err := s.rail.Status(cctx, ref(p))
	if errors.Is(err, rail.ErrUnknownRef) {
		res, err = s.rail.Settle(cctx, request(p))
		return s.apply(ctx, p, res, err)
	}
	if err != nil {
		// Status has no "invalid request" case: any error is unknown.
		return s.unknown(ctx, p, err)
	}
	return s.apply(ctx, p, res, nil)
}

// confirm is resolve for a rail webhook (ADR-124): it asks the rail's
// Status for one settling payment and applies the answer, with a deadline
// of its own (webhookStatusTimeout) inside whatever ctx allows.
//
// There is deliberately no Settle fallback here, unlike resolve. If the
// rail has no record of the reference (ErrUnknownRef), the payment stays
// settling and Recover, after its grace period, settles it. A fallback
// would let a forged webhook make the gateway settle a payment the rail
// never saw, which is exactly what ADR-124 exists to prevent (Ivan,
// 2026-10-28). This is not a missing case.
func (s *Service) confirm(ctx context.Context, p Payment) outcome {
	cctx, cancel := context.WithTimeout(ctx, s.webhookStatusTimeout)
	defer cancel()
	res, err := s.rail.Status(cctx, ref(p))
	if errors.Is(err, rail.ErrUnknownRef) {
		s.log.LogAttrs(ctx, slog.LevelWarn, "rail has no record of a webhook's payment; left settling for recovery",
			slog.String("payment_id", p.ID.String()), slog.String("rail", p.Rail))
		return outcomeUnknown
	}
	if err != nil {
		return s.unknown(ctx, p, err)
	}
	// apply records the answer on a context the caller can't cancel (finish
	// detaches it), so once the rail has answered, a client that hangs up
	// doesn't lose it.
	return s.apply(ctx, p, res, nil)
}

// apply records the answer to a Settle call (see the top of this file).
func (s *Service) apply(ctx context.Context, p Payment, res rail.Result, err error) outcome {
	switch {
	case errors.Is(err, rail.ErrInvalidRequest):
		s.log.LogAttrs(ctx, slog.LevelError, "rail rejected the settle request as invalid; payment failed, not retried",
			slog.String("payment_id", p.ID.String()), slog.String("rail", p.Rail))
		return s.finish(ctx, p, Failed, FailureInvalidRequest)
	case err != nil:
		return s.unknown(ctx, p, err)
	}
	switch res.Outcome {
	case rail.Settled:
		return s.finish(ctx, p, Settled, "")
	case rail.Declined:
		return s.finish(ctx, p, Failed, FailureDeclined)
	case rail.Pending:
		return outcomePending
	default:
		s.log.LogAttrs(ctx, slog.LevelError, "rail returned an unknown outcome; payment left settling",
			slog.String("payment_id", p.ID.String()), slog.String("rail", p.Rail), slog.String("outcome", string(res.Outcome)))
		return outcomeUnknown
	}
}

// unknown logs an attempt with no definitive answer. The error text is the
// rail's or the driver's; adapters keep secrets and URLs out of it.
func (s *Service) unknown(ctx context.Context, p Payment, err error) outcome {
	s.log.LogAttrs(ctx, slog.LevelWarn, "settle outcome unknown; payment left settling for recovery",
		slog.String("payment_id", p.ID.String()), slog.String("rail", p.Rail), slog.String("error", err.Error()))
	return outcomeUnknown
}

// PayerRef is the rail reference of the payer's posting for payment id
// (ADR-123): payer debit, clearing credit. It differs from the rail's own
// reference (rail.Ref), so the ledger's at-most-once constraint holds each
// posting separately. It is exported because it is a durable fact in
// ledger_entries.rail_ref: anything that reconstructs a payment's postings
// (dispute replay, the console, reconciliation) computes it, as with
// rail.Ref.
func PayerRef(id uuid.UUID) string { return "payer_" + id.String() }

// markSettled moves payment id to settled and posts the payer's side, in one
// SERIALIZABLE transaction, so a payment is never settled without it
// (ADR-123). It is the only way to reach settled: there is no exported
// MarkSettled, because a path to settled without this posting would break
// AC1 silently (SPEC-002 10a-ii-a2). The order is deliberate (Ivan,
// 2026-10-19):
//
//   - The transition goes first. A writer that lost the race gets
//     ErrInvalidTransition from the state machine, as before, and finish
//     treats it as first-writer-wins. Posting first would make the ledger's
//     unique constraint the state machine's concurrency control.
//
//   - There is no ledger.Posted pre-read. Only the forward-only
//     settling -> settled UPDATE can lead to this posting, so it already
//     admits one writer; the unique constraint is the backstop (ADR-120).
//     A read here would take predicate locks on ledger_entries inside the
//     transaction, the conflict ADR-120 moved the rail's read out to avoid.
//
//   - The posting takes the payer and amount from the row the transition
//     returns, never from the caller's copy: what is posted is what this
//     transaction committed (Ivan, 2026-10-21). The caller's copy may be a
//     snapshot from a Recover page or an earlier request.
//
// If PostRef fails after the transition, or the commit fails after both, the
// whole transaction rolls back and the payment stays settling for Recover.
func (s *Service) markSettled(ctx context.Context, id uuid.UUID) error {
	return store.Serializable(ctx, s.pool, func(tx pgx.Tx) error {
		p, err := transition(ctx, tx, id, Settled, nil, nil)
		if err != nil {
			return err
		}
		return ledger.PostRef(ctx, tx, p.ID, PayerRef(p.ID), []ledger.Entry{
			{AccountID: p.PayerAccountID, Direction: ledger.Debit, Amount: p.Amount},
			{AccountID: uuid.UUID(s.clearing), Direction: ledger.Credit, Amount: p.Amount},
		})
	})
}

// finish moves p to settled or failed. If another settle attempt got there
// first, the transition is refused (ErrInvalidTransition) and the first
// writer wins: the payment keeps what was recorded first. The same final
// state and reason means the work is done; anything else (settled against
// failed, or declined against invalid_request) means the rail answered two
// attempts differently, which is logged at Error for someone to look at.
func (s *Service) finish(ctx context.Context, p Payment, to State, reason FailureReason) outcome {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.settleTimeout)
	defer cancel()
	var err error
	if to == Settled {
		err = s.markSettled(ctx, p.ID)
	} else {
		_, err = s.MarkFailed(ctx, p.ID, reason)
	}
	if errors.Is(err, ErrInvalidTransition) {
		cur, gerr := s.Get(ctx, p.ID)
		switch {
		case gerr != nil:
			err = gerr
		case cur.State == to && (to != Failed || *cur.FailureReason == reason):
			err = nil
		default:
			// The rail's answers are deterministic per reference, so two
			// attempts reaching different final states means a rail broke
			// its contract. Someone has to look.
			attrs := []slog.Attr{slog.String("payment_id", p.ID.String()), slog.String("rail", p.Rail),
				slog.String("state", string(cur.State)), slog.String("rail_outcome", string(to))}
			if cur.FailureReason != nil {
				attrs = append(attrs, slog.String("failure_reason", string(*cur.FailureReason)))
			}
			if to == Failed {
				attrs = append(attrs, slog.String("rail_failure_reason", string(reason)))
			}
			s.log.LogAttrs(ctx, slog.LevelError, "payment already finished differently than the rail now reports; keeping the first outcome", attrs...)
			return outcomeUnknown
		}
	}
	if err != nil {
		s.log.LogAttrs(ctx, slog.LevelWarn, "could not record the rail's answer; payment left settling for recovery",
			slog.String("payment_id", p.ID.String()), slog.String("rail", p.Rail), slog.String("error", err.Error()))
		return outcomeUnknown
	}
	attrs := []slog.Attr{slog.String("payment_id", p.ID.String()), slog.String("rail", p.Rail)}
	if to == Settled {
		s.log.LogAttrs(ctx, slog.LevelInfo, "payment settled", attrs...)
		return outcomeSettled
	}
	s.log.LogAttrs(ctx, slog.LevelInfo, "payment failed", append(attrs, slog.String("failure_reason", string(reason)))...)
	return outcomeFailed
}

// ref is the rail reference recorded when p moved to settling. Every
// settling payment has one (MarkSettling requires it); the derived reference
// is only a fallback.
func ref(p Payment) string {
	if p.RailRef != nil {
		return *p.RailRef
	}
	return rail.Ref(p.ID)
}

func request(p Payment) rail.Request {
	return rail.Request{
		PaymentID: p.ID, Ref: ref(p), Amount: p.Amount, Currency: p.Currency,
		Payer: p.PayerAccountID, Payee: p.PayeeAccountID,
	}
}
