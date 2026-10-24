//go:build integration

package metering_test

import (
	"context"
	"strings"
	"testing"

	"github.com/zsoftly/zs-core/internal/killbill"

	"github.com/zsoftly/zs-core/internal/enginetest"
	"github.com/zsoftly/zs-core/pkg/billing"
	"github.com/zsoftly/zs-core/pkg/metering"
	"github.com/zsoftly/zs-core/pkg/scope"
)

// Sending a rollup reads from Kill Bill only what it has to: the catalogue
// and the account's subscriptions. The subscription it just confirmed is
// not read a second time, and the Kill Bill tenant is looked up once per
// pass, not once per rollup.
func TestSendReadsKillBillOnlyWhatItNeeds(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.brand("zs-reads")
	tn := e.tenant(b, "Reads Co")
	e.subscribe(b, tn, enginetest.PlanMetered)
	const rollups = 3
	for d := 0; d < rollups; d++ {
		at := e.clock.Now().AddDate(0, 0, -d)
		if _, _, err := e.record(tn.Scope(), metering.Event{Meter: "api-calls", EventID: "rd-" + string(rune('a'+d)), QuantityMicro: 1_000_000, OccurredAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	before := len(e.fake.Reads())
	report, err := e.svc.Forward(ctx)
	if err != nil || report.Forwarded != rollups || report.Failed != 0 || report.Unconfirmed != 0 {
		t.Fatalf("forward = %+v %v", report, err)
	}
	reads := e.fake.Reads()[before:]
	var tenants, subscriptions int
	for _, p := range reads {
		switch {
		case strings.HasPrefix(p, "/1.0/kb/tenants"):
			tenants++
		case strings.HasPrefix(p, "/1.0/kb/subscriptions/"):
			subscriptions++
		}
	}
	t.Logf("KILL_BILL_READS rollups=%d total=%d tenant_lookups=%d subscription_reads=%d per_rollup=%.2f", rollups, len(reads), tenants, subscriptions, float64(len(reads))/rollups)
	if tenants != 1 || subscriptions != 0 || len(reads) != 2*rollups+1 {
		t.Fatalf("Kill Bill reads = %d (tenant lookups %d, subscription reads %d), want %d with 1 tenant lookup and 0 subscription reads: %v",
			len(reads), tenants, subscriptions, 2*rollups+1, reads)
	}
}

func accountOf(t *testing.T, e *env, scope scope.Scope) *billing.TenantAccount {
	t.Helper()
	acct, err := e.svc.Billing.TenantAccount(context.Background(), e.db.Pool, scope)
	if err != nil {
		t.Fatal(err)
	}
	return acct
}

// The target is built only from a subscription of the account itself.
func TestUsageTargetOfRefusesASubscriptionOfAnotherAccount(t *testing.T) {
	e := newEnv(t)
	b := e.brand("zs-ofacct")
	tn := e.tenant(b, "OfAcct Co")
	acct := accountOf(t, e, tn.Scope())
	own := killbill.Subscription{AccountID: strings.ToUpper(acct.ID()), SubscriptionID: "s-1"}
	if target, err := acct.UsageTargetOf(b.ID.String(), own); err != nil || !strings.EqualFold(target.AccountID, acct.ID()) {
		t.Fatalf("own subscription = %+v %v", target, err)
	}
	for name, sub := range map[string]killbill.Subscription{
		"another account": {AccountID: "00000000-0000-7000-8000-000000000001", SubscriptionID: "s-2"},
		"no account":      {SubscriptionID: "s-3"},
	} {
		if target, err := acct.UsageTargetOf(b.ID.String(), sub); err == nil {
			t.Fatalf("%s accepted: %+v", name, target)
		}
	}
}

// The Kill Bill tenant is cached by the brand's API key, and an account whose
// stored tenant is not the one Kill Bill answers for is refused.
func TestTenantIDCacheFollowsTheBrandKeyAndRefusesAStoredMismatch(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.brand("zs-keya")
	other := e.brand("zs-keyb")
	tn := e.tenant(a, "KeyA Co")
	cache := metering.NewTenantIDs()
	if id, err := cache.Get(ctx, accountOf(t, e, tn.Scope())); err != nil || id != a.ID.String() {
		t.Fatalf("first lookup = %q %v, want %s", id, err, a.ID)
	}
	// The brand moves to the other Kill Bill tenant, key and stored ID together.
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := e.db.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE brands SET killbill_api_key = 'zs-keyb-parked', killbill_tenant_id = gen_random_uuid() WHERE id = $1`, other.ID)
	exec(`UPDATE brands SET killbill_api_key = $2, killbill_tenant_id = $3 WHERE id = $1`, a.ID, other.Slug, other.ID)
	if id, err := cache.Get(ctx, accountOf(t, e, tn.Scope())); err != nil || id != other.ID.String() {
		t.Fatalf("lookup after the key changed = %q %v, want %s, not the cached %s", id, err, other.ID, a.ID)
	}
	// The stored tenant no longer matches what the key resolves to.
	exec(`UPDATE brands SET killbill_tenant_id = $2 WHERE id = $1`, a.ID, a.ID)
	if id, err := cache.Get(ctx, accountOf(t, e, tn.Scope())); err == nil {
		t.Fatalf("a stored tenant that differs from the live one was accepted: %q", id)
	}
}

// A brand whose stored tenant is not the live one holds its rollups: nothing
// is bound or sent, and the reason is usage_target_unconfirmed.
func TestStoredTenantMismatchHoldsTheRollup(t *testing.T) {
	e, slug, tenantID := setupOneRollup(t, "zs-heldtenant")
	ctx := context.Background()
	if _, err := e.db.Pool.Exec(ctx, `UPDATE brands SET killbill_tenant_id = gen_random_uuid() WHERE slug = $1`, slug); err != nil {
		t.Fatal(err)
	}
	report, err := e.svc.Forward(ctx)
	if err != nil || report.Failed != 1 || report.Forwarded != 0 || report.Unconfirmed != 0 {
		t.Fatalf("forward = %+v %v", report, err)
	}
	var lastErr string
	var bound bool
	if err := e.db.Pool.QueryRow(ctx, `SELECT last_error, target_endpoint_fingerprint IS NOT NULL FROM usage_rollups WHERE tenant_id = $1`, tenantID).Scan(&lastErr, &bound); err != nil {
		t.Fatal(err)
	}
	if lastErr != "usage_target_unconfirmed" || bound || usagePosts(e) != 0 {
		t.Fatalf("last_error = %q bound = %v posts = %d", lastErr, bound, usagePosts(e))
	}
}
