//go:build integration

package tenant_test

import (
	"context"
	"testing"

	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/pkg/outbox"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

func TestBillingRecipientAlreadyMatchingIsNoop(t *testing.T) {
	e := newEnv(t)
	p := e.person("same@example.test")
	tn := e.mustOrg(p, "Matching")
	before := len(e.fake.Writes())
	if r := e.recipient(tn, p.Email, tenant.BillingRecipientReady); r.Changed || len(e.fake.Writes()) != before {
		t.Fatal("matching recipient issued a write")
	}
}

func TestBillingRecipientDeferredMirrorRemainsUnpublished(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.person("retry@example.test")
	tn := e.mustOrg(p, "Deferred mirror")
	if _, err := e.db.Pool.Exec(ctx, `UPDATE tenant_billing_recipients SET next_attempt_at=now()+interval '5 minutes' WHERE tenant_id=$1`, tn.ID); err != nil {
		t.Fatal(err)
	}
	reader := outbox.NewPostgresReader(e.db.Pool).WithEventTypes(tenant.MirrorEventType)
	relay := outbox.NewRelay("recipient-test", reader, tenant.MirrorHandler(e.db.Pool, e.eng), 50, 0, nil)
	if n, err := relay.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("deferred relay = %d %v", n, err)
	}
	var pending int
	if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE event_type=$1 AND tenant_id=$2 AND published_at IS NULL`, tenant.MirrorEventType, tn.ID).Scan(&pending); err != nil || pending == 0 {
		t.Fatalf("deferred event disappeared: %d %v", pending, err)
	}
	e.recipientDue(tn)
	if _, err := e.db.Pool.Exec(ctx, `UPDATE outbox SET next_attempt_at=now() WHERE event_type=$1 AND tenant_id=$2`, tenant.MirrorEventType, tn.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := relay.RunOnce(ctx); err != nil || n != pending {
		t.Fatalf("ready relay = %d want=%d %v", n, pending, err)
	}
}

func TestBillingRecipientLateStaleWriteAndCrossBrandIsolation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.person("first@example.test")
	tn := e.mustOrg(p, "Late stale")
	e.recipient(tn, p.Email, tenant.BillingRecipientReady)
	other := e.fake.InsertBrand(t, e.db.Pool, "zs-other-recipient")
	before := len(e.fake.Writes())
	if _, err := tenant.ReconcileBillingRecipient(ctx, e.db.Pool, e.eng, scope.Scope{Brand: other.ID, Tenant: tn.ID}, "wrong-brand"); code(err) != "tenant_not_found" || len(e.fake.Writes()) != before {
		t.Fatalf("wrong brand reconciled: %v", err)
	}
	if _, err := e.db.Pool.Exec(ctx, `UPDATE identities SET email='current@example.test' WHERE id=$1`, p.Identity.ID); err != nil {
		t.Fatal(err)
	}
	e.recipient(tn, "current@example.test", tenant.BillingRecipientReady)
	creds := killbill.TenantCredentials{APIKey: e.brand.Slug, APISecret: "kb-secret-" + e.brand.Slug}
	// An earlier accepted request can arrive after a successful newer pass.
	if err := e.eng.KillBill.SetAccountEmail(ctx, creds, killbill.CallContext{CreatedBy: "delayed-test-request"}, tn.KillBillAccountID, p.Email); err != nil {
		t.Fatal(err)
	}
	e.recipient(tn, "current@example.test", tenant.BillingRecipientReady)
}
