//go:build integration

package billing_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/internal/enginetest"
	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/internal/postgrestest"
	"github.com/zsoftly/zs-core/pkg/audit"
	"github.com/zsoftly/zs-core/pkg/billing"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idempotency"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type env struct {
	t     *testing.T
	db    *postgrestest.DB
	fake  *enginetest.Fake
	eng   tenant.Engines
	brand enginetest.Brand
	svc   *billing.Service
	clock *clock
	owner enginetest.Person
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db := postgrestest.New(t)
	fake := enginetest.New(t)
	e := &env{t: t, db: db, fake: fake, brand: fake.InsertBrand(t, db.Pool, "zs-acme"),
		eng:   tenant.Engines{Keycloak: fake.Keycloak(t), KillBill: fake.KillBill(t), Secrets: fake.Secrets()},
		clock: &clock{now: time.Now().UTC().Truncate(time.Second)}}
	e.svc = &billing.Service{DB: db.Pool, KillBill: fake.KillBill(t), Secrets: fake.Secrets(),
		WebhookSecret: []byte(strings.Repeat("w", 32)), Now: e.clock.Now}
	e.owner = enginetest.InsertPerson(t, db.Pool, e.brand, "owner@example.test")
	return e
}

func (e *env) tenant(name string) tenant.Tenant {
	e.t.Helper()
	tn, err := tenant.CreateOrganization(context.Background(), e.db.Pool, e.eng, tenant.CreateRequest{
		Brand: e.brand.ID, Creator: e.owner.Identity.ID, Name: name, IdempotencyKey: "key-" + idgen.MustNewV7().String(),
		BillingCountry: "CA", Actor: e.actor(), CorrelationID: "test",
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return tn
}

func (e *env) actor() events.Actor {
	return events.Actor{Type: events.ActorUser, ID: e.owner.Identity.PublicID()}
}

func (e *env) call() billing.Call {
	return billing.Call{Actor: e.actor(), CorrelationID: "corr-" + idgen.MustNewV7().String()}
}

func (e *env) staff(reason string) billing.Call {
	return billing.Call{Actor: events.Actor{Type: events.ActorStaff, ID: "sub:staff-1"}, Reason: reason, CorrelationID: "corr-staff"}
}

func (e *env) code(c billing.NewCreditCode) billing.CreditCode {
	e.t.Helper()
	var out billing.CreditCode
	err := pgx.BeginFunc(context.Background(), e.db.Pool, func(tx pgx.Tx) error {
		var err error
		out, err = billing.AddCreditCode(context.Background(), tx, e.brand.ID, c, events.Actor{Type: events.ActorService, ID: "zs-core-cli:test"}, "test")
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func codeOf(err error) string {
	var e *zserrors.Error
	if err == nil {
		return "ok"
	}
	if !errors.As(err, &e) {
		return "untyped: " + err.Error()
	}
	return zserrors.Lookup(e.Code).Name
}

func intp(n int) *int { return &n }

func operator() events.Actor { return events.Actor{Type: events.ActorService, ID: "zs-core-cli:test"} }

func TestCreditCodes(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	welcome := e.code(billing.NewCreditCode{Code: "welcome10", AmountMinor: 1000, Currency: "CAD", MaxRedemptions: intp(2), Reason: "launch", CreatedBy: "ops"})
	if welcome.Hint != "WELC" || welcome.PerTenantLimit != 1 || welcome.Status != billing.CodeActive {
		t.Fatalf("code = %+v", welcome)
	}
	err := pgx.BeginFunc(ctx, e.db.Pool, func(tx pgx.Tx) error {
		_, err := billing.AddCreditCode(ctx, tx, e.brand.ID, billing.NewCreditCode{Code: "WELCOME10", AmountMinor: 5, Currency: "CAD", Reason: "x", CreatedBy: "ops"}, operator(), "t")
		return err
	})
	if codeOf(err) != "conflict" {
		t.Fatalf("same code twice: %v", err)
	}
	err = pgx.BeginFunc(ctx, e.db.Pool, func(tx pgx.Tx) error {
		_, err := billing.AddCreditCode(ctx, tx, e.brand.ID, billing.NewCreditCode{Code: "EUROS", AmountMinor: 5, Currency: "EUR", Reason: "x", CreatedBy: "ops"}, operator(), "t")
		return err
	})
	if codeOf(err) != "validation_failed" {
		t.Fatalf("currency outside the brand: %v", err)
	}
	var stored []byte
	if err := e.db.Pool.QueryRow(ctx, `SELECT code_digest FROM credit_codes WHERE id IS NOT NULL LIMIT 1`).Scan(&stored); err != nil || strings.Contains(string(stored), "WELCOME") {
		t.Fatalf("the code is stored in clear: %v", err)
	}
	a, b, c := e.tenant("Tenant A"), e.tenant("Tenant B"), e.tenant("Tenant C")

	first, created, err := e.svc.Redeem(ctx, a.Scope(), e.call(), " Welcome10 ")
	if err != nil || !created || first.AmountMinor != 1000 || first.Source != billing.SourceCreditCode || first.KillBillCreditID == "" {
		t.Fatalf("redeem = %+v %v %v", first, created, err)
	}
	again, created, err := e.svc.Redeem(ctx, a.Scope(), e.call(), "WELCOME10")
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("second redeem by the same tenant = %+v %v %v", again, created, err)
	}
	if n := e.fake.CreditItems("zs-acme", a.KillBillAccountID); n != 1 {
		t.Fatalf("Kill Bill credits for tenant A = %d", n)
	}
	// The code is the reason of the Kill Bill credit.
	var reasons []string
	for _, w := range e.fake.Writes() {
		if strings.HasPrefix(w.Path, "/1.0/kb/credits") {
			reasons = append(reasons, w.Reason+"|"+w.CreatedBy)
		}
	}
	if len(reasons) != 1 || reasons[0] != "WELCOME10|user:"+e.owner.Identity.PublicID() {
		t.Fatalf("credit headers = %v", reasons)
	}
	if _, created, err := e.svc.Redeem(ctx, b.Scope(), e.call(), "WELCOME10"); err != nil || !created {
		t.Fatalf("second tenant: %v %v", created, err)
	}
	if _, _, err := e.svc.Redeem(ctx, c.Scope(), e.call(), "WELCOME10"); codeOf(err) != "credit_code_exhausted" {
		t.Fatalf("past the cap: %v", err)
	}
	sum, err := e.svc.Summary(ctx, a.Scope())
	if err != nil || sum.CreditMinor != 1000 || sum.BalanceMinor != -1000 || sum.Currency != "CAD" || sum.OnHold || sum.PaymentPlugin != enginetest.ExternalPlugin {
		t.Fatalf("summary = %+v %v", sum, err)
	}

	for name, setup := range map[string]func() string{
		"unknown":   func() string { return "NOPE1234" },
		"malformed": func() string { return "x" },
		"disabled": func() string {
			code := e.code(billing.NewCreditCode{Code: "OFF-CODE", AmountMinor: 1, Currency: "CAD", Reason: "x", CreatedBy: "ops"})
			if err := pgx.BeginFunc(ctx, e.db.Pool, func(tx pgx.Tx) error {
				_, err := billing.DisableCreditCode(ctx, tx, e.brand.ID, code.ID, "retired", operator(), "t")
				return err
			}); err != nil {
				t.Fatal(err)
			}
			return "OFF-CODE"
		},
		"expired": func() string {
			e.code(billing.NewCreditCode{Code: "OLD-CODE", AmountMinor: 1, Currency: "CAD", Reason: "x", CreatedBy: "ops"})
			if _, err := e.db.Pool.Exec(ctx, `UPDATE credit_codes SET expires_at = now() - interval '1 minute' WHERE hint = 'OLD-'`); err != nil {
				t.Fatal(err)
			}
			return "OLD-CODE"
		},
		"other currency": func() string {
			e.code(billing.NewCreditCode{Code: "USD-CODE", AmountMinor: 1, Currency: "USD", Reason: "x", CreatedBy: "ops"})
			return "USD-CODE"
		},
		"another brand": func() string {
			other := e.fake.InsertBrand(t, e.db.Pool, "zs-other")
			if err := pgx.BeginFunc(ctx, e.db.Pool, func(tx pgx.Tx) error {
				_, err := billing.AddCreditCode(ctx, tx, other.ID, billing.NewCreditCode{Code: "ONLY-OTHER", AmountMinor: 1, Currency: "CAD", Reason: "x", CreatedBy: "ops"}, operator(), "t")
				return err
			}); err != nil {
				t.Fatal(err)
			}
			return "ONLY-OTHER"
		},
	} {
		if _, _, err := e.svc.Redeem(ctx, c.Scope(), e.call(), setup()); codeOf(err) != "credit_code_invalid" {
			t.Errorf("%s: %v", name, err)
		}
	}
	codes, err := billing.ListCreditCodes(ctx, e.db.Pool, e.brand.ID)
	if err != nil || len(codes) != 4 {
		t.Fatalf("codes = %d %v", len(codes), err)
	}
	page, err := audit.Query(ctx, e.db.Pool, a.Scope(), audit.Filter{Action: "billing.credit.redeemed"})
	if err != nil || len(page.Events) != 1 {
		t.Fatalf("audit = %+v %v", page, err)
	}
}

// A redemption whose Kill Bill call failed stays pending and the retry
// resumes it. When Kill Bill had written the credit, the retry finds it
// instead of writing a second one.
func TestRedeemResumesAfterKillBillFails(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.code(billing.NewCreditCode{Code: "RETRY-ME", AmountMinor: 700, Currency: "CAD", Reason: "x", CreatedBy: "ops"})
	a := e.tenant("Tenant A")
	e.fake.SetDown(false, true)
	if _, _, err := e.svc.Redeem(ctx, a.Scope(), e.call(), "RETRY-ME"); codeOf(err) != "billing_unavailable" {
		t.Fatalf("Kill Bill down: %v", err)
	}
	e.fake.SetDown(false, false)
	var id idgen.UUID
	if err := e.db.Pool.QueryRow(ctx, `SELECT id FROM credit_code_redemptions WHERE status = 'pending'`).Scan(&id); err != nil {
		t.Fatalf("pending redemption: %v", err)
	}
	// Kill Bill wrote the credit but its answer was lost.
	creds := killbill.TenantCredentials{APIKey: "zs-acme", APISecret: "kb-secret-zs-acme"}
	if _, err := e.fake.KillBill(t).CreateCredit(ctx, creds, killbill.CallContext{CreatedBy: "test"}, killbill.InvoiceItem{
		AccountID: a.KillBillAccountID, Amount: "7.00", Currency: "CAD", Description: "zs-core credit " + idgen.Format(idgen.Credit, id)}); err != nil {
		t.Fatal(err)
	}
	c, created, err := e.svc.Redeem(ctx, a.Scope(), e.call(), "RETRY-ME")
	if err != nil || !created || c.ID != idgen.Format(idgen.Credit, id) {
		t.Fatalf("resume = %+v %v %v", c, created, err)
	}
	if n := e.fake.CreditItems("zs-acme", a.KillBillAccountID); n != 1 {
		t.Fatalf("Kill Bill credits = %d, want 1", n)
	}
}

// A pending attempt created before original credit targets were recorded is
// not safe to resume by issuing another Kill Bill credit.
func TestCreditRecoveryDoesNotPostForLegacyPendingAttempt(t *testing.T) {
	ctx := context.Background()
	creditWrites := func(e *env) int {
		n := 0
		for _, write := range e.fake.Writes() {
			if write.Method == http.MethodPost && strings.HasPrefix(write.Path, "/1.0/kb/credits") {
				n++
			}
		}
		return n
	}

	t.Run("redeem", func(t *testing.T) {
		e := newEnv(t)
		code := e.code(billing.NewCreditCode{Code: "LEGACY-REDEEM", AmountMinor: 700, Currency: "CAD", Reason: "x", CreatedBy: "ops"})
		a := e.tenant("Tenant A")
		codeID, err := idgen.ParseAs(idgen.CreditCode, code.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.db.Pool.Exec(ctx, `INSERT INTO credit_code_redemptions
    (id, brand_id, tenant_id, credit_code_id, amount_minor, currency, actor_type, actor_id, correlation_id)
VALUES ($1, $2, $3, $4, 700, 'CAD', 'user', $5, 'legacy')`, idgen.MustNewV7(), a.BrandID, a.ID, codeID, e.owner.Identity.PublicID()); err != nil {
			t.Fatal(err)
		}
		before := creditWrites(e)
		_, created, err := e.svc.Redeem(ctx, a.Scope(), e.call(), "LEGACY-REDEEM")
		if created || err == nil || strings.HasPrefix(codeOf(err), "untyped:") {
			t.Fatalf("legacy retry = created %v, err %v", created, err)
		}
		if after := creditWrites(e); after != before {
			t.Fatalf("legacy retry credit POSTs = %d, want %d", after, before)
		}
		var status string
		if err := e.db.Pool.QueryRow(ctx, `SELECT status FROM credit_code_redemptions WHERE tenant_id = $1`, a.ID).Scan(&status); err != nil || status != "pending" {
			t.Fatalf("legacy redemption status = %q, %v", status, err)
		}
	})

	t.Run("grant", func(t *testing.T) {
		e := newEnv(t)
		a := e.tenant("Tenant A")
		req := billing.StaffCredit{AmountMinor: 700, Currency: "CAD", IdempotencyKey: "legacy-grant"}
		payload, err := json.Marshal(map[string]any{"amount_minor": req.AmountMinor, "currency": req.Currency, "reason": "goodwill", "expires_at": req.ExpiresAt})
		if err != nil {
			t.Fatal(err)
		}
		fingerprint, err := idempotency.Fingerprint("billing.credit.grant", payload)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.db.Pool.Exec(ctx, `INSERT INTO staff_credits
    (id, brand_id, tenant_id, idempotency_key, fingerprint, amount_minor, currency, reason, actor_type, actor_id, correlation_id)
VALUES ($1, $2, $3, $4, $5, 700, 'CAD', 'goodwill', 'staff', 'sub:staff-1', 'legacy')`,
			idgen.MustNewV7(), a.BrandID, a.ID, req.IdempotencyKey, fingerprint); err != nil {
			t.Fatal(err)
		}
		before := creditWrites(e)
		_, created, err := e.svc.GrantCredit(ctx, a.Scope(), e.staff("goodwill"), req)
		if created || err == nil || strings.HasPrefix(codeOf(err), "untyped:") {
			t.Fatalf("legacy retry = created %v, err %v", created, err)
		}
		if after := creditWrites(e); after != before {
			t.Fatalf("legacy retry credit POSTs = %d, want %d", after, before)
		}
		var status string
		if err := e.db.Pool.QueryRow(ctx, `SELECT status FROM staff_credits WHERE tenant_id = $1`, a.ID).Scan(&status); err != nil || status != "pending" {
			t.Fatalf("legacy staff credit status = %q, %v", status, err)
		}
	})
}

func TestFirstCreditAttemptStoresRecoveryTarget(t *testing.T) {
	ctx := context.Background()
	target := func(t *testing.T, e *env, accountID, table string, where string, args ...any) {
		t.Helper()
		var rowID idgen.UUID
		var endpoint, kbTenant, account, currency, description *string
		var amount *int64
		query := `SELECT id, original_endpoint_kind, original_killbill_tenant_id::text, original_killbill_account_id::text,
    original_currency, original_amount_minor, original_description FROM ` + table + ` WHERE ` + where
		if err := e.db.Pool.QueryRow(ctx, query, args...).Scan(&rowID, &endpoint, &kbTenant, &account, &currency, &amount, &description); err != nil {
			t.Fatal(err)
		}
		if endpoint == nil || kbTenant == nil || account == nil || currency == nil || amount == nil || description == nil {
			t.Fatalf("recovery target has NULL field: endpoint=%v tenant=%v account=%v currency=%v amount=%v description=%v", endpoint, kbTenant, account, currency, amount, description)
		}
		wantEndpoint := "redeem"
		if table == "staff_credits" {
			wantEndpoint = "grant"
		}
		wantDescription := "zs-core credit " + idgen.Format(idgen.Credit, rowID)
		if *endpoint != wantEndpoint || *kbTenant != e.brand.ID.String() || *account != accountID || *currency != "CAD" || *amount != 700 || *description != wantDescription {
			t.Fatalf("recovery target = endpoint=%q tenant=%q account=%q currency=%q amount=%d description=%q", *endpoint, *kbTenant, *account, *currency, *amount, *description)
		}
	}

	t.Run("redeem", func(t *testing.T) {
		e := newEnv(t)
		e.code(billing.NewCreditCode{Code: "TARGET-REDEEM", AmountMinor: 700, Currency: "CAD", Reason: "x", CreatedBy: "ops"})
		a := e.tenant("Tenant A")
		e.fake.SetDown(false, true)
		if _, _, err := e.svc.Redeem(ctx, a.Scope(), e.call(), "TARGET-REDEEM"); codeOf(err) != "billing_unavailable" {
			t.Fatalf("first redeem = %v", err)
		}
		target(t, e, a.KillBillAccountID, "credit_code_redemptions", "tenant_id = $1", a.ID)
	})

	t.Run("grant", func(t *testing.T) {
		e := newEnv(t)
		a := e.tenant("Tenant A")
		e.fake.SetDown(false, true)
		if _, _, err := e.svc.GrantCredit(ctx, a.Scope(), e.staff("goodwill"), billing.StaffCredit{AmountMinor: 700, Currency: "CAD", IdempotencyKey: "target-grant"}); codeOf(err) != "billing_unavailable" {
			t.Fatalf("first grant = %v", err)
		}
		target(t, e, a.KillBillAccountID, "staff_credits", "tenant_id = $1", a.ID)
	})
}

func TestStaffCreditsAndExpirySweeper(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.tenant("Tenant A")
	sc := a.Scope()
	in := func(d time.Duration) *time.Time { x := e.clock.Now().Add(d); return &x }

	if _, _, err := e.svc.GrantCredit(ctx, sc, e.staff(""), billing.StaffCredit{AmountMinor: 1, Currency: "CAD", IdempotencyKey: "grant-key-0"}); codeOf(err) != "reason_required" {
		t.Fatalf("no reason: %v", err)
	}
	if _, _, err := e.svc.GrantCredit(ctx, sc, e.staff("x"), billing.StaffCredit{AmountMinor: 1, Currency: "USD", IdempotencyKey: "grant-key-0"}); codeOf(err) != "validation_failed" {
		t.Fatalf("another currency: %v", err)
	}
	expiring, created, err := e.svc.GrantCredit(ctx, sc, e.staff("goodwill"), billing.StaffCredit{AmountMinor: 1000, Currency: "CAD", ExpiresAt: in(time.Hour), IdempotencyKey: "grant-key-1"})
	if err != nil || !created || expiring.Source != billing.SourceStaff || expiring.ExpiresAt == nil {
		t.Fatalf("grant = %+v %v %v", expiring, created, err)
	}
	replay, created, err := e.svc.GrantCredit(ctx, sc, e.staff("goodwill"), billing.StaffCredit{AmountMinor: 1000, Currency: "CAD", ExpiresAt: expiring.ExpiresAt, IdempotencyKey: "grant-key-1"})
	if err != nil || created || replay.ID != expiring.ID {
		t.Fatalf("replay = %+v %v %v", replay, created, err)
	}
	if _, _, err := e.svc.GrantCredit(ctx, sc, e.staff("goodwill"), billing.StaffCredit{AmountMinor: 999, Currency: "CAD", IdempotencyKey: "grant-key-1"}); codeOf(err) != "idempotency_key_conflict" {
		t.Fatalf("same key, other amount: %v", err)
	}
	e.clock.advance(time.Minute)
	if _, _, err := e.svc.GrantCredit(ctx, sc, e.staff("permanent"), billing.StaffCredit{AmountMinor: 500, Currency: "CAD", IdempotencyKey: "grant-key-2"}); err != nil {
		t.Fatal(err)
	}
	// The tenant uses 3.00 of its 15.00 of credit.
	creds := killbill.TenantCredentials{APIKey: "zs-acme", APISecret: "kb-secret-zs-acme"}
	if _, err := e.fake.KillBill(t).CreateExternalCharge(ctx, creds, killbill.CallContext{CreatedBy: "test"}, killbill.InvoiceItem{AccountID: a.KillBillAccountID, Amount: "3.00", Currency: "CAD"}); err != nil {
		t.Fatal(err)
	}
	sweeper := &billing.CreditSweeper{Service: e.svc}
	if r, err := sweeper.SweepOnce(ctx); err != nil || r.Expired != 0 {
		t.Fatalf("sweep before expiry = %+v %v", r, err)
	}
	e.clock.advance(2 * time.Hour)
	r, err := sweeper.SweepOnce(ctx)
	// Oldest first: the 3.00 came out of the expiring credit, the newer
	// permanent 5.00 is untouched, so 7.00 is reversed.
	if err != nil || r.Expired != 1 || r.ReversedMinor != 700 || r.Errors != 0 {
		t.Fatalf("sweep = %+v %v", r, err)
	}
	if got := e.fake.Credit("zs-acme", a.KillBillAccountID); got != 500 {
		t.Fatalf("account credit after expiry = %d, want 500", got)
	}
	if r, err := sweeper.SweepOnce(ctx); err != nil || r.Expired != 0 {
		t.Fatalf("second sweep = %+v %v", r, err)
	}
	var reversed int64
	var expiredAt *time.Time
	if err := e.db.Pool.QueryRow(ctx, `SELECT reversed_minor, expired_at FROM staff_credits WHERE idempotency_key = 'grant-key-1'`).Scan(&reversed, &expiredAt); err != nil || reversed != 700 || expiredAt == nil {
		t.Fatalf("row = %d %v %v", reversed, expiredAt, err)
	}
	// A credit spent in full reverses nothing.
	if _, _, err := e.svc.GrantCredit(ctx, sc, e.staff("short"), billing.StaffCredit{AmountMinor: 300, Currency: "CAD", ExpiresAt: in(time.Hour), IdempotencyKey: "grant-key-3"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fake.KillBill(t).CreateExternalCharge(ctx, creds, killbill.CallContext{CreatedBy: "test"}, killbill.InvoiceItem{AccountID: a.KillBillAccountID, Amount: "8.00", Currency: "CAD"}); err != nil {
		t.Fatal(err)
	}
	e.clock.advance(2 * time.Hour)
	if r, err := sweeper.SweepOnce(ctx); err != nil || r.Expired != 1 || r.ReversedMinor != 0 {
		t.Fatalf("spent credit sweep = %+v %v", r, err)
	}
	page, err := audit.Query(ctx, e.db.Pool, sc, audit.Filter{Action: "billing.credit.expired"})
	if err != nil || len(page.Events) != 1 {
		t.Fatalf("expiry audit = %d %v", len(page.Events), err)
	}
	page, err = audit.Query(ctx, e.db.Pool, sc, audit.Filter{Action: "billing.credit.expired_nothing_to_reverse"})
	if err != nil || len(page.Events) != 1 {
		t.Fatalf("expiry audit with nothing to reverse = %d %v", len(page.Events), err)
	}
	page, err = audit.Query(ctx, e.db.Pool, sc, audit.Filter{Action: "billing.credit.granted"})
	if err != nil || len(page.Events) != 3 || page.Events[0].Reason == "" {
		t.Fatalf("grant audit = %+v %v", page.Events, err)
	}
}

func TestSubscriptionsInvoicesAndEntitlements(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a, b := e.tenant("Tenant A"), e.tenant("Tenant B")
	sc := a.Scope()

	if ent := e.svc.CheckEntitlement(ctx, sc, enginetest.ProductTest); ent.Allowed || ent.Reason != billing.ReasonNoActiveSubscription {
		t.Fatalf("before subscribing = %+v", ent)
	}
	sub, replayed, err := e.svc.CreateSubscription(ctx, sc, e.call(), enginetest.PlanUsage, "sub-key-1")
	if err != nil || replayed || sub.Plan != enginetest.PlanUsage || sub.Phase != "TRIAL" || sub.State != "ACTIVE" {
		t.Fatalf("create = %+v %v %v", sub, replayed, err)
	}
	again, replayed, err := e.svc.CreateSubscription(ctx, sc, e.call(), enginetest.PlanUsage, "sub-key-1")
	if err != nil || !replayed || again.ID != sub.ID || e.fake.Subscriptions("zs-acme") != 1 {
		t.Fatalf("replay = %+v %v %v", again, replayed, err)
	}
	if _, _, err := e.svc.CreateSubscription(ctx, sc, e.call(), enginetest.PlanBasic, "sub-key-1"); codeOf(err) != "idempotency_key_conflict" {
		t.Fatalf("same key, other plan: %v", err)
	}
	if _, _, err := e.svc.CreateSubscription(ctx, sc, e.call(), "no-such-plan", "sub-key-2"); codeOf(err) != "billing_rejected" {
		t.Fatalf("unknown plan: %v", err)
	}
	for _, f := range []string{enginetest.ProductTest, enginetest.UnitAPICall} {
		if ent := e.svc.CheckEntitlement(ctx, sc, f); !ent.Allowed || ent.Plan != enginetest.PlanUsage || ent.Limit != nil {
			t.Fatalf("%s = %+v", f, ent)
		}
	}
	if ent := e.svc.CheckEntitlement(ctx, sc, "vm:create"); ent.Allowed || ent.Reason != billing.ReasonNotInPlan {
		t.Fatalf("other feature = %+v", ent)
	}
	if ent := e.svc.CheckEntitlement(ctx, b.Scope(), enginetest.ProductTest); ent.Allowed {
		t.Fatal("tenant B is entitled through tenant A's subscription")
	}
	list, err := e.svc.ListSubscriptions(ctx, sc)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v %v", list, err)
	}
	if _, err := e.svc.ChangePlan(ctx, sc, e.call(), sub.ID, enginetest.PlanBasic, "sometime"); codeOf(err) != "validation_failed" {
		t.Fatalf("bad policy: %v", err)
	}
	if _, err := e.svc.ChangePlan(ctx, b.Scope(), e.call(), sub.ID, enginetest.PlanBasic, billing.PolicyImmediate); codeOf(err) != "not_found" {
		t.Fatalf("change from another tenant: %v", err)
	}
	if _, err := e.svc.ChangePlan(ctx, sc, e.call(), "../accounts", enginetest.PlanBasic, billing.PolicyImmediate); codeOf(err) != "not_found" {
		t.Fatalf("not an ID: %v", err)
	}
	changed, err := e.svc.ChangePlan(ctx, sc, e.call(), sub.ID, enginetest.PlanBasic, billing.PolicyImmediate)
	if err != nil || changed.Plan != enginetest.PlanBasic {
		t.Fatalf("change = %+v %v", changed, err)
	}
	invoices, next, err := e.svc.ListInvoices(ctx, sc, tenant.Page{})
	if err != nil || len(invoices) != 1 || next != "" || invoices[0].Items != nil {
		t.Fatalf("invoices = %+v %v", invoices, err)
	}
	inv, err := e.svc.GetInvoice(ctx, sc, invoices[0].ID)
	if err != nil || len(inv.Items) != 1 || inv.Items[0].Type != "FIXED" {
		t.Fatalf("invoice = %+v %v", inv, err)
	}
	if _, err := e.svc.GetInvoice(ctx, b.Scope(), invoices[0].ID); codeOf(err) != "not_found" {
		t.Fatalf("invoice from another tenant: %v", err)
	}
	if _, err := e.svc.InvoicePDF(ctx, sc, invoices[0].ID); codeOf(err) != "invoice_pdf_unavailable" {
		t.Fatalf("pdf: %v", err)
	}
	if _, err := e.svc.InvoicePDF(ctx, b.Scope(), invoices[0].ID); codeOf(err) != "not_found" {
		t.Fatalf("pdf from another tenant: %v", err)
	}

	// Holds. An overdue state that blocks changes refuses new
	// subscriptions and keeps entitlement; one that disables entitlement
	// stops both.
	e.fake.SetOverdue("zs-acme", a.KillBillAccountID, killbill.OverdueState{Name: "WARNING", IsBlockChanges: true})
	if _, _, err := e.svc.CreateSubscription(ctx, sc, e.call(), enginetest.PlanUsage, "sub-key-3"); codeOf(err) != "wallet_on_hold" {
		t.Fatalf("subscribe while changes are blocked: %v", err)
	}
	if ent := e.svc.CheckEntitlement(ctx, sc, "core-basic"); !ent.Allowed {
		t.Fatalf("entitlement while changes are blocked = %+v", ent)
	}
	e.fake.SetOverdue("zs-acme", a.KillBillAccountID, killbill.OverdueState{Name: "BLOCKED", IsBlockChanges: true, IsDisableEntitlementAndChangesBlocked: true})
	if ent := e.svc.CheckEntitlement(ctx, sc, "core-basic"); ent.Allowed || ent.Reason != billing.ReasonWalletOnHold {
		t.Fatalf("entitlement while disabled = %+v", ent)
	}
	var cached string
	if err := e.db.Pool.QueryRow(ctx, `SELECT overdue_state FROM tenant_billing_standing WHERE tenant_id = $1`, a.ID).Scan(&cached); err != nil || cached != "BLOCKED" {
		t.Fatalf("standing cache = %q %v", cached, err)
	}
	e.fake.SetOverdue("zs-acme", a.KillBillAccountID, killbill.OverdueState{Name: "__KILLBILL__CLEAR__OVERDUE_STATE__", IsClearState: true})

	// Suspension blocks the Kill Bill account: entitlement stops.
	operatorActor := events.Actor{Type: events.ActorService, ID: "zs-core-cli:test"}
	if _, err := tenant.Suspend(ctx, e.db.Pool, e.eng, sc, operatorActor, "test", "test"); err != nil {
		t.Fatal(err)
	}
	if ent := e.svc.CheckEntitlement(ctx, sc, "core-basic"); ent.Allowed || ent.Reason != billing.ReasonWalletOnHold {
		t.Fatalf("entitlement while suspended = %+v", ent)
	}
	sum, err := e.svc.Summary(ctx, sc)
	if err != nil || !sum.OnHold || strings.Join(sum.HoldReasons, ",") != "tenant_suspended,account_blocked" {
		t.Fatalf("summary while suspended = %+v %v", sum, err)
	}
	if _, err := tenant.Reactivate(ctx, e.db.Pool, e.eng, sc, operatorActor, "test", "test"); err != nil {
		t.Fatal(err)
	}
	if ent := e.svc.CheckEntitlement(ctx, sc, "core-basic"); !ent.Allowed {
		t.Fatalf("entitlement after reactivation = %+v", ent)
	}

	// Kill Bill down: every call is billing_unavailable and entitlement
	// fails closed.
	e.fake.SetDown(false, true)
	if ent := e.svc.CheckEntitlement(ctx, sc, "core-basic"); ent.Allowed || ent.Reason != billing.ReasonBillingUnavailable {
		t.Fatalf("entitlement with Kill Bill down = %+v", ent)
	}
	for name, call := range map[string]func() error{
		"summary":       func() error { _, err := e.svc.Summary(ctx, sc); return err },
		"subscriptions": func() error { _, err := e.svc.ListSubscriptions(ctx, sc); return err },
		"invoices":      func() error { _, _, err := e.svc.ListInvoices(ctx, sc, tenant.Page{}); return err },
		"payments":      func() error { _, _, err := e.svc.ListPayments(ctx, sc, tenant.Page{}); return err },
		"methods":       func() error { _, err := e.svc.ListPaymentMethods(ctx, sc); return err },
		"catalogue":     func() error { _, err := e.svc.Catalogue(ctx, e.brand.ID); return err },
		"subscribe": func() error {
			_, _, err := e.svc.CreateSubscription(ctx, sc, e.call(), enginetest.PlanUsage, "sub-key-4")
			return err
		},
	} {
		if err := call(); codeOf(err) != "billing_unavailable" {
			t.Errorf("%s with Kill Bill down: %v", name, err)
		}
	}
	e.fake.SetDown(false, false)

	if _, err := e.svc.CancelSubscription(ctx, sc, e.call(), sub.ID, billing.PolicyImmediate); err != nil {
		t.Fatal(err)
	}
	if ent := e.svc.CheckEntitlement(ctx, sc, "core-basic"); ent.Allowed || ent.Reason != billing.ReasonNoActiveSubscription {
		t.Fatalf("after cancellation = %+v", ent)
	}
	// A closed tenant has no billing surface.
	if _, err := tenant.Close(ctx, e.db.Pool, e.eng, sc, operatorActor, "test", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Summary(ctx, sc); codeOf(err) != "tenant_not_found" {
		t.Fatalf("summary of a closed tenant: %v", err)
	}
	if ent := e.svc.CheckEntitlement(ctx, sc, "core-basic"); ent.Allowed || ent.Reason != billing.ReasonTenantNotFound {
		t.Fatalf("entitlement of a closed tenant = %+v", ent)
	}
	// Every write carried the caller and the correlation ID.
	for _, w := range e.fake.Writes() {
		if strings.HasPrefix(w.Path, "/1.0/kb/subscriptions") && (w.CreatedBy != "user:"+e.owner.Identity.PublicID() || !strings.HasPrefix(w.Comment, "corr-")) {
			t.Fatalf("subscription write headers = %+v", w)
		}
	}
}

func TestPaymentsRefundsAndPaymentMethods(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a, b := e.tenant("Tenant A"), e.tenant("Tenant B")
	sc := a.Scope()
	pay := e.fake.AddPurchase("zs-acme", a.KillBillAccountID, 2000, "CAD")
	e.fake.AddPurchase("zs-acme", a.KillBillAccountID, 100, "CAD")
	list, next, err := e.svc.ListPayments(ctx, sc, tenant.Page{Limit: 1})
	if err != nil || len(list) != 1 || next == "" || list[0].Number != "2" {
		t.Fatalf("payments = %+v %q %v", list, next, err)
	}
	if _, _, err := e.svc.Refund(ctx, sc, e.staff(""), pay, 500, "refund-key-1"); codeOf(err) != "reason_required" {
		t.Fatalf("no reason: %v", err)
	}
	p, replayed, err := e.svc.Refund(ctx, sc, e.staff("damaged"), pay, 500, "refund-key-1")
	if err != nil || replayed || p.RefundedMinor != 500 || p.PurchasedMinor != 2000 {
		t.Fatalf("refund = %+v %v %v", p, replayed, err)
	}
	p, replayed, err = e.svc.Refund(ctx, sc, e.staff("damaged"), pay, 500, "refund-key-1")
	if err != nil || !replayed || p.RefundedMinor != 500 {
		t.Fatalf("replay = %+v %v %v", p, replayed, err)
	}
	if _, _, err := e.svc.Refund(ctx, b.Scope(), e.staff("x"), pay, 1, "refund-key-2"); codeOf(err) != "not_found" {
		t.Fatalf("refund from another tenant: %v", err)
	}
	if _, _, err := e.svc.Refund(ctx, sc, e.staff("x"), pay, 999999, "refund-key-3"); codeOf(err) != "billing_rejected" || !strings.Contains(err.Error(), "refund_exceeds_payment") {
		t.Fatalf("refund above the payment: %v", err)
	}

	added, replayed, err := e.svc.AddPaymentMethod(ctx, sc, e.call(), billing.NewPaymentMethod{IdempotencyKey: "pm-key-1", SetDefault: true,
		PluginProperties: []billing.Property{{Key: "note", Value: "x"}}})
	if err != nil || replayed || added.PaymentMethod == nil || added.PaymentMethod.PluginName != enginetest.ExternalPlugin || !added.PaymentMethod.IsDefault {
		t.Fatalf("add = %+v %v %v", added, replayed, err)
	}
	again, replayed, err := e.svc.AddPaymentMethod(ctx, sc, e.call(), billing.NewPaymentMethod{IdempotencyKey: "pm-key-1", SetDefault: true,
		PluginProperties: []billing.Property{{Key: "note", Value: "x"}}})
	if err != nil || !replayed || again.PaymentMethod.ID != added.PaymentMethod.ID {
		t.Fatalf("replay = %+v %v %v", again, replayed, err)
	}
	// The external payment plugin takes one payment method per account.
	if _, _, err := e.svc.AddPaymentMethod(ctx, sc, e.call(), billing.NewPaymentMethod{IdempotencyKey: "pm-key-2"}); codeOf(err) != "billing_rejected" {
		t.Fatalf("second external payment method: %v", err)
	}
	if _, _, err := e.svc.AddPaymentMethod(ctx, sc, e.call(), billing.NewPaymentMethod{IdempotencyKey: "pm-key-3", PluginProperties: []billing.Property{{Key: "bad key", Value: "x"}}}); codeOf(err) != "validation_failed" {
		t.Fatalf("bad property: %v", err)
	}
	hosted, _, err := e.svc.AddPaymentMethod(ctx, b.Scope(), e.call(), billing.NewPaymentMethod{IdempotencyKey: "pm-key-4", HostedPage: true})
	if err != nil || hosted.Form == nil || hosted.PaymentMethod == nil {
		t.Fatalf("hosted page = %+v %v", hosted, err)
	}
	if _, err := e.svc.SetDefaultPaymentMethod(ctx, b.Scope(), e.call(), added.PaymentMethod.ID); codeOf(err) != "not_found" {
		t.Fatalf("set another tenant's default: %v", err)
	}
	if err := e.svc.DeletePaymentMethod(ctx, sc, e.call(), added.PaymentMethod.ID); codeOf(err) != "billing_rejected" {
		t.Fatalf("delete the default: %v", err)
	}
	methods, err := e.svc.ListPaymentMethods(ctx, sc)
	if err != nil || len(methods) != 1 {
		t.Fatalf("methods = %+v %v", methods, err)
	}
}

func TestNotifications(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.tenant("Tenant A")
	other := e.fake.InsertBrand(t, e.db.Pool, "zs-other")
	notification := func(event, account string) []byte {
		body, _ := json.Marshal(map[string]any{"eventType": event, "accountId": account, "objectType": "INVOICE",
			"objectId": idgen.MustNewV7().String(), "metaData": nil})
		return body
	}
	body := notification("INVOICE_CREATION", a.KillBillAccountID)
	r, err := e.svc.HandleNotification(ctx, e.brand.ID, body)
	if err != nil || r.Event != billing.EventInvoiceCreated || r.Duplicate || r.Ignored {
		t.Fatalf("invoice = %+v %v", r, err)
	}
	if r, err := e.svc.HandleNotification(ctx, e.brand.ID, body); err != nil || !r.Duplicate || r.Event != "" {
		t.Fatalf("duplicate = %+v %v", r, err)
	}
	e.fake.SetOverdue("zs-acme", a.KillBillAccountID, killbill.OverdueState{Name: "WARNING", IsBlockChanges: true})
	if r, err := e.svc.HandleNotification(ctx, e.brand.ID, notification("OVERDUE_CHANGE", a.KillBillAccountID)); err != nil || r.Event != billing.EventOverdueChanged {
		t.Fatalf("overdue = %+v %v", r, err)
	}
	var state, last string
	if err := e.db.Pool.QueryRow(ctx, `SELECT overdue_state, last_event_type FROM tenant_billing_standing WHERE tenant_id = $1`, a.ID).Scan(&state, &last); err != nil || state != "WARNING" || last != "OVERDUE_CHANGE" {
		t.Fatalf("standing = %q %q %v", state, last, err)
	}
	for name, n := range map[string]struct {
		brand idgen.UUID
		body  []byte
	}{
		"account of another brand": {other.ID, notification("INVOICE_CREATION", a.KillBillAccountID)},
		"unknown account":          {e.brand.ID, notification("PAYMENT_SUCCESS", idgen.MustNewV7().String())},
		"no account":               {e.brand.ID, []byte(`{"eventType":"TENANT_CONFIG_CHANGE","accountId":null,"objectType":"TENANT_KVS","objectId":"x","metaData":"{}"}`)},
	} {
		if r, err := e.svc.HandleNotification(ctx, n.brand, n.body); err != nil || !r.Ignored || r.Event != "" {
			t.Errorf("%s = %+v %v", name, r, err)
		}
	}
	if _, err := e.svc.HandleNotification(ctx, e.brand.ID, []byte(`{"eventType":"bad type"}`)); codeOf(err) != "validation_failed" {
		t.Fatalf("bad body: %v", err)
	}
	// Kill Bill down: the notification is not recorded, so Kill Bill's
	// retry is handled.
	e.fake.SetDown(false, true)
	retried := notification("PAYMENT_FAILED", a.KillBillAccountID)
	if _, err := e.svc.HandleNotification(ctx, e.brand.ID, retried); codeOf(err) != "billing_unavailable" {
		t.Fatalf("Kill Bill down: %v", err)
	}
	e.fake.SetDown(false, false)
	if r, err := e.svc.HandleNotification(ctx, e.brand.ID, retried); err != nil || r.Event != billing.EventPaymentFailed {
		t.Fatalf("retry = %+v %v", r, err)
	}
	rows, err := e.db.Pool.Query(ctx, `SELECT event_type, envelope FROM outbox WHERE event_type LIKE 'billing.%' ORDER BY occurred_at`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var typ string
		var env map[string]any
		if err := rows.Scan(&typ, &env); err != nil {
			t.Fatal(err)
		}
		if env["tenant_id"] != a.PublicID() || env["brand_id"] != idgen.Format(idgen.Brand, e.brand.ID) {
			t.Fatalf("event scope = %v", env)
		}
		got = append(got, typ)
	}
	if strings.Join(got, ",") != "billing.invoice.created,billing.overdue.changed,billing.payment.failed" {
		t.Fatalf("outbox = %v", got)
	}
}

func TestCredentialsAndProxy(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a, b := e.tenant("Tenant A"), e.tenant("Tenant B")
	sc := a.Scope()
	m, err := tenant.MembershipOf(ctx, e.db.Pool, sc, e.owner.Identity.ID)
	if err != nil {
		t.Fatal(err)
	}
	cred, err := e.svc.IssueCredential(ctx, sc, e.call(), billing.Holder{MembershipID: m.ID})
	if err != nil || !strings.HasPrefix(cred.Token, billing.CredentialPrefix) || cred.AccountID != a.KillBillAccountID ||
		cred.AccountExternalKey != a.PublicID() || cred.ExpiresAt.Sub(e.clock.Now()) > billing.CredentialTTL {
		t.Fatalf("credential = %+v %v", cred, err)
	}
	proxy := func(token string, tn idgen.UUID, method, path, query string, body string) (billing.ProxyResponse, error) {
		req := billing.ProxyRequest{Token: token, Tenant: tn, Method: method, Path: path, RawQuery: query}
		if body != "" {
			req.ContentType, req.Body = "application/json", []byte(body)
		}
		return e.svc.Proxy(ctx, req)
	}
	resp, err := proxy(cred.Token, a.ID, http.MethodGet, "1.0/kb/accounts/"+a.KillBillAccountID, "accountWithBalanceAndCBA=true", "")
	if err != nil || resp.Status != http.StatusOK || !strings.Contains(string(resp.Body), a.KillBillAccountID) {
		t.Fatalf("own account = %+v %v", resp, err)
	}
	if resp, err := proxy(cred.Token, a.ID, http.MethodGet, "1.0/kb/accounts", "externalKey="+a.PublicID(), ""); err != nil || resp.Status != http.StatusOK {
		t.Fatalf("own account by external key = %+v %v", resp, err)
	}
	if _, err := proxy(cred.Token, a.ID, http.MethodGet, "1.0/kb/accounts/"+b.KillBillAccountID, "", ""); codeOf(err) != "not_found" {
		t.Fatalf("another tenant's account: %v", err)
	}
	if _, err := proxy(cred.Token, a.ID, http.MethodGet, "1.0/kb/accounts", "externalKey="+b.PublicID(), ""); codeOf(err) != "not_found" {
		t.Fatalf("another tenant's external key: %v", err)
	}
	if _, err := proxy(cred.Token, b.ID, http.MethodGet, "1.0/kb/accounts/"+b.KillBillAccountID, "", ""); codeOf(err) != "billing_token_invalid" {
		t.Fatalf("credential on another tenant's route: %v", err)
	}
	created, err := proxy(cred.Token, a.ID, http.MethodPost, "1.0/kb/subscriptions", "", `{"accountId":"`+a.KillBillAccountID+`","planName":"`+enginetest.PlanBasic+`"}`)
	if err != nil || created.Status != http.StatusCreated || !strings.HasPrefix(created.Location, billing.ProxyPath(a.ID)+"1.0/kb/subscriptions/") {
		t.Fatalf("subscription through the proxy = %+v %v", created, err)
	}
	if _, err := proxy(cred.Token, a.ID, http.MethodPost, "1.0/kb/subscriptions", "", `{"accountId":"`+a.KillBillAccountID+`","planName":"p","priceOverrides":[]}`); codeOf(err) != "not_found" {
		t.Fatalf("price override: %v", err)
	}
	if _, err := proxy(cred.Token, a.ID, http.MethodPost, "1.0/kb/subscriptions", "", `{"accountId":"`+a.KillBillAccountID+`","planName":"no-such-plan"}`); codeOf(err) != "billing_rejected" {
		t.Fatalf("plan Kill Bill refuses: %v", err)
	}
	// A holder that is not an active member gets nothing.
	ghost, err := e.svc.IssueCredential(ctx, sc, e.call(), billing.Holder{MembershipID: idgen.MustNewV7()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := proxy(ghost.Token, a.ID, http.MethodGet, "1.0/kb/accounts/"+a.KillBillAccountID, "", ""); codeOf(err) != "tenant_not_found" {
		t.Fatalf("ghost holder: %v", err)
	}
	// A suspended tenant reads but does not write; an expired credential
	// does neither.
	operatorActor := events.Actor{Type: events.ActorService, ID: "zs-core-cli:test"}
	if _, err := tenant.Suspend(ctx, e.db.Pool, e.eng, sc, operatorActor, "test", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := proxy(cred.Token, a.ID, http.MethodGet, "1.0/kb/accounts/"+a.KillBillAccountID, "", ""); err != nil {
		t.Fatalf("read while suspended: %v", err)
	}
	if _, err := proxy(cred.Token, a.ID, http.MethodPost, "1.0/kb/subscriptions", "", `{"accountId":"`+a.KillBillAccountID+`","planName":"`+enginetest.PlanBasic+`"}`); codeOf(err) != "tenant_not_found" {
		t.Fatalf("write while suspended: %v", err)
	}
	e.clock.advance(billing.CredentialTTL)
	if _, err := proxy(cred.Token, a.ID, http.MethodGet, "1.0/kb/accounts/"+a.KillBillAccountID, "", ""); codeOf(err) != "billing_token_invalid" {
		t.Fatalf("expired credential: %v", err)
	}
	if _, err := (&billing.Service{DB: e.db.Pool, KillBill: e.fake.KillBill(t), Secrets: e.fake.Secrets()}).IssueCredential(ctx, sc, e.call(), billing.Holder{MembershipID: m.ID}); codeOf(err) != "not_ready" {
		t.Fatalf("credential without the secret: %v", err)
	}
}

// Concurrent redemptions of one code by one tenant write one Kill Bill
// credit.
func TestConcurrentRedemptions(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.code(billing.NewCreditCode{Code: "RACE-CODE", AmountMinor: 100, Currency: "CAD", Reason: "x", CreatedBy: "ops"})
	a := e.tenant("Tenant A")
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := e.svc.Redeem(ctx, a.Scope(), e.call(), "RACE-CODE")
			if err != nil && !zserrors.IsCode(err, zserrors.CodeIdempotencyKeyInProgress) {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("redeem: %v", err)
	}
	if n := e.fake.CreditItems("zs-acme", a.KillBillAccountID); n != 1 {
		t.Fatalf("Kill Bill credits = %d, want 1", n)
	}
	var count int
	if err := e.db.Pool.QueryRow(ctx, `SELECT redemptions FROM credit_codes WHERE hint = 'RACE'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("redemptions = %d %v", count, err)
	}
}
