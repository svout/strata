package payments

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/svout/strata/gateway/internal/httpx"
	"github.com/svout/strata/gateway/internal/rail"
	"github.com/svout/strata/gateway/internal/testutil"
)

// SPEC-002 subtask 10b-i: the rail webhook (ADR-124 and its 2026-10-28
// amendment). The handler is mounted here on the real httpx router, as
// 10b-ii will mount it in cmd/gateway. AC5 itself is ticked only after
// subtask 11's chaos run.

// webhookAPI is a Service settling on a fake PSP (wrapped to count calls),
// with its webhook handler on the httpx router.
type webhookAPI struct {
	t      *testing.T
	pool   *pgxpool.Pool
	fake   *rail.Fake
	hook   *hookRail
	svc    *Service
	router *chi.Mux
	log    *syncBuffer
	payer  uuid.UUID
	payee  uuid.UUID
}

func newWebhookAPI(t *testing.T, steps []rail.Step, opts ...Option) *webhookAPI {
	t.Helper()
	pool := testutil.Postgres(t)
	fake := rail.NewFake(pool, steps...)
	hook := &hookRail{Adapter: fake}
	a := newWebhookAPIOn(t, pool, hook, opts...)
	a.fake, a.hook = fake, hook
	return a
}

// newWebhookAPIOn is newWebhookAPI on any rail r; fake and hook are unset.
func newWebhookAPIOn(t *testing.T, pool *pgxpool.Pool, r rail.Adapter, opts ...Option) *webhookAPI {
	t.Helper()
	var buf syncBuffer
	log := httpx.NewLogger(&buf, slog.LevelDebug)
	svc := New(pool, append([]Option{WithRail(r, clearingFor(t, pool, r.Name())), WithLogger(log)}, opts...)...)
	router := httpx.NewRouter(log)
	router.Mount("/v1/rail/webhooks", svc.Webhook(log))
	return &webhookAPI{t: t, pool: pool, svc: svc, router: router, log: &buf,
		payer: newAccount(t.Context(), t, pool), payee: newAccount(t.Context(), t, pool)}
}

// post sends body to the webhook for railName.
func (a *webhookAPI) post(railName, body string) *httptest.ResponseRecorder {
	a.t.Helper()
	rec := httptest.NewRecorder()
	a.router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/rail/webhooks/"+railName, strings.NewReader(body)))
	return rec
}

// hint is the generic webhook body naming payment id's reference.
func hint(id uuid.UUID) string { return `{"ref":"` + rail.Ref(id) + `"}` }

// create makes a payment through the Service, so it is settled on the fake
// with the fake's next step.
func (a *webhookAPI) create(key string) uuid.UUID {
	a.t.Helper()
	return createFor(a.t, a.svc, key, a.payer, a.payee).PaymentID
}

func (a *webhookAPI) resolve(id uuid.UUID, o rail.Outcome) {
	a.t.Helper()
	if err := a.fake.Resolve(a.t.Context(), rail.Ref(id), o, ""); err != nil {
		a.t.Fatal(err)
	}
}

func wantCode(t *testing.T, rec *httptest.ResponseRecorder, code int) {
	t.Helper()
	if rec.Code != code {
		t.Fatalf("status = %d, want %d: %s", rec.Code, code, rec.Body)
	}
}

// AC5: 32 concurrent duplicate webhooks for one payment settle it once. Money
// moves once at the rail, the payer posting is two legs, and only create
// ever calls Settle: the webhook path asks Status and nothing else.
func TestWebhook_AC5_ConcurrentDuplicatesSettleOnce(t *testing.T) {
	t.Parallel()
	const n = 32
	a := newWebhookAPI(t, []rail.Step{{Outcome: rail.Pending}}, WithWebhookBound(n))
	id := a.create("k")
	a.resolve(id, rail.Settled)

	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := range n {
		wg.Go(func() { codes[i] = a.post(rail.FakeName, hint(id)).Code })
	}
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusNoContent {
			t.Fatalf("webhook %d = %d, want 204", i, c)
		}
	}
	assertState(t, a.svc, id, Settled, nil)
	if got := fakeMoves(t, a.fake, id); got != 1 {
		t.Fatalf("money moved %d times, want 1", got)
	}
	if got := payerEntries(t, a.pool, id); got != 2 {
		t.Fatalf("payer posting legs = %d, want 2", got)
	}
	if got := a.hook.settles.Load(); got != 1 {
		t.Fatalf("Settle called %d times, want 1 (create only)", got)
	}
	assertBalances(t, a.pool, map[uuid.UUID]int64{a.payer: -1250, uuid.UUID(clearingFor(t, a.pool, rail.FakeName)): 1250})
}

// AC5, out of order: a webhook for a payment that isn't settling on this
// rail gets the same 204 as a handled one, and the rail is never asked.
func TestWebhook_NotSettlingIs204WithoutRailCall(t *testing.T) {
	t.Parallel()
	a := newWebhookAPI(t, []rail.Step{{}, {Outcome: rail.Declined}})
	settled := a.create("settled")
	failed := a.create("failed")
	created, err := a.svc.create(t.Context(), CreateInput{Amount: 1250, Currency: Currency, PayerAccountID: a.payer, PayeeAccountID: a.payee})
	if err != nil {
		t.Fatal(err)
	}
	// Settling, but on the internal rail (it answers pending here, so the
	// payment stays settling): only the rail check can turn it away.
	internal := New(a.pool, WithRail(&hookRail{Adapter: internalRail(t, a.pool), answer: &rail.Result{Outcome: rail.Pending}},
		clearingFor(t, a.pool, rail.InternalName)))
	other := createFor(t, internal, "other rail", a.payer, a.payee).PaymentID
	assertState(t, internal, other, Settling, nil)

	for name, id := range map[string]uuid.UUID{
		"settled": settled, "failed": failed, "created": created.ID,
		"other rail": other, "unknown payment": uuid.New(),
	} {
		if rec := a.post(rail.FakeName, hint(id)); rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
			t.Errorf("%s: %d %q, want an empty 204", name, rec.Code, rec.Body)
		}
	}
	if got := a.hook.statuses.Load(); got != 0 {
		t.Fatalf("Status called %d times, want 0", got)
	}
	declined := FailureDeclined
	assertState(t, a.svc, settled, Settled, nil)
	assertState(t, a.svc, failed, Failed, &declined)
	assertState(t, a.svc, created.ID, Created, nil)
}

// AC5: a webhook while the rail is still pending changes nothing; once the
// rail has finished, the next one settles the payment, and a later one
// doesn't reach the rail.
func TestWebhook_PendingThenSettled(t *testing.T) {
	t.Parallel()
	a := newWebhookAPI(t, []rail.Step{{Outcome: rail.Pending}})
	id := a.create("k")

	wantCode(t, a.post(rail.FakeName, hint(id)), http.StatusNoContent)
	assertState(t, a.svc, id, Settling, nil)

	a.resolve(id, rail.Settled)
	wantCode(t, a.post(rail.FakeName, hint(id)), http.StatusNoContent)
	assertState(t, a.svc, id, Settled, nil)

	wantCode(t, a.post(rail.FakeName, hint(id)), http.StatusNoContent)
	if got := a.hook.statuses.Load(); got != 2 {
		t.Fatalf("Status called %d times, want 2 (none once settled)", got)
	}
	if got := fakeMoves(t, a.fake, id); got != 1 {
		t.Fatalf("money moved %d times, want 1", got)
	}
}

// ADR-124: nothing in the body but the reference is read. A body claiming a
// decline, for a payment the rail settled, settles it.
func TestWebhook_BodyOutcomeIgnored(t *testing.T) {
	t.Parallel()
	a := newWebhookAPI(t, []rail.Step{{Outcome: rail.Pending}})
	id := a.create("k")
	a.resolve(id, rail.Settled)

	body := `{"ref":"` + rail.Ref(id) + `","outcome":"declined","amount":1,"reason":"insufficient_funds"}`
	wantCode(t, a.post(rail.FakeName, body), http.StatusNoContent)
	assertState(t, a.svc, id, Settled, nil)
}

// ADR-124's amendment: if the rail has no record of the reference, the
// webhook doesn't settle (a forged hint must not make the gateway settle a
// payment the rail never saw). The payment stays settling, and Recover,
// which owns that case, settles it.
func TestWebhook_UnknownRefNeverSettles(t *testing.T) {
	t.Parallel()
	a := newWebhookAPI(t, []rail.Step{{Err: errors.New("connection reset")}})
	id := a.create("k")
	assertState(t, a.svc, id, Settling, nil)

	wantCode(t, a.post(rail.FakeName, hint(id)), http.StatusNoContent)
	assertState(t, a.svc, id, Settling, nil)
	if got := a.hook.settles.Load(); got != 1 {
		t.Fatalf("Settle called %d times, want 1 (create only)", got)
	}
	if got := fakeMoves(t, a.fake, id); got != 0 {
		t.Fatalf("money moved %d times, want 0", got)
	}

	a.svc.recoverGrace = 0
	if _, err := a.svc.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertState(t, a.svc, id, Settled, nil)
}

// ADR-124: a malformed request is turned away before any I/O. The pool is
// closed, so any database query would fail with a 500; every case here gets
// its own 4xx and the rail is never asked. A valid hint then gets the 500,
// which shows the database really is unreachable.
func TestWebhook_RejectsBeforeIO(t *testing.T) {
	t.Parallel()
	a := newWebhookAPI(t, nil)
	a.pool.Close()
	id := uuid.New()

	for name, tc := range map[string]struct {
		rail, body string
		code       int
		problem    string
	}{
		"unknown rail":      {rail.InternalName, hint(id), http.StatusNotFound, "unknown_rail"},
		"empty body":        {rail.FakeName, ``, http.StatusBadRequest, "invalid_ref"},
		"not json":          {rail.FakeName, `ref=pay_x`, http.StatusBadRequest, "invalid_ref"},
		"no ref":            {rail.FakeName, `{}`, http.StatusBadRequest, "invalid_ref"},
		"ref not a string":  {rail.FakeName, `{"ref":42}`, http.StatusBadRequest, "invalid_ref"},
		"wrong prefix":      {rail.FakeName, `{"ref":"payer_` + id.String() + `"}`, http.StatusBadRequest, "invalid_ref"},
		"not a uuid":        {rail.FakeName, `{"ref":"pay_` + strings.Repeat("z", 36) + `"}`, http.StatusBadRequest, "invalid_ref"},
		"over 255":          {rail.FakeName, `{"ref":"pay_` + strings.Repeat("a", 300) + `"}`, http.StatusBadRequest, "invalid_ref"},
		"body over 1 KiB":   {rail.FakeName, `{"ref":"` + rail.Ref(id) + `","pad":"` + strings.Repeat("a", 1<<10) + `"}`, http.StatusRequestEntityTooLarge, "request_too_large"},
		"non-canonical ref": {rail.FakeName, `{"ref":"pay_` + strings.ToUpper(id.String()) + `"}`, http.StatusBadRequest, "invalid_ref"},
	} {
		rec := a.post(tc.rail, tc.body)
		if rec.Code != tc.code || !strings.Contains(rec.Body.String(), `"code":"`+tc.problem+`"`) {
			t.Errorf("%s: %d %s, want %d %s", name, rec.Code, rec.Body, tc.code, tc.problem)
		}
	}
	if got := a.hook.statuses.Load(); got != 0 {
		t.Fatalf("Status called %d times, want 0", got)
	}
	wantCode(t, a.post(rail.FakeName, hint(id)), http.StatusInternalServerError)
}

// A Service with no rail has no registered rail: every webhook is 404.
func TestWebhook_NoRailIsUnknownRail(t *testing.T) {
	t.Parallel()
	svc := New(testutil.Postgres(t))
	rec := httptest.NewRecorder()
	svc.Webhook(slog.New(slog.DiscardHandler)).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/"+rail.InternalName, strings.NewReader(hint(uuid.New()))))
	wantCode(t, rec, http.StatusNotFound)
}

// hangingRail accepts a Status call and, while hang is set, never answers:
// it returns only when the caller's context ends, as a rail that took the
// connection and went quiet would.
type hangingRail struct {
	*hookRail
	hang atomic.Bool
}

func (h *hangingRail) Status(ctx context.Context, ref string) (rail.Result, error) {
	if !h.hang.Load() {
		return h.hookRail.Status(ctx, ref)
	}
	h.statuses.Add(1)
	<-ctx.Done()
	return rail.Result{}, ctx.Err()
}

// The Status bound and its timeout together (Ivan, 2026-10-29). While a
// hung rail holds every slot, the next webhook is shed at once with 503 and
// Retry-After, and counted in WebhookShed. The hung calls give their slots
// back after webhookStatusTimeout, not the router's 5 s deadline, and later
// webhooks are handled again. A budget nothing exercises is one we don't
// have.
func TestWebhook_HungRailCantHoldTheBound(t *testing.T) {
	t.Parallel()
	const bound, timeout = 2, 200 * time.Millisecond
	pool := testutil.Postgres(t)
	fake := rail.NewFake(pool, rail.Step{Outcome: rail.Pending}, rail.Step{Outcome: rail.Pending}, rail.Step{Outcome: rail.Pending})
	hung := &hangingRail{hookRail: &hookRail{Adapter: fake}}
	a := newWebhookAPIOn(t, pool, hung, WithWebhookBound(bound))
	a.fake = fake
	a.svc.webhookStatusTimeout = timeout
	ids := []uuid.UUID{a.create("1"), a.create("2"), a.create("3")}

	hung.hang.Store(true)
	var wg sync.WaitGroup
	elapsed := make([]time.Duration, bound)
	for i := range bound {
		wg.Go(func() {
			start := time.Now()
			if rec := a.post(rail.FakeName, hint(ids[i])); rec.Code != http.StatusNoContent {
				t.Errorf("hung webhook %d = %d, want 204", i, rec.Code)
			}
			elapsed[i] = time.Since(start)
		})
	}
	waitFor(t, func() bool { return hung.statuses.Load() == bound })

	rec := a.post(rail.FakeName, hint(ids[2]))
	wantCode(t, rec, http.StatusServiceUnavailable)
	if rec.Header().Get("Retry-After") == "" || !strings.Contains(rec.Body.String(), `"code":"try_again_later"`) {
		t.Fatalf("shed response = %v %s, want Retry-After and try_again_later", rec.Header(), rec.Body)
	}
	if got := a.svc.WebhookShed(); got != 1 {
		t.Fatalf("WebhookShed = %d, want 1", got)
	}
	if got := hung.statuses.Load(); got != bound {
		t.Fatalf("Status called %d times, want %d (a shed request never asks)", got, bound)
	}

	wg.Wait()
	for i, d := range elapsed {
		// Well under the router's 5 s deadline: the slot is held for the
		// Status timeout, plus the lookup.
		if d < timeout || d > 2*time.Second {
			t.Errorf("hung webhook %d took %v, want about %v", i, d, timeout)
		}
	}
	assertState(t, a.svc, ids[0], Settling, nil)

	hung.hang.Store(false)
	for _, id := range ids {
		a.resolve(id, rail.Settled)
		wantCode(t, a.post(rail.FakeName, hint(id)), http.StatusNoContent)
		assertState(t, a.svc, id, Settled, nil)
	}
	if got := a.svc.WebhookShed(); got != 1 {
		t.Fatalf("WebhookShed = %d after the rail recovered, want still 1", got)
	}
}

// waitFor polls cond until it holds, failing the test after 5 s.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5 s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// AC8: nothing a webhook caller sends reaches the log or a response, in a
// malformed reference or in members the handler ignores. The payment id is
// logged as text.
func TestWebhook_LogsNoBodyContent(t *testing.T) {
	t.Parallel()
	const secret = "sk_live_webhook_4242"
	a := newWebhookAPI(t, []rail.Step{{Outcome: rail.Pending}})
	id := a.create("k")
	a.resolve(id, rail.Settled)

	bad := a.post(rail.FakeName, `{"ref":"pay_`+secret+`"}`)
	wantCode(t, bad, http.StatusBadRequest)
	wantCode(t, a.post(rail.FakeName, `{"ref":"`+rail.Ref(id)+`","signature":"`+secret+`"}`), http.StatusNoContent)

	out := a.log.String()
	if strings.Contains(out, secret) || strings.Contains(bad.Body.String(), secret) {
		t.Fatalf("secret leaked:\nlog: %s\nbody: %s", out, bad.Body)
	}
	if want := `"msg":"payment settled","payment_id":"` + id.String() + `"`; !strings.Contains(out, want) {
		t.Fatalf("log missing %s:\n%s", want, out)
	}
}

// ADR-124's amendment: the Status bound defaults to 8 and is configurable;
// a bound below 1 would shed every webhook, so New refuses it.
func TestWebhook_BoundDefaultAndOption(t *testing.T) {
	t.Parallel()
	if got := cap(New(nil).webhookSlots); got != 8 {
		t.Fatalf("default bound = %d, want 8", got)
	}
	if got := cap(New(nil, WithWebhookBound(3)).webhookSlots); got != 3 {
		t.Fatalf("WithWebhookBound(3) gave %d slots, want 3", got)
	}
	for _, n := range []int{0, -1} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("WithWebhookBound(%d) did not panic", n)
				}
			}()
			New(nil, WithWebhookBound(n))
		}()
	}
}
