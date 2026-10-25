//go:build integration

package tenant_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

// raceKB is a Kill Bill that creates the invoice of a cancel after the dry run
// of the closure invoice and before its trigger. The trigger then finds
// nothing to invoice and answers the bare 404, although the dry run showed
// something.
type raceKB struct {
	mu sync.Mutex
	// dry is how the successive dry runs answer, as for answerPreflight: 0
	// something, 1 nothing, 2 unavailable. The last answer repeats.
	dry []int32
	// triggerStatus is how the trigger answers: 404 bare, or another status.
	triggerStatus int
	// invoices are the invoices of the account, as the invoice list and an
	// invoice read answer them.
	invoices []map[string]any
	// subscriptions are the subscriptions of the account, all CANCELLED.
	subscriptions []string
	dryRuns       int
	posts         int
	listings      int
}

// cancelInvoice is an invoice of the account as Kill Bill lists it: dated
// date, committed, with one item of itemType for subscription.
func cancelInvoice(id, accountID, status, date, targetDate, itemType, subscription string) map[string]any {
	return map[string]any{
		"invoiceId": id, "accountId": accountID, "status": status, "currency": "CAD",
		"invoiceDate": date, "targetDate": targetDate,
		"items": []map[string]any{{"invoiceId": id, "accountId": accountID, "itemType": itemType, "subscriptionId": subscription}},
	}
}

func (k *raceKB) counts() (dryRuns, posts, listings int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.dryRuns, k.posts, k.listings
}

// serve installs the Kill Bill on e for the tenant.
func (k *raceKB) serve(t *testing.T, e *env, tn tenant.Tenant) {
	t.Helper()
	if k.triggerStatus == 0 {
		k.triggerStatus = http.StatusNotFound
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k.mu.Lock()
		defer k.mu.Unlock()
		account := "/1.0/kb/accounts/" + tn.KillBillAccountID
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/1.0/kb/invoices/dryRun":
			answer := k.dry[min(k.dryRuns, len(k.dry)-1)]
			k.dryRuns++
			answerPreflight(w, r, tn, answer, 0)
		case r.Method == http.MethodPost && r.URL.Path == "/1.0/kb/invoices":
			k.posts++
			if k.triggerStatus == http.StatusNotFound {
				w.Header().Set("Content-Type", "text/html;charset=utf-8")
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte("<!doctype html><html><body><h3>Apache Tomcat/9.0.122</h3></body></html>"))
				return
			}
			http.Error(w, "trigger unavailable", k.triggerStatus)
		case r.Method == http.MethodGet && r.URL.Path == account+"/bundles":
			subs := make([]killbill.Subscription, 0, len(k.subscriptions))
			for _, id := range k.subscriptions {
				subs = append(subs, killbill.Subscription{AccountID: tn.KillBillAccountID, SubscriptionID: id, State: "CANCELLED"})
			}
			writeJSON(w, []killbill.Bundle{{AccountID: tn.KillBillAccountID, BundleID: idgen.MustNewV7().String(), Subscriptions: subs}})
		case r.Method == http.MethodGet && r.URL.Path == account+"/invoices":
			k.listings++
			writeJSON(w, k.invoices)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/1.0/kb/invoices/"):
			for _, in := range k.invoices {
				if r.URL.Path == "/1.0/kb/invoices/"+in["invoiceId"].(string) {
					writeJSON(w, in)
					return
				}
			}
			http.NotFound(w, r)
		default:
			if !answerPreflight(w, r, tn, 1, 0) {
				http.NotFound(w, r)
			}
		}
	}))
	t.Cleanup(server.Close)
	kb, err := killbill.New(killbill.Config{URL: server.URL, AdminUser: "admin", AdminPassword: "pw", HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	e.eng.KillBill = kb
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// closureDays are the day the closure cancelled and its finance target date.
func closureDays(t *testing.T, e *env, tn tenant.Tenant) (cancelDay, targetDate string) {
	t.Helper()
	if err := e.db.Pool.QueryRow(context.Background(), `SELECT (cancellation_completed_at AT TIME ZONE 'UTC')::date::text, finance_target_date::text
FROM tenant_closure_settlements WHERE brand_id = $1 AND tenant_id = $2`, tn.BrandID, tn.ID).Scan(&cancelDay, &targetDate); err != nil {
		t.Fatal(err)
	}
	return cancelDay, targetDate
}

// outcomeKinds counts the outcomes of the closure invoice by kind.
func outcomeKinds(t *testing.T, e *env) map[string]int {
	t.Helper()
	rows, err := e.db.Pool.Query(context.Background(), `SELECT kind, count(*) FROM tenant_closure_invoice_outcomes GROUP BY kind`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var kind string
		var n int
		if err := rows.Scan(&kind, &n); err != nil {
			t.Fatal(err)
		}
		out[kind] = n
	}
	return out
}

func sameKinds(got map[string]int, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for _, kind := range want {
		if got[kind] != 1 {
			return false
		}
	}
	return true
}

// Kill Bill creates the invoice of the cancel after the dry run and before the
// trigger. The trigger answers the bare 404 although the dry run showed
// something. Asked again, the dry run says nothing is left, and the cancel's
// own committed invoice, dated the cancel day, carries usage of a cancelled
// subscription: that invoice is the closure invoice. It is recorded as
// evidenced after the strict readback, and nothing is posted again.
func TestClosureInvoiceExecutionEvidencesTheCancelInvoiceAfterATriggerNotFound(t *testing.T) {
	var m kbMode
	var posts, lookups atomic.Int64
	e, tn := claimedClosure(t, &m, &posts, &lookups)
	ctx := context.Background()
	cancelDay, target := closureDays(t, e, tn)
	subscription, invoice := idgen.MustNewV7().String(), idgen.MustNewV7().String()
	k := &raceKB{dry: []int32{0, 1}, subscriptions: []string{subscription}, invoices: []map[string]any{
		cancelInvoice(invoice, tn.KillBillAccountID, "COMMITTED", cancelDay, target, "USAGE", subscription),
	}}
	k.serve(t, e, tn)
	result, err := tenant.ExecuteClosureInvoiceOnce(ctx, e.db.Pool, e.eng, tn.Scope())
	if err != nil || !result.Evidenced || result.NoInvoice {
		t.Fatalf("execution = %+v %v", result, err)
	}
	if kinds := outcomeKinds(t, e); !sameKinds(kinds, "started", "invoice_id", "evidenced") {
		t.Fatalf("outcomes = %v", kinds)
	}
	var recorded string
	if err := e.db.Pool.QueryRow(ctx, `SELECT invoice_id::text FROM tenant_closure_invoice_outcomes WHERE kind = 'evidenced'`).Scan(&recorded); err != nil || recorded != invoice {
		t.Fatalf("evidenced invoice = %q, %v", recorded, err)
	}
	dryRuns, kbPosts, _ := k.counts()
	if dryRuns != 2 || kbPosts != 1 {
		t.Fatalf("dry runs = %d posts = %d", dryRuns, kbPosts)
	}
	again, err := tenant.ExecuteClosureInvoiceOnce(ctx, e.db.Pool, e.eng, tn.Scope())
	if _, kbPosts, _ = k.counts(); err != nil || !again.Evidenced || kbPosts != 1 {
		t.Fatalf("repeat = %+v %v posts=%d", again, err, kbPosts)
	}
}

// After the trigger answered the bare 404 and the dry run now says nothing is
// left, an account without a matching invoice has nothing to invoice. A
// committed invoice that is not the cancel's own is not evidence: another
// day, not committed, another account, no usage or repair item, or an item of
// a subscription that is not a cancelled one of the tenant.
func TestClosureInvoiceExecutionRecordsNothingToInvoiceWhenNoCancelInvoiceMatches(t *testing.T) {
	foreign := idgen.MustNewV7().String()
	for _, tc := range []struct {
		name    string
		invoice func(account, subscription, cancelDay, target string) map[string]any
	}{
		{name: "no invoice"},
		{name: "dated before the cancel day", invoice: func(account, subscription, cancelDay, target string) map[string]any {
			return cancelInvoice(idgen.MustNewV7().String(), account, "COMMITTED", "2000-01-01", target, "USAGE", subscription)
		}},
		{name: "not committed", invoice: func(account, subscription, cancelDay, target string) map[string]any {
			return cancelInvoice(idgen.MustNewV7().String(), account, "DRAFT", cancelDay, target, "USAGE", subscription)
		}},
		{name: "another account", invoice: func(account, subscription, cancelDay, target string) map[string]any {
			return cancelInvoice(idgen.MustNewV7().String(), idgen.MustNewV7().String(), "COMMITTED", cancelDay, target, "USAGE", subscription)
		}},
		{name: "recurring item only", invoice: func(account, subscription, cancelDay, target string) map[string]any {
			return cancelInvoice(idgen.MustNewV7().String(), account, "COMMITTED", cancelDay, target, "RECURRING", subscription)
		}},
		{name: "item of another subscription", invoice: func(account, subscription, cancelDay, target string) map[string]any {
			return cancelInvoice(idgen.MustNewV7().String(), account, "COMMITTED", cancelDay, target, "USAGE", foreign)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var m kbMode
			var posts, lookups atomic.Int64
			e, tn := claimedClosure(t, &m, &posts, &lookups)
			ctx := context.Background()
			cancelDay, target := closureDays(t, e, tn)
			subscription := idgen.MustNewV7().String()
			k := &raceKB{dry: []int32{0, 1}, subscriptions: []string{subscription}}
			if tc.invoice != nil {
				k.invoices = []map[string]any{tc.invoice(tn.KillBillAccountID, subscription, cancelDay, target)}
			}
			k.serve(t, e, tn)
			result, err := tenant.ExecuteClosureInvoiceOnce(ctx, e.db.Pool, e.eng, tn.Scope())
			if err != nil || !result.NoInvoice || result.Evidenced {
				t.Fatalf("execution = %+v %v", result, err)
			}
			if kinds := outcomeKinds(t, e); !sameKinds(kinds, "started", "no_invoice") {
				t.Fatalf("outcomes = %v", kinds)
			}
			again, err := tenant.ExecuteClosureInvoiceOnce(ctx, e.db.Pool, e.eng, tn.Scope())
			dryRuns, kbPosts, _ := k.counts()
			if err != nil || !again.NoInvoice || kbPosts != 1 || dryRuns != 2 {
				t.Fatalf("repeat = %+v %v dry runs=%d posts=%d", again, err, dryRuns, kbPosts)
			}
		})
	}
}

// Anything short of a clear answer stays an unknown outcome for a person, and
// nothing is posted again: a dry run that still shows something, a dry run
// that fails, a trigger that fails with anything but 404, and two invoices
// that both look like the cancel's.
func TestClosureInvoiceExecutionKeepsAnUnclearTriggerFailureForManualReview(t *testing.T) {
	for _, tc := range []struct {
		name     string
		dry      []int32
		trigger  int
		invoices int
		wantDry  int
	}{
		{name: "dry run still shows something", dry: []int32{0, 0}, invoices: 1, wantDry: 2},
		{name: "dry run fails", dry: []int32{0, 2}, invoices: 1, wantDry: 2},
		{name: "trigger fails with another status", dry: []int32{0, 1}, trigger: http.StatusServiceUnavailable, invoices: 1, wantDry: 1},
		{name: "two invoices look like the cancel's", dry: []int32{0, 1}, invoices: 2, wantDry: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var m kbMode
			var posts, lookups atomic.Int64
			e, tn := claimedClosure(t, &m, &posts, &lookups)
			ctx := context.Background()
			cancelDay, target := closureDays(t, e, tn)
			subscription := idgen.MustNewV7().String()
			k := &raceKB{dry: tc.dry, triggerStatus: tc.trigger, subscriptions: []string{subscription}}
			for range tc.invoices {
				k.invoices = append(k.invoices, cancelInvoice(idgen.MustNewV7().String(), tn.KillBillAccountID, "COMMITTED", cancelDay, target, "USAGE", subscription))
			}
			k.serve(t, e, tn)
			result, err := tenant.ExecuteClosureInvoiceOnce(ctx, e.db.Pool, e.eng, tn.Scope())
			if err == nil || result.Evidenced || result.NoInvoice {
				t.Fatalf("execution = %+v %v", result, err)
			}
			if kinds := outcomeKinds(t, e); !sameKinds(kinds, "started", "post_unknown") {
				t.Fatalf("outcomes = %v", kinds)
			}
			dryRuns, kbPosts, _ := k.counts()
			if dryRuns != tc.wantDry || kbPosts != 1 {
				t.Fatalf("dry runs = %d posts = %d", dryRuns, kbPosts)
			}
			if _, err := tenant.ExecuteClosureInvoiceOnce(ctx, e.db.Pool, e.eng, tn.Scope()); !tenant.IsClosureManualReview(err) {
				t.Fatalf("repeat = %v, want manual review", err)
			}
			if _, kbPosts, _ = k.counts(); kbPosts != 1 {
				t.Fatalf("repeat posted again: %d", kbPosts)
			}
		})
	}
}
