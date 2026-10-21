package billing

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/pkg/brand"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

func codeOf(err error) string {
	var e *zserrors.Error
	if !errors.As(err, &e) {
		return "untyped"
	}
	return zserrors.Lookup(e.Code).Name
}

func TestMinorUnits(t *testing.T) {
	for _, c := range []struct {
		in       json.Number
		currency string
		want     int64
	}{
		{"12.34", "CAD", 1234}, {"0.00", "USD", 0}, {"-12.34", "CAD", -1234}, {"10", "CAD", 1000}, {"1.005", "CAD", 101},
		{"-1.005", "CAD", -101}, {"1.004", "CAD", 100}, {"500", "JPY", 500}, {"1.2345", "KWD", 1235}, {"1E+1", "CAD", 1000}, {"", "CAD", 0},
	} {
		got, err := ToMinor(c.in, c.currency)
		if err != nil || got != c.want {
			t.Errorf("ToMinor(%s, %s) = %d %v, want %d", c.in, c.currency, got, err, c.want)
		}
	}
	if _, err := ToMinor("1.00", "XXX"); err == nil {
		t.Error("unknown currency accepted")
	}
	if _, err := ToMinor("abc", "CAD"); err == nil {
		t.Error("not a number accepted")
	}
	for _, c := range []struct {
		minor    int64
		currency string
		want     json.Number
	}{{1234, "CAD", "12.34"}, {5, "CAD", "0.05"}, {-150, "USD", "-1.50"}, {500, "JPY", "500"}, {1, "KWD", "0.001"}} {
		if got, err := FromMinor(c.minor, c.currency); err != nil || got != c.want {
			t.Errorf("FromMinor(%d, %s) = %s %v, want %s", c.minor, c.currency, got, err, c.want)
		}
	}
}

func TestKillBillErrorsNeverLeakText(t *testing.T) {
	cause := &killbill.Error{Op: "create subscription", Status: 400, Code: 2005}
	err := kbError(fmt.Errorf("wrapped: %w", cause))
	status, env := zserrors.ToEnvelope(err)
	if status != 422 || env.Error.Code != "billing_rejected" || env.Error.Message != "Kill Bill rejected the request with error code 2005." {
		t.Fatalf("4xx = %d %+v", status, env)
	}
	for _, c := range []struct {
		err  error
		want string
	}{
		{&killbill.Unavailable{Op: "x", Status: 500, Err: errors.New("org.postgresql.util.PSQLException: duplicate key")}, "billing_unavailable"},
		{fmt.Errorf("x: %w", killbill.ErrCredentialsRejected), "billing_unavailable"},
		{fmt.Errorf("x: %w", killbill.ErrNotFound), "not_found"},
		{fmt.Errorf("x: %w", killbill.ErrConflict), "billing_rejected"},
		{&killbill.Error{Op: "x", Status: 404}, "billing_rejected"},
	} {
		got := kbError(c.err)
		_, env := zserrors.ToEnvelope(got)
		if codeOf(got) != c.want || strings.Contains(env.Error.Message, "PSQL") {
			t.Errorf("%v = %s %q", c.err, codeOf(got), env.Error.Message)
		}
	}
	if _, env := zserrors.ToEnvelope(kbError(&killbill.Unavailable{Op: "x", Status: 503, Err: errors.New("x")})); !env.Error.Retryable {
		t.Error("billing_unavailable is not retryable")
	}
}

func TestCallHeaders(t *testing.T) {
	cc := Call{Actor: events.Actor{Type: events.ActorUser, ID: "idn_x"}, Reason: "why", CorrelationID: "req-1"}.kb()
	if cc.CreatedBy != "user:idn_x" || cc.Reason != "why" || cc.Comment != "req-1" {
		t.Fatalf("call context = %+v", cc)
	}
	if cc := (Call{Actor: SweeperActor, CorrelationID: "c"}).kb(); cc.CreatedBy != "zs-core" {
		t.Fatalf("system actor = %+v", cc)
	}
}

func TestBlocked(t *testing.T) {
	state := func(service, date string, change bool) killbill.BlockingState {
		return killbill.BlockingState{Service: service, EffectiveDate: date, IsBlockChange: change}
	}
	if blocked(nil) {
		t.Fatal("no states blocks")
	}
	if !blocked([]killbill.BlockingState{state("zs-core-tenant", "2026-01-01T00:00:00.000Z", true)}) {
		t.Fatal("a blocking state does not block")
	}
	// The latest state of a service is in force: reactivation lifts it.
	if blocked([]killbill.BlockingState{state("zs-core-tenant", "2026-01-01T00:00:00.000Z", true), state("zs-core-tenant", "2026-01-02T00:00:00.000Z", false)}) {
		t.Fatal("a lifted state still blocks")
	}
	if blocked([]killbill.BlockingState{state("overdue-service", "2026-01-01T00:00:00.000Z", true)}) {
		t.Fatal("overdue states are reported by the overdue state")
	}
	st := standing{reasons: []string{HoldOverdueBlocksChanges}}
	if !st.onHold() || st.holdsEntitlement() {
		t.Fatal("an overdue state that blocks changes only must not stop entitlement")
	}
	if !(standing{reasons: []string{HoldTenantSuspended}}).holdsEntitlement() {
		t.Fatal("a suspended tenant keeps its entitlement")
	}
}

func TestPageByNumber(t *testing.T) {
	items := []int64{3, 1, 5, 4, 2}
	num := func(n int64) int64 { return n }
	first, next, err := pageByNumber(append([]int64(nil), items...), num, tenant.Page{Limit: 2})
	if err != nil || fmt.Sprint(first) != "[5 4]" || next == "" {
		t.Fatalf("first = %v %q %v", first, next, err)
	}
	second, next, err := pageByNumber(append([]int64(nil), items...), num, tenant.Page{Limit: 2, Cursor: next})
	if err != nil || fmt.Sprint(second) != "[3 2]" || next == "" {
		t.Fatalf("second = %v %q %v", second, next, err)
	}
	last, next, err := pageByNumber(append([]int64(nil), items...), num, tenant.Page{Limit: 2, Cursor: next})
	if err != nil || fmt.Sprint(last) != "[1]" || next != "" {
		t.Fatalf("last = %v %q %v", last, next, err)
	}
	if _, _, err := pageByNumber(items, num, tenant.Page{Cursor: "bad"}); codeOf(err) != "invalid_cursor" {
		t.Fatalf("bad cursor: %v", err)
	}
}

func TestCatalogueSummaryAndFeatures(t *testing.T) {
	trial := killbill.CatalogPhase{Type: "TRIAL", FixedPrices: []killbill.Price{{Currency: "CAD", Value: "0.00"}}, Duration: killbill.Duration{Unit: "DAYS", Number: 14}}
	ever := killbill.CatalogPhase{Type: "EVERGREEN", Prices: []killbill.Price{{Currency: "CAD", Value: "10.00"}, {Currency: "XTS", Value: "1"}}, Duration: killbill.Duration{Unit: "UNLIMITED", Number: -1},
		Usages: []killbill.CatalogUsage{{Tiers: []killbill.CatalogTier{{Blocks: []killbill.TieredBlock{{Unit: "api-call", Size: "1000", Prices: []killbill.Price{{Currency: "CAD", Value: "1.00"}}}}}}}}}
	c, err := summarize(killbill.Catalog{Name: "c", Currencies: []string{"CAD"}, Units: []killbill.CatalogUnit{{Name: "api-call"}},
		Products: []killbill.CatalogProduct{{Type: "BASE", Name: "core-test", Plans: []killbill.CatalogPlan{{Name: "p", BillingPeriod: "MONTHLY", Phases: []killbill.CatalogPhase{trial, ever}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	p := c.Products[0]
	if strings.Join(p.Features, ",") != "core-test,api-call" || p.Plans[0].Phases[0].Duration.Number != 14 || p.Plans[0].Phases[1].Duration != nil ||
		p.Plans[0].Phases[1].RecurringPrices[0].AmountMinor != 1000 || len(p.Plans[0].Phases[1].RecurringPrices) != 1 ||
		p.Plans[0].Phases[1].Usage[0].Prices[0].AmountMinor != 100 {
		t.Fatalf("summary = %+v", c)
	}
	for _, f := range []string{"core-test", "api-call", "vm:create", "a.b_c-d"} {
		if !ValidFeature(f) {
			t.Errorf("%s refused", f)
		}
	}
	for _, f := range []string{"", "-x", "a b", strings.Repeat("a", 101), "../x"} {
		if ValidFeature(f) {
			t.Errorf("%q accepted", f)
		}
	}
}

func TestCodes(t *testing.T) {
	for in, want := range map[string]string{" welcome10 ": "WELCOME10", "Spring-2026": "SPRING-2026"} {
		if got, ok := NormalizeCode(in); !ok || got != want {
			t.Errorf("NormalizeCode(%q) = %q %v", in, got, ok)
		}
	}
	for _, bad := range []string{"abc", "-ABCD", "AB CD", strings.Repeat("A", 65), "ÀBCD"} {
		if _, ok := NormalizeCode(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
	a, b := idgen.MustNewV7(), idgen.MustNewV7()
	if string(CodeDigest(a, "WELCOME10")) == string(CodeDigest(b, "WELCOME10")) || len(CodeDigest(a, "X")) != 32 {
		t.Fatal("a code has one digest across brands")
	}
}

func TestWebhookToken(t *testing.T) {
	s := &Service{WebhookSecret: []byte(strings.Repeat("s", 32))}
	b := idgen.MustNewV7()
	tok := brand.WebhookToken(s.WebhookSecret, b)
	if len(tok) != 64 || !s.VerifyWebhookToken(b, tok) {
		t.Fatalf("token %q not verified", tok)
	}
	// Change the last hex digit to another one: a fixed digit equals the
	// token's own last digit one time in 16.
	last := "0"
	if tok[63] == '0' {
		last = "1"
	}
	if s.VerifyWebhookToken(idgen.MustNewV7(), tok) || s.VerifyWebhookToken(b, tok[:63]+last) || (&Service{}).VerifyWebhookToken(b, tok) {
		t.Fatal("a wrong brand, token or secret verified")
	}
	if !strings.HasPrefix(brand.WebhookPath(s.WebhookSecret, b), "/v1/billing/webhooks/killbill/brd_") {
		t.Fatal("webhook path")
	}
	for _, ev := range []string{"INVOICE_CREATION", "PAYMENT_SUCCESS", "PAYMENT_FAILED", "OVERDUE_CHANGE", "SUBSCRIPTION_CREATION"} {
		if !events.ValidType(coreEvents[ev]) {
			t.Errorf("%s maps to %q", ev, coreEvents[ev])
		}
	}
	if !upperSnake("INVOICE_CREATION") || upperSnake("invoice") {
		t.Fatal("upperSnake")
	}
	if _, ok := coreEvents["INVOICE_PAYMENT_SUCCESS"]; ok {
		t.Fatal("an invoice payment would make a second payment event")
	}
	meta := `{"paymentTransactionId":"14a5deb0-4b82-491a-9c0b-d0be2dc6f89d","amount":2.00,"currency":"CAD","status":"SUCCESS","transactionType":"REFUND"}`
	got := paymentMeta(&meta)
	if got["transaction_type"] != "REFUND" || got["killbill_transaction_id"] != "14a5deb0-4b82-491a-9c0b-d0be2dc6f89d" || got["transaction_status"] != "SUCCESS" || len(got) != 3 {
		t.Fatalf("payment metadata = %v", got)
	}
	bad := `{"transactionType":"<script>"}`
	if len(paymentMeta(&bad)) != 0 || len(paymentMeta(nil)) != 0 {
		t.Fatal("unexpected metadata accepted")
	}
}

func TestCredentialSignature(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	s := &Service{WebhookSecret: []byte(strings.Repeat("k", 32)), Now: func() time.Time { return now }}
	b, tn, m, k := idgen.MustNewV7(), idgen.MustNewV7(), idgen.MustNewV7(), idgen.MustNewV7()
	mint := func(c credentialClaims) string {
		payload, _ := json.Marshal(c)
		return CredentialPrefix + b64(payload) + "." + b64(s.sign(payload))
	}
	good := credentialClaims{Version: 1, Brand: b.String(), Tenant: tn.String(), Membership: m.String(), APIKey: k.String(), Expires: now.Add(time.Minute).Unix()}
	sc, h, err := s.verifyCredential(mint(good))
	if err != nil || sc.Brand != b || sc.Tenant != tn || h.MembershipID != m || h.APIKeyID != k {
		t.Fatalf("verify = %+v %+v %v", sc, h, err)
	}
	expired := good
	expired.Expires = now.Unix()
	other := &Service{WebhookSecret: []byte(strings.Repeat("x", 32)), Now: s.Now}
	tok := mint(good)
	for name, bad := range map[string]string{
		"expired":        mint(expired),
		"tampered":       strings.Replace(tok, ".", "x.", 1),
		"no prefix":      strings.TrimPrefix(tok, CredentialPrefix),
		"api key secret": "zs_brand_abc",
	} {
		if _, _, err := s.verifyCredential(bad); codeOf(err) != "billing_token_invalid" {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, _, err := other.verifyCredential(tok); codeOf(err) != "billing_token_invalid" {
		t.Errorf("another deployment's secret verified: %v", err)
	}
	// The credential key differs from the webhook token key.
	if string(s.credentialKey()) == string(s.WebhookSecret) {
		t.Fatal("credential key is the raw secret")
	}
}

func TestAllowProxy(t *testing.T) {
	const acct = "0c772bcb-4fbb-428e-b1fe-369099ddd687"
	const other = "c38ee740-0f94-4a44-b7c1-32c0fb9d0ff2"
	const ext = "tnt_01m5bzbkj6en5tr1ekx7m6xdae"
	const plugin = "__EXTERNAL_PAYMENT__"
	sub := func(fields string) []byte { return []byte(`{"accountId":"` + acct + `","planName":"p"` + fields + `}`) }
	for _, c := range []struct {
		name, method, path, query string
		body                      []byte
		ok                        bool
	}{
		{"own account", "GET", "1.0/kb/accounts/" + acct, "accountWithBalanceAndCBA=true", nil, true},
		{"own invoices", "GET", "1.0/kb/accounts/" + acct + "/invoices", "", nil, true},
		{"own account in upper case", "GET", "1.0/kb/accounts/" + strings.ToUpper(acct) + "/payments", "", nil, true},
		{"by own external key", "GET", "1.0/kb/accounts", "externalKey=" + ext, nil, true},
		{"by another external key", "GET", "1.0/kb/accounts", "externalKey=tnt_other", nil, false},
		{"external key twice", "GET", "1.0/kb/accounts", "externalKey=" + ext + "&externalKey=x", nil, false},
		{"lookup with an unknown query", "GET", "1.0/kb/accounts", "externalKey=" + ext + "&pluginProperty=x", nil, false},
		{"another account", "GET", "1.0/kb/accounts/" + other, "", nil, false},
		{"another account below", "GET", "1.0/kb/accounts/" + other + "/invoices", "", nil, false},
		{"dot dot", "GET", "1.0/kb/accounts/" + acct + "/../" + other, "", nil, false},
		{"empty segment", "GET", "1.0/kb/accounts//" + acct, "", nil, false},
		{"encoded", "GET", "1.0/kb/accounts/" + acct + "%2F", "", nil, false},
		{"invoice by id", "GET", "1.0/kb/invoices/" + other, "", nil, false},
		{"tenant config", "GET", "1.0/kb/tenants/uploadPluginConfig/killbill-stripe", "", nil, false},
		{"not kill bill", "GET", "admin/x", "", nil, false},
		{"unblock own account", "POST", "1.0/kb/accounts/" + acct + "/block", "", []byte(`{}`), false},
		{"payment on own account", "POST", "1.0/kb/accounts/" + acct + "/payments", "", []byte(`{"transactionType":"CREDIT"}`), false},
		{"credit", "POST", "1.0/kb/credits", "", []byte(`[{"accountId":"` + acct + `"}]`), false},
		{"usage", "POST", "1.0/kb/usages", "", []byte(`{}`), false},
		{"payment method", "POST", "1.0/kb/accounts/" + acct + "/paymentMethods", "isDefault=true", []byte(`{"pluginName":"` + plugin + `","externalKey":"k"}`), true},
		{"payment method of another plugin", "POST", "1.0/kb/accounts/" + acct + "/paymentMethods", "", []byte(`{"pluginName":"killbill-stripe"}`), false},
		{"payment method paying invoices", "POST", "1.0/kb/accounts/" + acct + "/paymentMethods", "payAllUnpaidInvoices=true", []byte(`{"pluginName":"` + plugin + `"}`), false},
		{"payment method of another account", "POST", "1.0/kb/accounts/" + other + "/paymentMethods", "", []byte(`{"pluginName":"` + plugin + `"}`), false},
		{"subscription", "POST", "1.0/kb/subscriptions", "", sub(`,"externalKey":"x"`), true},
		{"subscription for another account", "POST", "1.0/kb/subscriptions", "", []byte(`{"accountId":"` + other + `","planName":"p"}`), false},
		{"subscription with price override", "POST", "1.0/kb/subscriptions", "", sub(`,"priceOverrides":[{"fixedPrice":0}]`), false},
		{"subscription backdated", "POST", "1.0/kb/subscriptions", "entitlementDate=2020-01-01", sub(""), false},
		{"subscription with a repeated account", "POST", "1.0/kb/subscriptions", "", []byte(`{"accountId":"` + acct + `","planName":"p","accountId":"` + other + `"}`), false},
		{"subscription list", "POST", "1.0/kb/subscriptions", "", []byte(`[{"accountId":"` + acct + `"}]`), false},
		{"subscription with trailing data", "POST", "1.0/kb/subscriptions", "", append(sub(""), []byte(` {}`)...), false},
		{"delete", "DELETE", "1.0/kb/accounts/" + acct, "", nil, false},
	} {
		err := AllowProxy(c.method, c.path, c.query, c.body, acct, ext, plugin)
		if (err == nil) != c.ok {
			t.Errorf("%s: %v", c.name, err)
		}
		if err != nil && codeOf(err) != "not_found" {
			t.Errorf("%s: %s, want not_found", c.name, codeOf(err))
		}
	}
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// The sweeper's bound on a reversal. Credits are used oldest first and
// credit that did not come from Core is used last, so it is never reversed.
func TestReversible(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	credit := func(amount int64, at time.Time) liveCredit {
		return liveCredit{id: idgen.MustNewV7(), remaining: amount, createdAt: at}
	}
	due := func(l liveCredit) dueCredit {
		return dueCredit{creditRow: creditRow{id: l.id, amountMinor: l.remaining, createdAt: l.createdAt}}
	}
	d := credit(1000, t0)
	newer := credit(500, t0.Add(time.Hour))
	older := credit(300, t0.Add(-time.Hour))
	for _, c := range []struct {
		name    string
		cba     int64
		live    []liveCredit
		nonCore int64
		want    int64
	}{
		{"unused", 1000, []liveCredit{d}, 0, 1000},
		{"partly used", 600, []liveCredit{d}, 0, 600},
		{"spent", 0, []liveCredit{d}, 0, 0},
		{"newer credit untouched", 1200, []liveCredit{d, newer}, 0, 700},
		{"newer credit only left", 400, []liveCredit{d, newer}, 0, -100},
		{"older credit used first", 1500, []liveCredit{older, d, newer}, 0, 1000},
		// An overpayment beyond every Core credit stays.
		{"credit beyond Core", 2000, []liveCredit{d}, 0, 1000},
		// The Core credit was spent, then an overpayment added 500.
		{"spent, then an overpayment", 500, []liveCredit{d}, 500, 0},
		{"partly spent, then an overpayment", 900, []liveCredit{d}, 500, 400},
		// A pending credit is newer credit too.
		{"pending newer credit", 1100, []liveCredit{d, credit(500, t0.Add(time.Minute))}, 0, 600},
	} {
		if got := reversible(due(d), c.cba, c.live, c.nonCore); got != c.want {
			t.Errorf("%s: reversible = %d, want %d", c.name, got, c.want)
		}
	}
	// Two credits written in the same instant are ordered by ID.
	a, b := credit(100, t0), credit(100, t0)
	if a.id.String() > b.id.String() {
		a, b = b, a
	}
	if got := reversible(due(a), 200, []liveCredit{a, b}, 0); got != 100 {
		t.Errorf("same instant, older ID = %d", got)
	}
	if got := reversible(due(b), 100, []liveCredit{b}, 0); got != 100 {
		t.Errorf("same instant, newer ID = %d", got)
	}
}

func TestNonCoreCreditAdded(t *testing.T) {
	core := creditDescription(idgen.MustNewV7())
	invoices := []killbill.Invoice{
		{Items: []killbill.InvoiceItem{{ItemType: "CREDIT_ADJ", Amount: "-10.00", Description: core}, {ItemType: "CBA_ADJ", Amount: "10.00"}}},
		{Items: []killbill.InvoiceItem{{ItemType: "EXTERNAL_CHARGE", Amount: "10.00"}, {ItemType: "CBA_ADJ", Amount: "-10.00"}}},
		{Items: []killbill.InvoiceItem{{ItemType: "RECURRING", Amount: "5.00"}, {ItemType: "CBA_ADJ", Amount: "3.00"}}},
		{Items: []killbill.InvoiceItem{{ItemType: "CREDIT_ADJ", Amount: "-2.00", Description: "goodwill"}, {ItemType: "CBA_ADJ", Amount: "2.00"}}},
	}
	if got, err := nonCoreCreditAdded(invoices, "CAD"); err != nil || got != 500 {
		t.Fatalf("non-Core credit = %d %v, want 500", got, err)
	}
	if !strings.HasPrefix(core, creditDescriptionPrefix) || core != "zs-core credit "+strings.TrimPrefix(core, "zs-core credit ") {
		t.Fatalf("description = %q", core)
	}
}

func TestSweepBackoff(t *testing.T) {
	for attempts, want := range map[int]time.Duration{1: 10 * time.Second, 2: 20 * time.Second, 3: 40 * time.Second, 10: 5120 * time.Second, 14: 81920 * time.Second, 15: 24 * time.Hour, 60: 24 * time.Hour} {
		if got := sweepBackoff(attempts); got != want {
			t.Errorf("after %d attempts = %s, want %s", attempts, got, want)
		}
	}
}

// A refund is bounded by what the payment captured or purchased and has
// not refunded.
func TestRefundable(t *testing.T) {
	if got := refundable(Payment{PurchasedMinor: 2000, RefundedMinor: 500}); got != 1500 {
		t.Fatalf("purchase = %d", got)
	}
	if got := refundable(Payment{AuthMinor: 3000, CapturedMinor: 1000, RefundedMinor: 1000}); got != 0 {
		t.Fatalf("capture = %d", got)
	}
	key := "zs-refund-x"
	p := killbill.Payment{Transactions: []killbill.PaymentTransaction{
		{TransactionID: "a", TransactionType: "REFUND", TransactionExternalKey: key, Status: "PAYMENT_FAILURE"},
		{TransactionID: "b", TransactionType: "PURCHASE", TransactionExternalKey: key, Status: "SUCCESS"},
	}}
	if got := refundTransaction(p, key); got != "" {
		t.Fatalf("failed refund counted as done: %q", got)
	}
	p.Transactions = append(p.Transactions, killbill.PaymentTransaction{TransactionID: "c", TransactionType: "REFUND", TransactionExternalKey: key, Status: "SUCCESS"})
	if got := refundTransaction(p, key); got != "c" {
		t.Fatalf("refund = %q", got)
	}
}
