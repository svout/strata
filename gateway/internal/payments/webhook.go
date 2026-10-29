package payments

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/svout/strata/gateway/internal/httpx"
	"github.com/svout/strata/gateway/internal/rail"
)

// Rail webhooks (SPEC-002 subtask 10b, ADR-124 and its 2026-10-28
// amendment). A rail calls POST /{rail} to say an operation may have
// finished. The endpoint is unauthenticated in P1, so the body is only a
// hint naming a reference: nothing else in it is read. The gateway asks the
// rail's own Status and applies that answer as recovery would.
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
//  3. A Status slot must be free, else 503 with Retry-After at once.
//  4. The payment must exist, be on this rail and be settling; anything else
//     is 204 with no rail call.
//  5. Status, then the same apply path as Recover.
//
// Every accepted hint gets the same 204 whatever happened in 4 or 5, so the
// response doesn't say which payments exist (timing still differs; accepted
// in the amendment).
//
// The handler is not mounted until 10b-ii adds the per-source rate limit:
// an unauthenticated, state-changing endpoint shouldn't exist on the network
// before its protections do.

// maxWebhookBodyBytes caps a webhook body; {"ref":"pay_<uuid>"} is 49 bytes.
const maxWebhookBodyBytes = 1 << 10

// webhookRequest is the generic hint body. Other members are ignored, not
// rejected: the body is only a hint, so there is nothing else to validate.
// Rails with their own format (Stripe, subtask 12) parse it in their adapter.
type webhookRequest struct {
	Ref string `json:"ref"`
}

// Webhook serves POST /{rail}; mount it at /v1/rail/webhooks on the httpx
// router (10b-ii).
func (s *Service) Webhook(log *slog.Logger) http.Handler {
	h := handler{svc: s, log: log}
	r := chi.NewRouter()
	r.Post("/{rail}", h.webhook)
	return r
}

func (h handler) webhook(w http.ResponseWriter, r *http.Request) {
	s := h.svc
	// 1. A Service with no rail has no registered rail at all.
	if s.rail == nil || chi.URLParam(r, "rail") != s.rail.Name() {
		httpx.WriteProblem(w, http.StatusNotFound, "unknown_rail", "Unknown rail")
		return
	}

	// 2. Shape, before any I/O.
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

	// 3. The slot is taken before the lookup, not just around Status, for
	// two reasons: it bounds the database work an unauthenticated caller
	// can cause too, and a 503 that came only after the lookup would tell
	// a caller the payment exists and is settling, undoing the uniform 204.
	// The acquire never waits: queueing here would let a flood pile up
	// requests that each end in a rail call.
	select {
	case s.webhookSlots <- struct{}{}:
		defer func() { <-s.webhookSlots }()
	default:
		s.webhookShed.Add(1)
		h.log.LogAttrs(r.Context(), slog.LevelWarn, "rail webhook shed: every Status slot is taken",
			slog.String("rail", s.rail.Name()),
			slog.String("request_id", httpx.RequestID(r.Context())),
		)
		w.Header().Set("Retry-After", retryAfterSeconds)
		httpx.WriteProblem(w, http.StatusServiceUnavailable, "try_again_later", "Too many concurrent webhooks; retry later")
		return
	}

	// 4. Only a settling payment on this rail goes on to the rail.
	p, err := s.Get(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		h.ignore(w, r, id, "unknown payment")
		return
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if p.Rail != s.rail.Name() {
		h.ignore(w, r, id, "payment is on another rail")
		return
	}
	// This check is what keeps a late, duplicate or out-of-order webhook
	// from touching a finished payment (AC5): it never reaches the rail or
	// the apply path at all. It is not just an optimisation. A finish that
	// races in after it is still caught by the forward-only transition
	// (first writer wins, in finish).
	if p.State != Settling {
		h.ignore(w, r, id, "payment is not settling")
		return
	}

	// 5. The outcome is logged by confirm and finish; the caller learns
	// nothing from it.
	s.confirm(r.Context(), p)
	w.WriteHeader(http.StatusNoContent)
}

// ignore answers a well-formed hint that needs no rail call with the same
// 204 as a handled one. It logs at Debug: a flood of these is the rate
// limiter's business (10b-ii), not the log's.
func (h handler) ignore(w http.ResponseWriter, r *http.Request, id uuid.UUID, why string) {
	h.log.LogAttrs(r.Context(), slog.LevelDebug, "rail webhook ignored: "+why,
		slog.String("payment_id", id.String()),
		slog.String("request_id", httpx.RequestID(r.Context())),
	)
	w.WriteHeader(http.StatusNoContent)
}
