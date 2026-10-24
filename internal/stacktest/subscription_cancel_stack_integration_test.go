//go:build integration

package stacktest

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/pkg/idgen"
)

// TestSubscriptionCancelOnTheStack checks, on a real Kill Bill, the behaviour
// the fake of internal/enginetest copies and CancelSubscriptionOnce
// classifies: bundles with categories, a cancel by policy and by requested
// date, a repeat, the cascade to an add-on, and a subscription of another
// Kill Bill tenant.
func TestSubscriptionCancelOnTheStack(t *testing.T) {
	url, password := os.Getenv("ZS_TEST_KILLBILL_URL"), os.Getenv("ZS_TEST_KILLBILL_ADMIN_PASSWORD")
	if url == "" || password == "" {
		t.Skip("ZS_TEST_KILLBILL_URL and ZS_TEST_KILLBILL_ADMIN_PASSWORD are not set")
	}
	ctx := context.Background()
	kb, err := killbill.New(killbill.Config{URL: url, AdminUser: "admin", AdminPassword: password})
	if err != nil {
		t.Fatal(err)
	}
	cc := killbill.CallContext{CreatedBy: "stack-test", Reason: "cancel", Comment: "stack test"}
	newTenant := func(prefix string) killbill.TenantCredentials {
		key := prefix + "-" + idgen.MustNewV7().String()
		creds := killbill.TenantCredentials{APIKey: key, APISecret: "secret-" + key}
		if _, err := kb.CreateTenant(ctx, cc, creds, key); err != nil {
			t.Fatal(err)
		}
		return creds
	}
	creds := newTenant("cancel")
	other := newTenant("cancel-other")
	simplePlan(t, url, password, creds, `{"planId":"base-monthly","productName":"Base","productCategory":"BASE","currency":"USD","amount":10,"billingPeriod":"MONTHLY","trialLength":0,"trialTimeUnit":"DAYS","availableBaseProducts":[]}`)
	simplePlan(t, url, password, creds, `{"planId":"addon-monthly","productName":"AddOn","productCategory":"ADD_ON","currency":"USD","amount":5,"billingPeriod":"MONTHLY","trialLength":0,"trialTimeUnit":"DAYS","availableBaseProducts":["Base"]}`)

	newAccount := func(name string) string {
		id, err := kb.CreateAccount(ctx, creds, cc, killbill.Account{ExternalKey: name + "-" + idgen.MustNewV7().String(), Name: name, Currency: "USD", Email: name + "@example.test"})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	// newBase makes a base subscription and waits until Kill Bill has invoiced
	// it, which sets chargedThroughDate a few seconds after creation.
	newBase := func(account string, billed bool) killbill.Subscription {
		id, err := kb.CreateSubscription(ctx, creds, cc, account, "base-monthly", "base-"+idgen.MustNewV7().String())
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(60 * time.Second)
		for {
			s, err := kb.Subscription(ctx, creds, id)
			if err != nil {
				t.Fatal(err)
			}
			if !billed || s.ChargedThroughDate != "" {
				return s
			}
			if time.Now().After(deadline) {
				t.Fatalf("subscription %s was never invoiced: %+v", id, s)
			}
			time.Sleep(time.Second)
		}
	}
	today := time.Now().UTC().Format(time.DateOnly)
	day := func(s killbill.Subscription) string {
		d, err := killbill.CancelEffectiveDate(s)
		if err != nil {
			t.Fatalf("%+v: %v", s, err)
		}
		return d
	}
	read := func(id string) killbill.Subscription {
		s, err := kb.Subscription(ctx, creds, id)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	cancel := func(account, id string, how killbill.CancelHow) killbill.CancelResult {
		res, _ := kb.CancelSubscriptionOnce(ctx, creds, cc, account, id, how)
		return res
	}

	// Bundles carry the category, the state and no cancelled date.
	account := newAccount("bundle")
	base := newBase(account, false)
	addOnID, err := kb.CreateAddOn(ctx, creds, cc, account, base.BundleID, "addon-monthly", "addon-"+idgen.MustNewV7().String())
	if err != nil {
		t.Fatal(err)
	}
	bundles, err := kb.AccountBundles(ctx, creds, account)
	if err != nil || len(bundles) != 1 || len(bundles[0].Subscriptions) != 2 {
		t.Fatalf("bundles = %+v %v", bundles, err)
	}
	categories := map[string]string{}
	for _, s := range bundles[0].Subscriptions {
		categories[s.SubscriptionID] = s.ProductCategory
		if s.State != "ACTIVE" || s.CancelledDate != "" || s.StartDate == "" || s.PlanName == "" {
			t.Fatalf("new subscription = %+v", s)
		}
	}
	if categories[base.SubscriptionID] != "BASE" || categories[addOnID] != "ADD_ON" {
		t.Fatalf("categories = %v", categories)
	}

	// A subscription of another Kill Bill tenant is a server error, which the
	// client keeps from reaching the cancel.
	_, err = kb.Subscription(ctx, other, base.SubscriptionID)
	var unavailable *killbill.Unavailable
	if !errors.As(err, &unavailable) || unavailable.Status != http.StatusInternalServerError {
		t.Fatalf("another tenant's read = %v, want a 500", err)
	}
	if res, err := kb.CancelSubscriptionOnce(ctx, other, cc, account, base.SubscriptionID, killbill.CancelHow{Policy: killbill.PolicyImmediate}); err == nil || res.Outcome != killbill.NotCancelled {
		t.Fatalf("another tenant's cancel = %+v %v", res, err)
	}
	if s := read(base.SubscriptionID); s.State != "ACTIVE" {
		t.Fatalf("a refused cancel changed the subscription: %+v", s)
	}

	// An immediate cancel of the base cancels the add-on with the same day.
	if res := cancel(account, base.SubscriptionID, killbill.CancelHow{Policy: killbill.PolicyImmediate}); res.Outcome != killbill.Cancelled || res.Status != http.StatusNoContent {
		t.Fatalf("immediate cancel = %+v", res)
	}
	b, a := read(base.SubscriptionID), read(addOnID)
	if b.State != "CANCELLED" || a.State != "CANCELLED" || day(b) != today || day(a) != today {
		t.Fatalf("after the immediate cancel base = %+v add-on = %+v (today %s)", b, a, today)
	}
	// A repeat, and an add-on whose base is cancelled, are state refusals.
	for _, id := range []string{base.SubscriptionID, addOnID} {
		if res := cancel(account, id, killbill.CancelHow{Policy: killbill.PolicyImmediate}); res.Outcome != killbill.CancelRefused || res.Code != 1031 || res.Status != http.StatusInternalServerError {
			t.Fatalf("repeat cancel of %s = %+v", id, res)
		}
	}

	// END_OF_TERM on an invoiced subscription leaves it active until the term
	// ends, can be repeated, and can still be made immediate.
	account = newAccount("endofterm")
	billed := newBase(account, true)
	for i := 0; i < 2; i++ {
		if res := cancel(account, billed.SubscriptionID, killbill.CancelHow{Policy: killbill.PolicyEndOfTerm}); res.Outcome != killbill.Cancelled {
			t.Fatalf("end of term cancel %d = %+v", i, res)
		}
	}
	s := read(billed.SubscriptionID)
	if s.State != "ACTIVE" || day(s) != s.ChargedThroughDate || day(s) <= today {
		t.Fatalf("scheduled cancel = %+v, want ACTIVE and the term end after %s", s, today)
	}
	if res := cancel(account, billed.SubscriptionID, killbill.CancelHow{Policy: killbill.PolicyImmediate}); res.Outcome != killbill.Cancelled {
		t.Fatalf("immediate cancel of a scheduled one = %+v", res)
	}
	if s := read(billed.SubscriptionID); s.State != "CANCELLED" || day(s) != today {
		t.Fatalf("after the immediate cancel = %+v", s)
	}

	// A requested day alone ends the entitlement on that day and, with the
	// flag the client sends, the billing too. The same day again is accepted,
	// an earlier day moves the cancel, a later day is refused as already
	// pending (code 1031), and a day in the past is refused with code 1001.
	account = newAccount("requested")
	dated := newBase(account, true)
	later := time.Now().UTC().AddDate(0, 0, 10).Format(time.DateOnly)
	if res := cancel(account, dated.SubscriptionID, killbill.CancelHow{RequestedDate: later}); res.Outcome != killbill.Cancelled {
		t.Fatalf("requested date cancel = %+v", res)
	}
	s = read(dated.SubscriptionID)
	if s.State != "ACTIVE" || day(s) != later {
		t.Fatalf("dated cancel = %+v, want ACTIVE until %s", s, later)
	}
	if res := cancel(account, dated.SubscriptionID, killbill.CancelHow{RequestedDate: later}); res.Outcome != killbill.Cancelled {
		t.Fatalf("repeated dated cancel = %+v", res)
	}
	if res := cancel(account, dated.SubscriptionID, killbill.CancelHow{RequestedDate: time.Now().UTC().AddDate(0, 0, 12).Format(time.DateOnly)}); res.Outcome != killbill.CancelRefused || res.Code != 1031 {
		t.Fatalf("later dated cancel = %+v", res)
	}
	earlier := time.Now().UTC().AddDate(0, 0, 5).Format(time.DateOnly)
	if res := cancel(account, dated.SubscriptionID, killbill.CancelHow{RequestedDate: earlier}); res.Outcome != killbill.Cancelled {
		t.Fatalf("earlier dated cancel = %+v", res)
	}
	if s = read(dated.SubscriptionID); s.State != "ACTIVE" || day(s) != earlier {
		t.Fatalf("moved cancel = %+v, want ACTIVE until %s", s, earlier)
	}
	past := newBase(newAccount("past"), true)
	pastAccount := past.AccountID
	if res := cancel(pastAccount, past.SubscriptionID, killbill.CancelHow{RequestedDate: time.Now().UTC().AddDate(0, 0, -3).Format(time.DateOnly)}); res.Outcome != killbill.NotCancelled || res.Code != 1001 {
		t.Fatalf("past date cancel = %+v", res)
	}
}

// simplePlan adds a plan through Kill Bill's simple plan API, which the Core
// client has no use for.
func simplePlan(t *testing.T, url, password string, creds killbill.TenantCredentials, body string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+"/1.0/kb/catalog/simplePlan", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("admin", password)
	creds.Apply(req.Header)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(killbill.HeaderCreatedBy, "stack-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("simple plan = %d", resp.StatusCode)
	}
}
