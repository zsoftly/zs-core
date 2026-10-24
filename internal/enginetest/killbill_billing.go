package enginetest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/pkg/idgen"
)

// ExternalPlugin is Kill Bill's built-in payment plugin.
const ExternalPlugin = "__EXTERNAL_PAYMENT__"

// HostedPlugin is a payment plugin of the fake with a hosted payment page.
const HostedPlugin = "killbill-hosted-test"

// The test catalogue: a BASE product core-test with a monthly plan that has
// a 14-day trial and the usage unit api-call, a BASE product core-basic
// with a monthly plan without usage, and a BASE product core-metered whose
// monthly plan has no trial and prices api-call per unit.
const (
	PlanUsage    = "core-test-monthly"
	PlanBasic    = "core-basic-monthly"
	PlanMetered  = "core-metered-monthly"
	ProductTest  = "core-test"
	ProductMeter = "core-metered"
	UnitAPICall  = "api-call"
)

// KillBillWrite is the audit headers of one Kill Bill write the fake
// received.
type KillBillWrite struct {
	Method, Path, APIKey, CreatedBy, Reason, Comment string
}

// Reads returns the paths of the Kill Bill reads the fake served, oldest
// first, so a test can count the calls a piece of code makes.
func (f *Fake) Reads() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reads...)
}

// Writes returns the Kill Bill writes the fake received, oldest first.
func (f *Fake) Writes() []KillBillWrite {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]KillBillWrite(nil), f.requests...)
}

type kbItem struct {
	id, invoiceID, typ, description, subscriptionID, plan string
	start, end                                            string
	amount                                                int64
}

type kbInvoice struct {
	id, accountID, currency string
	number                  int
	items                   []*kbItem
	trackingIDs             []string
}

func (in *kbInvoice) amount() int64 {
	var sum int64
	for _, it := range in.items {
		if it.typ != "CBA_ADJ" && it.typ != "CREDIT_ADJ" {
			sum += it.amount
		}
	}
	return sum
}

func (in *kbInvoice) balance() int64 {
	var sum int64
	for _, it := range in.items {
		sum += it.amount
	}
	if sum < 0 {
		return 0
	}
	return sum
}

type kbSub struct {
	id, bundleID, accountID, externalKey, plan, product, state, phase, cancelled, start string
	// category is "" for a base subscription and "ADD_ON" for an add-on.
	category string
	// scheduled is true while a cancel waits for a future day: the state is
	// still ACTIVE and cancelled holds the day it takes effect.
	scheduled bool
}

type kbTxn struct {
	id, key, typ, status string
	amount               int64
}

type kbPayment struct {
	id, accountID, currency, pm string
	number                      int
	txns                        []kbTxn
}

func (p *kbPayment) total(typ string) int64 {
	var sum int64
	for _, t := range p.txns {
		if t.typ == typ && t.status == "SUCCESS" {
			sum += t.amount
		}
	}
	return sum
}

type kbPM struct {
	id, accountID, externalKey, plugin string
	isDefault, deleted                 bool
}

// kbUsage is one usage record the fake holds. AmountMicro is the amount in
// millionths of the unit.
type kbUsage struct {
	subscriptionID, trackingID, unit, date string
	amountMicro                            int64
	invoiced                               bool
}

type kbTenant struct {
	usages     []kbUsage
	catalog    []killbill.Catalog
	subs       []*kbSub
	invoices   []*kbInvoice
	payments   []*kbPayment
	pms        []*kbPM
	cba        map[string]int64
	overdue    map[string]killbill.OverdueState
	callbacks  []string
	configs    map[string]string
	nextNumber int
	nextPay    int
}

func newKBTenant() *kbTenant {
	return &kbTenant{catalog: []killbill.Catalog{TestCatalog()}, cba: map[string]int64{},
		overdue: map[string]killbill.OverdueState{}, configs: map[string]string{}}
}

func cad(v string) []killbill.Price {
	return []killbill.Price{{Currency: "CAD", Value: json.Number(v)}, {Currency: "USD", Value: json.Number(v)}}
}

// TestCatalog is the catalogue every Kill Bill tenant of the fake starts
// with, in Kill Bill's JSON shape.
func TestCatalog() killbill.Catalog {
	trial := killbill.CatalogPhase{Type: "TRIAL", FixedPrices: cad("0.00"), Prices: []killbill.Price{}, Duration: killbill.Duration{Unit: "DAYS", Number: 14}}
	usage := killbill.CatalogPhase{Type: "EVERGREEN", Prices: cad("10.00"), FixedPrices: []killbill.Price{}, Duration: killbill.Duration{Unit: "UNLIMITED", Number: -1},
		Usages: []killbill.CatalogUsage{{BillingPeriod: "MONTHLY", Tiers: []killbill.CatalogTier{{Blocks: []killbill.TieredBlock{{Unit: UnitAPICall, Size: "1000", Max: "1000000", Prices: cad("1.00")}}}}}}}
	basic := killbill.CatalogPhase{Type: "EVERGREEN", Prices: cad("5.00"), FixedPrices: []killbill.Price{}, Duration: killbill.Duration{Unit: "UNLIMITED", Number: -1}}
	metered := killbill.CatalogPhase{Type: "EVERGREEN", Prices: []killbill.Price{}, FixedPrices: []killbill.Price{}, Duration: killbill.Duration{Unit: "UNLIMITED", Number: -1},
		Usages: []killbill.CatalogUsage{{BillingPeriod: "MONTHLY", Tiers: []killbill.CatalogTier{{Blocks: []killbill.TieredBlock{{Unit: UnitAPICall, Size: "1", Max: "100000000", Prices: cad("0.01")}}}}}}}
	return killbill.Catalog{Name: "ZsReference", EffectiveDate: "2026-10-01T00:00:00.000+00:00", Currencies: []string{"CAD", "USD"},
		Units: []killbill.CatalogUnit{{Name: UnitAPICall, PrettyName: UnitAPICall}},
		Products: []killbill.CatalogProduct{
			{Type: "BASE", Name: ProductTest, PrettyName: ProductTest, Included: []string{}, Available: []string{},
				Plans: []killbill.CatalogPlan{{Name: PlanUsage, PrettyName: PlanUsage, BillingPeriod: "MONTHLY", Phases: []killbill.CatalogPhase{trial, usage}}}},
			{Type: "BASE", Name: "core-basic", PrettyName: "core-basic", Included: []string{}, Available: []string{},
				Plans: []killbill.CatalogPlan{{Name: PlanBasic, PrettyName: PlanBasic, BillingPeriod: "MONTHLY", Phases: []killbill.CatalogPhase{trial, basic}}}},
			{Type: "BASE", Name: ProductMeter, PrettyName: ProductMeter, Included: []string{}, Available: []string{},
				Plans: []killbill.CatalogPlan{{Name: PlanMetered, PrettyName: PlanMetered, BillingPeriod: "NO_BILLING_PERIOD", Phases: []killbill.CatalogPhase{metered}}}},
		}}
}

// SetOverdue sets the overdue state of an account.
func (f *Fake) SetOverdue(apiKey, accountID string, s killbill.OverdueState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.billing[apiKey].overdue[accountID] = s
}

// AddPurchase records a successful payment on an account through its
// default payment method, as a payment taken outside Core would, and
// returns its ID.
func (f *Fake) AddPurchase(apiKey, accountID string, amountMinor int64, currency string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.billing[apiKey]
	t.nextPay++
	p := &kbPayment{id: idgen.MustNewV7().String(), accountID: accountID, currency: currency, number: t.nextPay,
		txns: []kbTxn{{id: idgen.MustNewV7().String(), typ: "PURCHASE", status: "SUCCESS", amount: amountMinor}}}
	t.payments = append(t.payments, p)
	return p.id
}

// AddKillBillCredit writes a credit to an account behind Core's back.
func (f *Fake) AddKillBillCredit(apiKey, accountID string, amountMinor int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.billing[apiKey]
	t.addInvoice(accountID, "CAD", []*kbItem{{typ: "CREDIT_ADJ", amount: -amountMinor, description: "outside Core"}, {typ: "CBA_ADJ", amount: amountMinor}})
	t.cba[accountID] += amountMinor
}

// Credit returns the account credit of an account, in minor units.
func (f *Fake) Credit(apiKey, accountID string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.billing[apiKey].cba[accountID]
}

// Callbacks returns the push notification callbacks of a Kill Bill tenant.
func (f *Fake) Callbacks(apiKey string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.billing[apiKey].callbacks...)
}

// Subscriptions counts the subscriptions of a Kill Bill tenant.
func (f *Fake) Subscriptions(apiKey string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.billing[apiKey].subs)
}

// CreditItems counts the credit items of an account.
func (f *Fake) CreditItems(apiKey, accountID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, in := range f.billing[apiKey].invoices {
		for _, it := range in.items {
			if in.accountID == accountID && it.typ == "CREDIT_ADJ" {
				n++
			}
		}
	}
	return n
}

// Plugin returns the version and state of a plugin on the first node.
func (f *Fake) Plugin(key string) (string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.nodes[0].PluginsInfo {
		if p.PluginKey == key {
			return p.Version, p.State
		}
	}
	return "", ""
}

func (t *kbTenant) addInvoice(accountID, currency string, items []*kbItem) *kbInvoice {
	t.nextNumber++
	in := &kbInvoice{id: idgen.MustNewV7().String(), accountID: accountID, currency: currency, number: t.nextNumber}
	for _, it := range items {
		it.id, it.invoiceID = idgen.MustNewV7().String(), in.id
		in.items = append(in.items, it)
	}
	t.invoices = append(t.invoices, in)
	return in
}

// money formats minor units of a two-digit currency as Kill Bill does.
func money(minor int64) json.Number {
	sign := ""
	if minor < 0 {
		sign, minor = "-", -minor
	}
	return json.Number(fmt.Sprintf("%s%d.%02d", sign, minor/100, minor%100))
}

// parseMoney reads a decimal with at most two digits.
func parseMoney(v json.Number) (int64, bool) {
	s := string(v)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	whole, frac, _ := strings.Cut(s, ".")
	if len(frac) > 2 {
		return 0, false
	}
	frac += strings.Repeat("0", 2-len(frac))
	w, err1 := strconv.ParseInt(whole, 10, 64)
	f, err2 := strconv.ParseInt(frac, 10, 64)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	m := w*100 + f
	if neg {
		m = -m
	}
	return m, true
}

func kbError(w http.ResponseWriter, status, code int) {
	reply(w, status, map[string]any{"code": code, "message": "fake Kill Bill refusal"})
}

// kbException answers as Kill Bill does for a Java exception: the class, a
// code (0 for none) and a message.
func kbException(w http.ResponseWriter, status int, className string, code int, message string) {
	var c any
	if code != 0 {
		c = code
	}
	reply(w, status, map[string]any{"className": className, "code": c, "message": message, "causeClassName": nil, "causeMessage": nil, "stackTrace": []any{}})
}

func (f *Fake) account(apiKey, id string) *Account {
	for _, a := range f.accounts[apiKey] {
		if a.ID == id {
			return a
		}
	}
	return nil
}

func (f *Fake) blockedChange(a *Account) bool {
	latest := map[string]killbill.BlockingState{}
	for _, b := range a.Blocking {
		latest[b.Service] = b
	}
	for _, b := range latest {
		if b.IsBlockChange {
			return true
		}
	}
	return false
}

func (t *kbTenant) plan(name string) (killbill.CatalogProduct, killbill.CatalogPlan, bool) {
	c := t.catalog[len(t.catalog)-1]
	for _, p := range c.Products {
		for _, pl := range p.Plans {
			if pl.Name == name {
				return p, pl, true
			}
		}
	}
	return killbill.CatalogProduct{}, killbill.CatalogPlan{}, false
}

func (s *kbSub) productCategory() string {
	if s.category == "" {
		return "BASE"
	}
	return s.category
}

func (s *kbSub) json() killbill.Subscription {
	return killbill.Subscription{AccountID: s.accountID, BundleID: s.bundleID, SubscriptionID: s.id, ExternalKey: s.externalKey,
		StartDate: s.start, ProductName: s.product, ProductCategory: s.productCategory(), BillingPeriod: "MONTHLY",
		PhaseType: s.phase, PriceList: "DEFAULT", PlanName: s.plan, State: s.state, CancelledDate: s.cancelled}
}

func (in *kbInvoice) json(withItems bool) killbill.Invoice {
	out := killbill.Invoice{InvoiceID: in.id, InvoiceNumber: strconv.Itoa(in.number), InvoiceDate: "2026-10-20", TargetDate: "2026-10-20",
		Status: "COMMITTED", Currency: in.currency, AccountID: in.accountID, Amount: money(in.amount()), Balance: money(in.balance()),
		CreditAdj: "0.00", RefundAdj: "0.00", Items: []killbill.InvoiceItem{}}
	if withItems {
		for _, it := range in.items {
			out.Items = append(out.Items, it.json(in))
		}
		out.TrackingIDs = append([]string(nil), in.trackingIDs...)
	}
	return out
}

func (it *kbItem) json(in *kbInvoice) killbill.InvoiceItem {
	return killbill.InvoiceItem{InvoiceItemID: it.id, InvoiceID: in.id, AccountID: in.accountID, ItemType: it.typ,
		Description: it.description, SubscriptionID: it.subscriptionID, PlanName: it.plan, Amount: money(it.amount), Currency: in.currency,
		StartDate: it.start, EndDate: it.end}
}

func (p *kbPayment) json() killbill.Payment {
	out := killbill.Payment{AccountID: p.accountID, PaymentID: p.id, PaymentNumber: strconv.Itoa(p.number), Currency: p.currency,
		PaymentMethodID: p.pm, AuthAmount: "0.00", CapturedAmount: "0.00", PurchasedAmount: money(p.total("PURCHASE")),
		RefundedAmount: money(p.total("REFUND")), CreditedAmount: "0.00"}
	for _, t := range p.txns {
		out.Transactions = append(out.Transactions, killbill.PaymentTransaction{TransactionID: t.id, TransactionExternalKey: t.key,
			PaymentID: p.id, TransactionType: t.typ, Amount: money(t.amount), Currency: p.currency, EffectiveDate: "2026-10-20T00:00:00.000Z", Status: t.status})
	}
	return out
}

func (pm *kbPM) json() killbill.PaymentMethod {
	return killbill.PaymentMethod{PaymentMethodID: pm.id, ExternalKey: pm.externalKey, AccountID: pm.accountID, IsDefault: pm.isDefault, PluginName: pm.plugin}
}

// serveBilling serves the billing endpoints and reports whether it handled
// the request. f.mu is held.
func (f *Fake) serveBilling(w http.ResponseWriter, r *http.Request, apiKey, path string, body []byte) bool {
	t := f.billing[apiKey]
	if t == nil {
		t = newKBTenant()
		f.billing[apiKey] = t
	}
	q := r.URL.Query()
	parts := strings.Split(strings.TrimPrefix(path, "/1.0/kb/"), "/")
	switch {
	case path == "/1.0/kb/catalog" && r.Method == http.MethodGet:
		reply(w, http.StatusOK, t.catalog)
	case path == "/1.0/kb/invoices/dryRun" && r.Method == http.MethodPost:
		f.dryRun(w, t, q.Get("accountId"))
	case path == "/1.0/kb/usages" && r.Method == http.MethodPost:
		f.recordUsage(w, t, body)
	case len(parts) == 2 && parts[0] == "usages" && r.Method == http.MethodGet:
		f.rolledUpUsage(w, t, parts[1], q)
	case path == "/1.0/kb/tenants/registerNotificationCallback" && r.Method == http.MethodGet:
		reply(w, http.StatusOK, map[string]any{"key": "PUSH_NOTIFICATION_CB", "values": t.callbacks})
	case path == "/1.0/kb/tenants/registerNotificationCallback" && r.Method == http.MethodPost:
		// Kill Bill 0.24.22 keeps one callback per tenant.
		t.callbacks = []string{q.Get("cb")}
		w.WriteHeader(http.StatusCreated)
	case len(parts) == 2 && parts[0] == "accounts" && r.Method == http.MethodGet:
		a := f.account(apiKey, parts[1])
		if a == nil {
			kbError(w, http.StatusNotFound, 3003)
			return true
		}
		var owed int64
		for _, in := range t.invoices {
			if in.accountID == a.ID {
				owed += in.balance()
			}
		}
		out := killbill.Account{AccountID: a.ID, ExternalKey: a.ExternalKey, Name: a.Name, Currency: a.Currency, Country: a.Country, Email: a.Email, Locale: a.Locale}
		if q.Get("accountWithBalanceAndCBA") == "true" {
			out.AccountBalance, out.AccountCBA = money(owed-t.cba[a.ID]), money(t.cba[a.ID])
		}
		reply(w, http.StatusOK, out)
	case len(parts) == 2 && parts[0] == "accounts" && r.Method == http.MethodPut:
		a := f.account(apiKey, parts[1])
		if a == nil {
			kbError(w, http.StatusNotFound, 3003)
			return true
		}
		// Reproduce the primary-email update with default merge semantics.
		// A non-null empty string clears email; omitted/null email preserves it.
		var in struct {
			Email *string `json:"email"`
		}
		if err := json.Unmarshal(body, &in); err != nil || q.Get("treatNullAsReset") == "true" {
			kbError(w, http.StatusBadRequest, 2)
			return true
		}
		if in.Email != nil {
			a.Email = *in.Email
		}
		w.WriteHeader(http.StatusNoContent)
	case len(parts) == 3 && parts[0] == "accounts" && f.account(apiKey, parts[1]) == nil:
		kbError(w, http.StatusNotFound, 3003)
	case len(parts) == 3 && parts[0] == "accounts" && parts[2] == "emails" && r.Method == http.MethodGet:
		a := f.account(apiKey, parts[1])
		emails := make([]killbill.AccountEmail, 0, len(a.AdditionalEmails))
		for _, email := range a.AdditionalEmails {
			emails = append(emails, killbill.AccountEmail{AccountID: a.ID, Email: email})
		}
		reply(w, http.StatusOK, emails)
	case len(parts) == 3 && parts[0] == "accounts" && parts[2] == "block" && r.Method == http.MethodGet:
		a := f.account(apiKey, parts[1])
		out := []killbill.BlockingState{}
		for i, b := range a.Blocking {
			b.EffectiveDate = time.Date(2026, 1, 1, 0, 0, i, 0, time.UTC).Format("2006-01-02T15:04:05.000Z")
			out = append(out, b)
		}
		reply(w, http.StatusOK, out)
	case len(parts) == 3 && parts[0] == "accounts" && parts[2] == "overdue" && r.Method == http.MethodGet:
		s, ok := t.overdue[parts[1]]
		if !ok {
			s = killbill.OverdueState{Name: "__KILLBILL__CLEAR__OVERDUE_STATE__", IsClearState: true}
		}
		reply(w, http.StatusOK, s)
	case len(parts) == 3 && parts[0] == "accounts" && parts[2] == "bundles" && r.Method == http.MethodGet:
		bundles := map[string]*killbill.Bundle{}
		var order []string
		for _, s := range t.subs {
			if s.accountID != parts[1] {
				continue
			}
			b, ok := bundles[s.bundleID]
			if !ok {
				b = &killbill.Bundle{AccountID: s.accountID, BundleID: s.bundleID, ExternalKey: s.bundleID}
				bundles[s.bundleID] = b
				order = append(order, s.bundleID)
			}
			b.Subscriptions = append(b.Subscriptions, s.json())
		}
		out := []killbill.Bundle{}
		for _, id := range order {
			out = append(out, *bundles[id])
		}
		reply(w, http.StatusOK, out)
	case len(parts) == 3 && parts[0] == "accounts" && parts[2] == "invoices" && r.Method == http.MethodGet:
		out := []killbill.Invoice{}
		for _, in := range t.invoices {
			if in.accountID == parts[1] {
				out = append(out, in.json(q.Get("includeInvoiceComponents") == "true"))
			}
		}
		reply(w, http.StatusOK, out)
	case len(parts) == 3 && parts[0] == "accounts" && parts[2] == "payments" && r.Method == http.MethodGet:
		out := []killbill.Payment{}
		for _, p := range t.payments {
			if p.accountID == parts[1] {
				out = append(out, p.json())
			}
		}
		reply(w, http.StatusOK, out)
	case len(parts) == 3 && parts[0] == "accounts" && parts[2] == "paymentMethods" && r.Method == http.MethodGet:
		out := []killbill.PaymentMethod{}
		for _, pm := range t.pms {
			if pm.accountID == parts[1] && !pm.deleted {
				out = append(out, pm.json())
			}
		}
		reply(w, http.StatusOK, out)
	case len(parts) == 3 && parts[0] == "accounts" && parts[2] == "paymentMethods" && r.Method == http.MethodPost:
		var in killbill.PaymentMethod
		_ = json.Unmarshal(body, &in)
		pm, code := f.addPM(apiKey, t, parts[1], in, q.Get("isDefault") == "true")
		if code != 0 {
			kbError(w, http.StatusBadRequest, code)
			return true
		}
		w.Header().Set("Location", "http://fake/1.0/kb/paymentMethods/"+pm.id)
		w.WriteHeader(http.StatusCreated)
	case len(parts) == 5 && parts[0] == "accounts" && parts[2] == "paymentMethods" && parts[4] == "setDefault" && r.Method == http.MethodPut:
		var found *kbPM
		for _, pm := range t.pms {
			if pm.id == parts[3] && pm.accountID == parts[1] && !pm.deleted {
				found = pm
			}
		}
		if found == nil {
			kbError(w, http.StatusNotFound, 7002)
			return true
		}
		for _, pm := range t.pms {
			if pm.accountID == parts[1] {
				pm.isDefault = pm == found
			}
		}
		w.WriteHeader(http.StatusNoContent)
	case path == "/1.0/kb/paymentGateways/hosted/form" && r.Method == http.MethodPost:
		var in struct {
			Account       killbill.Account       `json:"account"`
			PaymentMethod killbill.PaymentMethod `json:"paymentMethod"`
		}
		_ = json.Unmarshal(body, &in)
		if f.account(apiKey, in.Account.AccountID) == nil {
			kbError(w, http.StatusNotFound, 3003)
			return true
		}
		if _, code := f.addPM(apiKey, t, in.Account.AccountID, in.PaymentMethod, false); code != 0 {
			kbError(w, http.StatusBadRequest, code)
			return true
		}
		desc := map[string]any{"kbAccountId": in.Account.AccountID, "formMethod": nil, "formUrl": nil, "formFields": map[string]any{}, "properties": map[string]any{}}
		if in.PaymentMethod.PluginName == HostedPlugin {
			desc["formMethod"], desc["formUrl"] = "GET", "https://pay.example.test/form"
			desc["formFields"] = map[string]any{"session": "sess_test_123"}
		}
		reply(w, http.StatusOK, desc)
	case len(parts) == 2 && parts[0] == "paymentMethods":
		var found *kbPM
		for _, pm := range t.pms {
			if pm.id == parts[1] && !pm.deleted {
				found = pm
			}
		}
		if found == nil {
			kbError(w, http.StatusNotFound, 7002)
			return true
		}
		switch r.Method {
		case http.MethodGet:
			reply(w, http.StatusOK, found.json())
		case http.MethodDelete:
			if found.isDefault {
				kbError(w, http.StatusBadRequest, 7015)
				return true
			}
			found.deleted = true
			w.WriteHeader(http.StatusNoContent)
		}
	case path == "/1.0/kb/subscriptions" && r.Method == http.MethodGet:
		for _, s := range t.subs {
			if s.externalKey == q.Get("externalKey") {
				reply(w, http.StatusOK, s.json())
				return true
			}
		}
		kbError(w, http.StatusNotFound, 1091)
	case path == "/1.0/kb/subscriptions" && r.Method == http.MethodPost:
		var in killbill.Subscription
		_ = json.Unmarshal(body, &in)
		a := f.account(apiKey, in.AccountID)
		if a == nil {
			kbError(w, http.StatusNotFound, 3003)
			return true
		}
		for _, s := range t.subs {
			if in.ExternalKey != "" && s.externalKey == in.ExternalKey {
				// Kill Bill 0.24.22 answers a repeated external key with 500.
				reply(w, http.StatusInternalServerError, map[string]any{"code": nil, "message": "duplicate key value violates unique constraint"})
				return true
			}
		}
		if f.blockedChange(a) {
			kbError(w, http.StatusBadRequest, 1090)
			return true
		}
		prod, plan, ok := t.plan(in.PlanName)
		if !ok {
			kbError(w, http.StatusBadRequest, 2005)
			return true
		}
		s := &kbSub{id: idgen.MustNewV7().String(), bundleID: idgen.MustNewV7().String(), accountID: a.ID, externalKey: in.ExternalKey,
			plan: plan.Name, product: prod.Name, state: "ACTIVE", phase: plan.Phases[0].Type, start: DefaultSubscriptionStart}
		if in.BundleID != "" {
			// A bundle ID makes the subscription an add-on of that bundle,
			// which must be one of the account's with a base subscription
			// that is still active.
			var base *kbSub
			for _, x := range t.subs {
				if x.bundleID == in.BundleID && x.accountID == a.ID && x.category == "" {
					base = x
				}
			}
			if base == nil || base.state != "ACTIVE" {
				kbException(w, http.StatusNotFound, "org.killbill.billing.util.callcontext.InternalCallContextFactory$ObjectDoesNotExist", 0, "Object id="+in.BundleID+" type=BUNDLE doesn't exist!")
				return true
			}
			s.bundleID, s.category = in.BundleID, "ADD_ON"
		}
		t.subs = append(t.subs, s)
		t.addInvoice(a.ID, a.Currency, []*kbItem{{typ: "FIXED", amount: 0, description: plan.Name + "-trial", subscriptionID: s.id, plan: plan.Name}})
		w.Header().Set("Location", "http://fake/1.0/kb/subscriptions/"+s.id)
		w.WriteHeader(http.StatusCreated)
	case len(parts) == 2 && parts[0] == "subscriptions":
		var s *kbSub
		for _, x := range t.subs {
			if x.id == parts[1] {
				s = x
			}
		}
		if s == nil {
			if f.subscriptionOfAnotherTenant(apiKey, parts[1]) {
				// Kill Bill 0.24.22 answers a subscription of another Kill
				// Bill tenant with a server error, not a 404.
				kbException(w, http.StatusInternalServerError, "java.lang.IllegalStateException", 0,
					"Object id="+parts[1]+" type=SUBSCRIPTION doesn't belong to tenant id="+f.tenantIDs[apiKey])
				return true
			}
			kbException(w, http.StatusNotFound, "org.killbill.billing.util.callcontext.InternalCallContextFactory$ObjectDoesNotExist", 0,
				"Object id="+parts[1]+" type=SUBSCRIPTION doesn't exist!")
			return true
		}
		switch r.Method {
		case http.MethodGet:
			reply(w, http.StatusOK, s.json())
		case http.MethodPut:
			var in killbill.Subscription
			_ = json.Unmarshal(body, &in)
			prod, plan, ok := t.plan(in.PlanName)
			if !ok || (q.Get("billingPolicy") != killbill.PolicyImmediate && q.Get("billingPolicy") != killbill.PolicyEndOfTerm) {
				kbError(w, http.StatusBadRequest, 2005)
				return true
			}
			if q.Get("billingPolicy") == killbill.PolicyImmediate {
				s.plan, s.product = plan.Name, prod.Name
			}
			w.WriteHeader(http.StatusNoContent)
		case http.MethodDelete:
			f.cancelSubscription(w, t, s, q)
		}
	case len(parts) == 2 && parts[0] == "invoices" && r.Method == http.MethodGet:
		for _, in := range t.invoices {
			if in.id == parts[1] {
				reply(w, http.StatusOK, in.json(true))
				return true
			}
		}
		kbError(w, http.StatusNotFound, 4006)
	case path == "/1.0/kb/credits" && r.Method == http.MethodPost:
		var items []killbill.InvoiceItem
		_ = json.Unmarshal(body, &items)
		if len(items) != 1 {
			kbError(w, http.StatusBadRequest, 2)
			return true
		}
		a := f.account(apiKey, items[0].AccountID)
		amount, ok := parseMoney(items[0].Amount)
		if a == nil {
			kbError(w, http.StatusNotFound, 3003)
			return true
		}
		if !ok || amount <= 0 {
			kbError(w, http.StatusBadRequest, 4009)
			return true
		}
		in := t.addInvoice(a.ID, items[0].Currency, []*kbItem{{typ: "CREDIT_ADJ", amount: -amount, description: items[0].Description}, {typ: "CBA_ADJ", amount: amount}})
		t.cba[a.ID] += amount
		out := in.items[0].json(in)
		out.Amount = money(amount)
		reply(w, http.StatusOK, []killbill.InvoiceItem{out})
	case len(parts) == 3 && parts[0] == "invoices" && parts[1] == "charges" && r.Method == http.MethodPost:
		var items []killbill.InvoiceItem
		_ = json.Unmarshal(body, &items)
		a := f.account(apiKey, parts[2])
		if a == nil || len(items) != 1 {
			kbError(w, http.StatusNotFound, 3003)
			return true
		}
		amount, ok := parseMoney(items[0].Amount)
		if !ok || amount <= 0 {
			kbError(w, http.StatusBadRequest, 4009)
			return true
		}
		// Kill Bill pays a new invoice from the account credit first.
		used := amount
		if used > t.cba[a.ID] {
			used = t.cba[a.ID]
		}
		list := []*kbItem{{typ: "EXTERNAL_CHARGE", amount: amount, description: items[0].Description}}
		if used > 0 {
			list = append(list, &kbItem{typ: "CBA_ADJ", amount: -used})
		}
		in := t.addInvoice(a.ID, items[0].Currency, list)
		t.cba[a.ID] -= used
		reply(w, http.StatusOK, []killbill.InvoiceItem{in.items[0].json(in)})
	case len(parts) == 2 && parts[0] == "payments" && r.Method == http.MethodGet:
		for _, p := range t.payments {
			if p.id == parts[1] {
				reply(w, http.StatusOK, p.json())
				return true
			}
		}
		kbError(w, http.StatusNotFound, 7019)
	case len(parts) == 3 && parts[0] == "payments" && parts[2] == "refunds" && r.Method == http.MethodPost:
		var p *kbPayment
		for _, x := range t.payments {
			if x.id == parts[1] {
				p = x
			}
		}
		if p == nil {
			kbError(w, http.StatusNotFound, 7019)
			return true
		}
		var in killbill.PaymentTransaction
		_ = json.Unmarshal(body, &in)
		for _, x := range p.txns {
			if in.TransactionExternalKey != "" && x.key == in.TransactionExternalKey && x.status == "SUCCESS" {
				kbError(w, http.StatusBadRequest, 7030)
				return true
			}
		}
		amount, ok := parseMoney(in.Amount)
		if !ok || amount <= 0 || amount > p.total("PURCHASE")-p.total("REFUND") || in.Currency != p.currency {
			kbError(w, http.StatusBadRequest, 7004)
			return true
		}
		p.txns = append(p.txns, kbTxn{id: idgen.MustNewV7().String(), key: in.TransactionExternalKey, typ: "REFUND", status: "SUCCESS", amount: amount})
		w.Header().Set("Location", "http://fake/1.0/kb/payments/"+p.id+"/")
		w.WriteHeader(http.StatusCreated)
	default:
		return false
	}
	return true
}

// addPM adds a payment method with the external payment plugin's rule: one
// external payment method per account. The first payment method of an
// account is its default. It returns a Kill Bill error code on refusal.
func (f *Fake) addPM(apiKey string, t *kbTenant, accountID string, in killbill.PaymentMethod, isDefault bool) (*kbPM, int) {
	if f.account(apiKey, accountID) == nil {
		return nil, 3003
	}
	first := true
	for _, pm := range t.pms {
		if pm.accountID == accountID && !pm.deleted {
			first = false
			if in.PluginName == ExternalPlugin && pm.plugin == ExternalPlugin {
				return nil, 7023
			}
		}
	}
	if in.PluginName != ExternalPlugin && in.PluginName != HostedPlugin {
		return nil, 7005
	}
	pm := &kbPM{id: idgen.MustNewV7().String(), accountID: accountID, externalKey: in.ExternalKey, plugin: in.PluginName, isDefault: first || isDefault}
	if pm.isDefault {
		for _, x := range t.pms {
			if x.accountID == accountID {
				x.isDefault = false
			}
		}
	}
	t.pms = append(t.pms, pm)
	return pm, 0
}

// serveNodes serves node information and the KPM node commands with the
// admin credentials. INSTALL_PLUGIN adds the plugin STOPPED on every node
// and START_PLUGIN starts it, as the KPM bundle of Kill Bill 0.24.22 does.
func (f *Fake) serveNodes(w http.ResponseWriter, r *http.Request, body []byte) {
	user, pass, ok := r.BasicAuth()
	if !ok || user != "admin" || pass != "pw" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodGet:
		reply(w, http.StatusOK, f.nodes)
	case http.MethodPost:
		var cmd killbill.NodeCommand
		if err := json.Unmarshal(body, &cmd); err != nil {
			kbError(w, http.StatusBadRequest, 2)
			return
		}
		props := map[string]string{}
		for _, p := range cmd.NodeCommandProperties {
			props[p.Key] = p.Value
		}
		for i := range f.nodes {
			n := &f.nodes[i]
			switch cmd.NodeCommandType {
			case killbill.NodeCommandInstallPlugin:
				if u, err := url.Parse(props["pluginUri"]); err != nil || u.Scheme != "https" {
					kbError(w, http.StatusBadRequest, 2)
					return
				}
				n.PluginsInfo = append(n.PluginsInfo, killbill.PluginInfo{PluginKey: props["pluginKey"], PluginName: props["pluginKey"] + "-plugin",
					Version: props["pluginVersion"], State: "STOPPED"})
			case killbill.NodeCommandStartPlugin:
				for j := range n.PluginsInfo {
					if n.PluginsInfo[j].PluginKey == props["pluginKey"] && n.PluginsInfo[j].Version == props["pluginVersion"] {
						n.PluginsInfo[j].State = "RUNNING"
					}
				}
			}
		}
		sort.SliceStable(f.nodes, func(i, j int) bool { return f.nodes[i].NodeName < f.nodes[j].NodeName })
		w.WriteHeader(http.StatusAccepted)
	default:
		http.NotFound(w, r)
	}
}

// recordUsage records usage as Kill Bill 0.24.22 does: an unknown
// subscription is 404, a tracking ID the subscription already has is 400
// with error code 50000 and records nothing, and the unit, the amount and
// the subscription's state are not checked. f.mu is held.
func (f *Fake) recordUsage(w http.ResponseWriter, t *kbTenant, body []byte) {
	var in killbill.SubscriptionUsageRecord
	if err := json.Unmarshal(body, &in); err != nil {
		kbError(w, http.StatusBadRequest, 2)
		return
	}
	found := false
	for _, s := range t.subs {
		found = found || s.id == in.SubscriptionID
	}
	if !found {
		reply(w, http.StatusNotFound, map[string]any{"code": nil, "message": "Object doesn't exist"})
		return
	}
	if in.TrackingID != "" {
		for _, u := range t.usages {
			if u.subscriptionID == in.SubscriptionID && u.trackingID == in.TrackingID {
				kbError(w, http.StatusBadRequest, 50000)
				return
			}
		}
	}
	var add []kbUsage
	for _, unit := range in.UnitUsageRecords {
		for _, rec := range unit.UsageRecords {
			micro, ok := parseMicro(rec.Amount)
			if !ok {
				kbError(w, http.StatusBadRequest, 2)
				return
			}
			add = append(add, kbUsage{subscriptionID: in.SubscriptionID, trackingID: in.TrackingID, unit: unit.UnitType,
				date: rec.RecordDate, amountMicro: micro})
		}
	}
	t.usages = append(t.usages, add...)
	w.WriteHeader(http.StatusCreated)
}

// rolledUpUsage sums the usage of a subscription per unit from startDate,
// inclusive, to endDate, exclusive. f.mu is held.
func (f *Fake) rolledUpUsage(w http.ResponseWriter, t *kbTenant, subscriptionID string, q url.Values) {
	start, end := q.Get("startDate"), q.Get("endDate")
	if start == "" || end == "" {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	found := false
	for _, s := range t.subs {
		found = found || s.id == subscriptionID
	}
	if !found {
		reply(w, http.StatusNotFound, map[string]any{"code": nil, "message": "Object doesn't exist"})
		return
	}
	totals := map[string]int64{}
	var order []string
	for _, u := range t.usages {
		day := u.date
		if len(day) > 10 {
			day = day[:10]
		}
		if u.subscriptionID != subscriptionID || day < start || day >= end {
			continue
		}
		if _, ok := totals[u.unit]; !ok {
			order = append(order, u.unit)
		}
		totals[u.unit] += u.amountMicro
	}
	out := killbill.RolledUpUsage{SubscriptionID: subscriptionID, StartDate: start + "T00:00:00.000Z", EndDate: end + "T00:00:00.000Z",
		RolledUpUnits: []killbill.RolledUpUnit{}}
	for _, unit := range order {
		v := totals[unit]
		out.RolledUpUnits = append(out.RolledUpUnits, killbill.RolledUpUnit{UnitType: unit, Amount: json.Number(fmt.Sprintf("%d.%06d000", v/1_000_000, v%1_000_000))})
	}
	reply(w, http.StatusOK, out)
}

// parseMicro reads a non-negative decimal with at most six digits after
// the point as millionths. Negative amounts are kept, as Kill Bill keeps
// them.
func parseMicro(v json.Number) (int64, bool) {
	s := string(v)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	whole, frac, _ := strings.Cut(s, ".")
	frac = strings.TrimRight(frac, "0")
	if len(frac) > 6 {
		return 0, false
	}
	frac += strings.Repeat("0", 6-len(frac))
	w, err1 := strconv.ParseInt(whole, 10, 64)
	f, err2 := strconv.ParseInt(frac, 10, 64)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	m := w*1_000_000 + f
	if neg {
		m = -m
	}
	return m, true
}

// FakeUsage is a usage record the fake holds.
type FakeUsage struct {
	SubscriptionID, TrackingID, Unit, Date string
	AmountMicro                            int64
}

// UsageRecords returns the usage records of a Kill Bill tenant, oldest
// first.
func (f *Fake) UsageRecords(apiKey string) []FakeUsage {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []FakeUsage
	for _, u := range f.billing[apiKey].usages {
		out = append(out, FakeUsage{SubscriptionID: u.subscriptionID, TrackingID: u.trackingID, Unit: u.unit, Date: u.date, AmountMicro: u.amountMicro})
	}
	return out
}

// AddExternalCharge writes an external charge to an account behind Core's
// back, paid from the account credit first.
func (f *Fake) AddExternalCharge(apiKey, accountID string, amountMinor int64, description string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.billing[apiKey]
	used := min(amountMinor, t.cba[accountID])
	list := []*kbItem{{typ: "EXTERNAL_CHARGE", amount: amountMinor, description: description}}
	if used > 0 {
		list = append(list, &kbItem{typ: "CBA_ADJ", amount: -used})
	}
	t.addInvoice(accountID, "CAD", list)
	t.cba[accountID] -= used
}

// AccountBlock adds an account blocking state behind Core's back.
func (f *Fake) AccountBlock(apiKey, accountID string, s killbill.BlockingState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a := f.account(apiKey, accountID); a != nil {
		a.Blocking = append(a.Blocking, s)
	}
}

// dryRun answers a dry-run invoice with the usage of the account's active
// subscriptions priced from the catalogue, the first tier of the phase,
// one block price per started block. The fake never invoices usage, so all
// of it is unbilled. Recurring charges are left out. Nothing to invoice is
// 204, as Kill Bill 0.24.22 answers. f.mu is held.
func (f *Fake) dryRun(w http.ResponseWriter, t *kbTenant, accountID string) {
	if f.dryRunDown {
		reply(w, http.StatusInternalServerError, map[string]any{"code": nil, "message": "dry run failed"})
		return
	}
	var total int64
	var items []*kbItem
	for _, s := range t.subs {
		if s.accountID != accountID || s.state != "ACTIVE" {
			continue
		}
		_, plan, ok := t.plan(s.plan)
		if !ok {
			continue
		}
		sums := map[string]int64{}
		for _, u := range t.usages {
			if u.subscriptionID == s.id && !u.invoiced {
				sums[u.unit] += u.amountMicro
			}
		}
		for _, ph := range plan.Phases {
			if ph.Type != s.phase {
				continue
			}
			for _, us := range ph.Usages {
				for _, tier := range us.Tiers {
					for _, b := range tier.Blocks {
						q := sums[b.Unit]
						size, ok1 := parseMicro(json.Number(b.Size))
						price, ok2 := parseMoney(b.Prices[0].Value)
						if q <= 0 || !ok1 || !ok2 || size <= 0 {
							continue
						}
						amount := (q + size - 1) / size * price
						total += amount
						items = append(items, &kbItem{typ: "USAGE", amount: amount, subscriptionID: s.id, plan: s.plan})
					}
				}
			}
		}
	}
	if total == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	in := &kbInvoice{id: idgen.MustNewV7().String(), accountID: accountID, currency: "CAD"}
	for _, it := range items {
		it.id, it.invoiceID = idgen.MustNewV7().String(), in.id
		in.items = append(in.items, it)
	}
	reply(w, http.StatusOK, in.json(true))
}

// Charges counts the account's external charges whose description starts
// with prefix.
func (f *Fake) Charges(apiKey, accountID, prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, in := range f.billing[apiKey].invoices {
		for _, it := range in.items {
			if in.accountID == accountID && it.typ == "EXTERNAL_CHARGE" && strings.HasPrefix(it.description, prefix) {
				n++
			}
		}
	}
	return n
}

// DefaultSubscriptionStart is the start date the fake gives a new
// subscription.
const DefaultSubscriptionStart = "2026-01-01T00:00:00.000Z"

// SetSubscriptionStart moves the start of a subscription, as a
// subscription created with a future start date has.
func (f *Fake) SetSubscriptionStart(apiKey, subscriptionID string, start time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.billing[apiKey].subs {
		if s.id == subscriptionID {
			s.start = start.UTC().Format("2006-01-02T15:04:05.000Z")
		}
	}
}

// InvoiceUsage bills the usage records of an account's subscriptions dated
// from from, inclusive, to to, exclusive, that no invoice bills yet, as
// Kill Bill's invoice run at the end of a usage period does: one USAGE item
// per subscription and unit, priced from the first tier of the catalogue,
// with the period as its start and end dates, and the tracking IDs of the
// records on the invoice. It returns the invoice ID, or "" when there was
// nothing to bill.
func (f *Fake) InvoiceUsage(apiKey, accountID string, from, to time.Time) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.billing[apiKey]
	var items []*kbItem
	var tracking []string
	seen := map[string]bool{}
	for _, s := range t.subs {
		if s.accountID != accountID {
			continue
		}
		_, plan, ok := t.plan(s.plan)
		if !ok {
			continue
		}
		sums := map[string]int64{}
		for i := range t.usages {
			u := &t.usages[i]
			day := u.date
			if len(day) > 10 {
				day = day[:10]
			}
			if u.subscriptionID != s.id || u.invoiced || day < from.Format(time.DateOnly) || day >= to.Format(time.DateOnly) {
				continue
			}
			u.invoiced = true
			sums[u.unit] += u.amountMicro
			if u.trackingID != "" && !seen[u.trackingID] {
				seen[u.trackingID] = true
				tracking = append(tracking, u.trackingID)
			}
		}
		for _, ph := range plan.Phases {
			if ph.Type != s.phase {
				continue
			}
			for _, us := range ph.Usages {
				for _, tier := range us.Tiers {
					for _, b := range tier.Blocks {
						q, ok := sums[b.Unit]
						if !ok {
							continue
						}
						size, _ := parseMicro(json.Number(b.Size))
						price, _ := parseMoney(b.Prices[0].Value)
						var amount int64
						if size > 0 {
							amount = (q + size - 1) / size * price
						}
						items = append(items, &kbItem{typ: "USAGE", amount: amount, subscriptionID: s.id, plan: s.plan,
							start: from.Format(time.DateOnly), end: to.Format(time.DateOnly)})
						delete(sums, b.Unit)
					}
				}
			}
		}
	}
	if len(items) == 0 {
		return ""
	}
	in := t.addInvoice(accountID, "CAD", items)
	in.trackingIDs = tracking
	return in.id
}

// CancelDay is the day the fake takes to be today for a cancel. A cancel with
// an immediate policy is dated this day, and so is a cancel with
// END_OF_TERM, whose term the fake does not model.
const CancelDay = "2026-11-03"

func (f *Fake) subscriptionOfAnotherTenant(apiKey, id string) bool {
	for key, t := range f.billing {
		if key == apiKey {
			continue
		}
		for _, s := range t.subs {
			if s.id == id {
				return true
			}
		}
	}
	return false
}

// cancelSubscription models DELETE /1.0/kb/subscriptions/{id} of Kill Bill
// 0.24.22 as the real stack answered it:
//   - a subscription that is CANCELLED is refused with HTTP 500 and error
//     code 1031;
//   - an entitlementPolicy or billingPolicy cancels by policy: IMMEDIATE ends
//     the subscription at once and END_OF_TERM schedules the cancel, which can
//     be repeated and can still be made immediate;
//   - without a policy, a requestedDate before today is refused with 400 and
//     code 1001, today ends the subscription and a later day schedules the
//     cancel; on a scheduled cancel the same day is accepted again, an earlier
//     day moves it earlier and a later day is refused like a cancelled
//     subscription, with code 1031 and the state PENDING CANCELLED;
//   - without either, the cancel is immediate;
//   - cancelling a base subscription cancels its add-ons with it.
func (f *Fake) cancelSubscription(w http.ResponseWriter, t *kbTenant, s *kbSub, q url.Values) {
	if s.state == "CANCELLED" {
		kbException(w, http.StatusInternalServerError, "org.killbill.billing.entitlement.api.EntitlementApiException", 1031,
			"Subscription "+s.id+" is in state CANCELLED: Failed to cancel")
		return
	}
	policy := q.Get("entitlementPolicy")
	if policy == "" {
		policy = q.Get("billingPolicy")
	}
	date := q.Get("requestedDate")
	switch {
	case policy != "":
		if policy != killbill.PolicyImmediate && policy != killbill.PolicyEndOfTerm {
			http.Error(w, "404 page not found", http.StatusNotFound)
			return
		}
		if policy == killbill.PolicyImmediate {
			f.endSubscription(t, s, CancelDay)
		} else {
			f.scheduleCancel(t, s, CancelDay)
		}
	case date != "":
		switch {
		case date < CancelDay:
			kbException(w, http.StatusBadRequest, "org.killbill.billing.entitlement.api.EntitlementApiException", 1001,
				"Requested date "+date+" is not allowed to be prior to the previous transition")
			return
		case s.scheduled && date > s.cancelled:
			kbException(w, http.StatusInternalServerError, "org.killbill.billing.entitlement.api.EntitlementApiException", 1031,
				"Subscription "+s.id+" is in state PENDING CANCELLED: Failed to cancel")
			return
		case s.scheduled && date == s.cancelled:
			// Already scheduled for that day: nothing changes.
		case date == CancelDay:
			f.endSubscription(t, s, date)
		default:
			f.scheduleCancel(t, s, date)
		}
	default:
		f.endSubscription(t, s, CancelDay)
	}
	w.WriteHeader(http.StatusNoContent)
}

// endSubscription cancels s and, for a base subscription, its add-ons.
func (f *Fake) endSubscription(t *kbTenant, s *kbSub, day string) {
	for _, x := range f.cancelGroup(t, s) {
		x.state, x.cancelled, x.scheduled = "CANCELLED", day, false
	}
}

// scheduleCancel schedules the cancel of s and of its add-ons for day.
func (f *Fake) scheduleCancel(t *kbTenant, s *kbSub, day string) {
	for _, x := range f.cancelGroup(t, s) {
		x.cancelled, x.scheduled = day, true
	}
}

func (f *Fake) cancelGroup(t *kbTenant, s *kbSub) []*kbSub {
	group := []*kbSub{s}
	if s.category != "" {
		return group
	}
	for _, x := range t.subs {
		if x != s && x.bundleID == s.bundleID && x.category == "ADD_ON" && x.state != "CANCELLED" {
			group = append(group, x)
		}
	}
	return group
}
