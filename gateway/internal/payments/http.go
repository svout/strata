package payments

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/svout/agentgate/gateway/internal/httpx"
	"github.com/svout/agentgate/gateway/internal/idem"
	"github.com/svout/agentgate/gateway/internal/store"
)

// retryAfterSeconds is sent with every 503. A fixed, small value tells an
// agent to back off briefly instead of retrying at once; with the same
// Idempotency-Key the retry is safe (ADR-118).
const retryAfterSeconds = "1"

// maxBodyBytes caps a create request; a payment body is a few hundred bytes.
const maxBodyBytes = 64 << 10

// Handler serves POST / and GET /{id}; mount it at /v1/payments on the
// httpx router, which adds the request ID, request log and panic recovery.
// Transitions have no endpoints: rail webhooks drive them (subtasks 9–10).
func (s *Service) Handler(log *slog.Logger) http.Handler {
	h := handler{svc: s, log: log}
	r := chi.NewRouter()
	r.Post("/", h.create)
	r.Get("/{id}", h.get)
	return r
}

var errTrailingData = errors.New("payments: data after the JSON body")

type handler struct {
	svc *Service
	log *slog.Logger
}

type createRequest struct {
	Amount         int64      `json:"amount"`
	Currency       string     `json:"currency"`
	PayerAccountID uuid.UUID  `json:"payer_account_id"`
	PayeeAccountID uuid.UUID  `json:"payee_account_id"`
	MandateID      *uuid.UUID `json:"mandate_id"`
}

// callerID identifies the caller for the key's scope and the request hash.
// P1 has no authentication, so it is the placeholder; P2 returns the agent ID
// from the verified mandate (ADR-118).
func callerID(*http.Request) string { return idem.AnonymousCaller }

func (h handler) create(w http.ResponseWriter, r *http.Request) {
	caller := callerID(r)
	key, err := idem.ParseKey(caller, r.Header.Get(idem.Header))
	if err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "idempotency_key_required", "Idempotency-Key header (1-255 bytes) is required")
		return
	}

	var req createRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	err = dec.Decode(&req)
	if err == nil {
		// Exactly one JSON value: anything after it, even a stray } or ]
		// that dec.More() would miss, makes the body invalid.
		if err = dec.Decode(new(json.RawMessage)); err == io.EOF { //nolint:errorlint // the decoder returns io.EOF itself
			err = nil
		} else if err == nil {
			err = errTrailingData
		}
	}
	if err != nil {
		if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
			httpx.WriteProblem(w, http.StatusRequestEntityTooLarge, "request_too_large", "Request body too large")
			return
		}
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_request", "Request body is not a valid payment")
		return
	}

	// Canonical body: the decoded request re-marshalled. Decoding is strict
	// (unknown fields, trailing data rejected), so nothing is lost, and
	// whitespace or member order can't cause a false mismatch.
	canonical, err := json.Marshal(req)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	hash := idem.Hash(idem.Request{Caller: caller, Method: r.Method, Path: strings.TrimSuffix(r.URL.Path, "/"), Body: canonical})

	res, err := h.svc.CreateIdempotent(r.Context(), key, hash, CreateInput(req))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// uuid.UUID is an array, which the redacting logger omits: log id.String().
	// The key is caller-chosen text, so only its digest is logged.
	msg := "payment created"
	if res.Replayed {
		msg = "payment replayed"
	}
	h.log.LogAttrs(r.Context(), slog.LevelInfo, msg,
		slog.String("payment_id", res.PaymentID.String()),
		slog.Int64("amount", req.Amount), // equal to the stored one: the hash matched
		slog.String("idempotency_key_digest", key.Digest()),
		slog.String("request_id", httpx.RequestID(r.Context())),
	)
	// The same bytes every time: the stored body is replayed as is.
	w.Header().Set("Location", strings.TrimSuffix(r.URL.Path, "/")+"/"+res.PaymentID.String())
	httpx.WriteJSON(w, res.Status, json.RawMessage(res.Body))
}

func (h handler) get(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_payment_id", "Payment id is not a valid UUID")
		return
	}
	p, err := h.svc.Get(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p)
}

// fail maps a service error to a problem response. Unknown errors are logged
// (error text only; see the Logger rules) and become a fixed 500.
func (h handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteProblem(w, http.StatusNotFound, "payment_not_found", "Payment not found")
	case errors.Is(err, ErrMissingAccount):
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_request", "Request body is not a valid payment")
	case errors.Is(err, ErrInvalidAmount):
		httpx.WriteProblem(w, http.StatusUnprocessableEntity, "invalid_amount", "Amount must be a positive number of minor units")
	case errors.Is(err, ErrUnsupportedCurrency):
		httpx.WriteProblem(w, http.StatusUnprocessableEntity, "unsupported_currency", "Currency must be GBP")
	case errors.Is(err, ErrSameAccount):
		httpx.WriteProblem(w, http.StatusUnprocessableEntity, "same_account", "Payer and payee must be different accounts")
	case errors.Is(err, idem.ErrMismatch):
		httpx.WriteProblem(w, http.StatusConflict, "idempotency_key_mismatch", "Idempotency-Key was already used for a different request")
	case errors.Is(err, store.ErrRetryLimit):
		// Too many concurrent transactions on the same rows; safe to retry.
		h.log.LogAttrs(r.Context(), slog.LevelWarn, "payment request retry limit reached",
			slog.String("request_id", httpx.RequestID(r.Context())),
		)
		w.Header().Set("Retry-After", retryAfterSeconds)
		httpx.WriteProblem(w, http.StatusServiceUnavailable, "try_again_later", "Too many concurrent requests; retry later")
	case errors.Is(err, ErrAccountNotFound):
		httpx.WriteProblem(w, http.StatusUnprocessableEntity, "account_not_found", "Payer or payee account not found")
	case errors.Is(err, context.DeadlineExceeded):
		// The router's request deadline ran out (httpx requestTimeout).
		h.log.LogAttrs(r.Context(), slog.LevelError, "payment request timed out",
			slog.String("request_id", httpx.RequestID(r.Context())),
		)
		w.Header().Set("Retry-After", retryAfterSeconds)
		httpx.WriteProblem(w, http.StatusServiceUnavailable, "request_timeout", "Request timed out")
	case errors.Is(err, context.Canceled):
		// The client went away (net/http cancels the request context on
		// disconnect). Not a server error, and there is no one to answer:
		// log at Debug and write nothing (status 0 in the request log).
		h.log.LogAttrs(r.Context(), slog.LevelDebug, "payment request cancelled by client",
			slog.String("request_id", httpx.RequestID(r.Context())),
		)
	default:
		h.log.LogAttrs(r.Context(), slog.LevelError, "payment request failed",
			slog.Any("error", err),
			slog.String("request_id", httpx.RequestID(r.Context())),
		)
		httpx.WriteProblem(w, http.StatusInternalServerError, "internal_error", "Internal Server Error")
	}
}
