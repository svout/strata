package payments

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/svout/strata/gateway/internal/httpx"
	"github.com/svout/strata/gateway/internal/rail"
)

// Rail webhooks (SPEC-002 subtask 10b, ADR-124, its 2026-10-28 amendment
// and 2026-10-29 addendum). A rail calls POST /{rail} to say an operation
// may have finished. The endpoint is unauthenticated in P1, so the body is
// only a hint naming a reference: nothing else in it is read. The gateway
// asks the rail's own Status and applies that answer as recovery would.
//
// Untrusted is not harmless: each accepted hint costs a Status call, so the
// order of checks puts the cheap, I/O-free ones first:
//
//  1. {rail} must be this Service's rail, else 404 unknown_rail. This is a
//     deliberate asymmetry with step 4: rail names are configuration, which
//     an integrator needs to diagnose a wrong webhook URL; payment existence
//     is data. The 404 does reveal whether the fake rail is enabled, which in
//     production it never is (ADR-124's amendment).
//  2. The body (at most 1 KiB) must be {"ref":"pay_<uuid>"}, else 400.
//  3. A slot must be free, else 503 with Retry-After at once (hint).
//  4. The payment must exist, be on this rail and be settling; anything else
//     is 204 with no rail call.
//  5. Status, then the slot is given back, then the same apply path as
//     Recover.
//
// Every accepted hint gets the same 204 whatever happened in 4 or 5, so the
// response doesn't say which payments exist (timing still differs; accepted
// in the amendment).
//
// Steps 1 and 2 are the handler's (parsing); 3 to 5 are the Service's
// (hint), so they can be tested without HTTP.
//
// The handler is not mounted until 10b-ii-b, behind the per-source rate
// limiter (httpx.RateLimiter, 10b-ii-a):
// an unauthenticated, state-changing endpoint shouldn't exist on the network
// before its protections do.

// maxWebhookBodyBytes caps a webhook body; {"ref":"pay_<uuid>"} is 50 bytes.
const maxWebhookBodyBytes = 1 << 10

// webhookRequest is the generic hint body. Other members are ignored, not
// rejected: the body is only a hint, so there is nothing else to validate.
// Rails with their own format (Stripe, subtask 12) parse it in their adapter.
type webhookRequest struct {
	Ref string `json:"ref"`
}

// hintResult is what the Service did with one webhook hint.
type hintResult int

const (
	hintIgnored hintResult = iota // well-formed, but nothing for the rail to answer
	hintHandled                   // Status asked and its answer applied
	hintShed                      // every slot taken; nothing done
)

// acceptsRail reports whether name is this Service's rail. A Service with
// no rail has no registered rail at all.
func (s *Service) acceptsRail(name string) bool {
	return s.rail != nil && name == s.rail.Name()
}

// hint handles a webhook naming payment id: steps 3 to 5 at the top of this
// file. An error is from the lookup; Status errors are the payment's
// outcome, not the request's, and leave it settling.
func (s *Service) hint(ctx context.Context, id uuid.UUID) (hintResult, error) {
	// 3. The slot is taken before the lookup, not just around Status, for
	// two reasons (ADR-124's addendum): it bounds the database work an
	// unauthenticated caller can cause too, and a 503 that came only after
	// the lookup would tell a caller the payment exists and is settling,
	// undoing the uniform 204. The acquire never waits: queueing here would
	// let a flood pile up requests that each end in a rail call.
	select {
	case s.webhookSlots <- struct{}{}:
	default:
		s.webhookShed.Add(1)
		return hintShed, nil
	}
	a, result, err := s.ask(ctx, id)
	// The slot is free again here. The write below runs without it: the
	// bound caps concurrent rail calls and hides existence, and holding it
	// through the write would serve neither while more than doubling how
	// long a slot can be held (Ivan, 2026-10-29).
	if err != nil || result != hintHandled {
		return result, err
	}
	s.confirm(ctx, a.p, a.res, a.err)
	return hintHandled, nil
}

// statusAnswer is the rail's Status answer for p.
type statusAnswer struct {
	p   Payment
	res rail.Result
	err error // Status's error: the payment's outcome, not the request's
}

// ask is the part of hint that holds the slot, which it gives back on
// return: the lookup and Status, each with its own deadline, so a stalled
// database or a rail that never answers holds a slot for at most
// webhookLookupTimeout + webhookStatusTimeout. The answer is set only when
// the result is hintHandled; the error is a lookup failure.
func (s *Service) ask(ctx context.Context, id uuid.UUID) (statusAnswer, hintResult, error) {
	defer func() { <-s.webhookSlots }()

	// 4. Only a settling payment on this rail goes on to the rail.
	lctx, cancel := context.WithTimeout(ctx, s.webhookLookupTimeout)
	p, err := s.Get(lctx, id)
	cancel()
	switch {
	case errors.Is(err, ErrNotFound):
		return statusAnswer{}, s.ignore(ctx, id, "unknown payment"), nil
	case err != nil:
		return statusAnswer{}, hintIgnored, err
	case p.Rail != s.rail.Name():
		return statusAnswer{}, s.ignore(ctx, id, "payment is on another rail"), nil
	case p.State != Settling:
		// This check is what keeps a late, duplicate or out-of-order
		// webhook from touching a finished payment (AC5): it never reaches
		// the rail or the apply path at all. It is not just an
		// optimisation. A finish that races in after it is still caught by
		// the forward-only transition (first writer wins, in finish).
		return statusAnswer{}, s.ignore(ctx, id, "payment is not settling"), nil
	}

	// 5. Status, with its own deadline inside whatever ctx allows.
	sctx, cancel := context.WithTimeout(ctx, s.webhookStatusTimeout)
	defer cancel()
	res, err := s.rail.Status(sctx, ref(p))
	return statusAnswer{p: p, res: res, err: err}, hintHandled, nil
}

// ignore logs a well-formed hint that needs no rail call, at Debug: a flood
// of these is the rate limiter's business (10b-ii), not the log's.
func (s *Service) ignore(ctx context.Context, id uuid.UUID, why string) hintResult {
	s.log.LogAttrs(ctx, slog.LevelDebug, "rail webhook ignored: "+why,
		slog.String("payment_id", id.String()))
	return hintIgnored
}

// Webhook serves POST /{rail}; mount it at /v1/rail/webhooks on the httpx
// router, behind httpx.RateLimiter (10b-ii-b).
func (s *Service) Webhook(log *slog.Logger) http.Handler {
	h := handler{svc: s, log: log}
	r := chi.NewRouter()
	r.Post("/{rail}", h.webhook)
	return r
}

func (h handler) webhook(w http.ResponseWriter, r *http.Request) {
	// 1. The rail, and 2. the shape, both before any I/O.
	if !h.svc.acceptsRail(chi.URLParam(r, "rail")) {
		httpx.WriteProblem(w, http.StatusNotFound, "unknown_rail", "Unknown rail")
		return
	}
	var req webhookRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxWebhookBodyBytes)).Decode(&req); err != nil {
		if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
			httpx.WriteProblem(w, http.StatusRequestEntityTooLarge, "request_too_large", "Request body too large")
			return
		}
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_ref", `Body must be {"ref":"pay_<uuid>"}`)
		return
	}
	id, err := rail.ParseRef(req.Ref)
	if err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_ref", `Body must be {"ref":"pay_<uuid>"}`)
		return
	}

	result, err := h.svc.hint(r.Context(), id)
	switch {
	case err != nil:
		h.failWebhook(w, r, err)
	case result == hintShed:
		// Debug, not Warn (Ivan, 2026-10-29): WebhookShed is the signal. A
		// line per shed request on an unauthenticated endpoint would let a
		// flood fill the log and bury the real signal.
		h.log.LogAttrs(r.Context(), slog.LevelDebug, "rail webhook shed: every slot is taken",
			slog.String("rail", chi.URLParam(r, "rail")),
			slog.String("request_id", httpx.RequestID(r.Context())),
		)
		w.Header().Set("Retry-After", retryAfterSeconds)
		httpx.WriteProblem(w, http.StatusServiceUnavailable, "try_again_later", "Too many concurrent webhooks; retry later")
	default:
		// The outcome is logged by confirm and finish; the caller learns
		// nothing from it.
		w.WriteHeader(http.StatusNoContent)
	}
}

// failWebhook maps a webhook's lookup error to a response. It is separate
// from fail on purpose, and must stay so (Ivan, 2026-10-29):
//
//   - fail turns ErrNotFound into 404 payment_not_found. A webhook error
//     that reached it would reopen the existence oracle the uniform 204
//     closes (ADR-124's amendment). Here every error is a 503 or a 500,
//     or nothing at all when the caller has gone.
//   - Its log lines name the webhook. AC8 and chapter 5 are read from these
//     logs, so a webhook failure logged as "payment request failed" would
//     be mislabelled evidence, not just an untidy message.
func (h handler) failWebhook(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		// The lookup's own deadline (webhookLookupTimeout) or the
		// router's ran out.
		h.log.LogAttrs(r.Context(), slog.LevelError, "rail webhook timed out",
			slog.String("request_id", httpx.RequestID(r.Context())),
		)
		w.Header().Set("Retry-After", retryAfterSeconds)
		httpx.WriteProblem(w, http.StatusServiceUnavailable, "request_timeout", "Request timed out")
	case errors.Is(err, context.Canceled):
		// The caller went away; there is no one to answer (as in fail).
		h.log.LogAttrs(r.Context(), slog.LevelDebug, "rail webhook cancelled by caller",
			slog.String("request_id", httpx.RequestID(r.Context())),
		)
	default:
		h.log.LogAttrs(r.Context(), slog.LevelError, "rail webhook failed",
			slog.Any("error", err),
			slog.String("request_id", httpx.RequestID(r.Context())),
		)
		httpx.WriteProblem(w, http.StatusInternalServerError, "internal_error", "Internal Server Error")
	}
}
