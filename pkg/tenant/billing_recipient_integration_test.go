//go:build integration

package tenant_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

func (e *env) recipient(tn tenant.Tenant, email string, status tenant.BillingRecipientStatus) tenant.BillingRecipientReport {
	e.t.Helper()
	r, err := tenant.ReconcileBillingRecipient(context.Background(), e.db.Pool, e.eng, tn.Scope(), "recipient-test")
	if err != nil || !r.Matches || r.Deferred || r.Status != status {
		e.t.Fatalf("reconcile = %+v %v", r, err)
	}
	a, ok := e.fake.Account(e.brand.Slug, tn.PublicID())
	if !ok || a.Email != email {
		e.t.Fatalf("recipient did not converge to expected owner: found=%v", ok)
	}
	return r
}

func (e *env) promote(tn tenant.Tenant, caller tenant.Caller, member tenant.Membership) {
	e.t.Helper()
	role := tenant.RoleOwner
	if err := e.tx(func(tx pgx.Tx) error {
		_, err := tenant.UpdateMember(context.Background(), tx, tn.Scope(), caller, member.ID, tenant.MemberChange{Role: &role})
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) recipientDue(tn tenant.Tenant) {
	e.t.Helper()
	if _, err := e.db.Pool.Exec(context.Background(), `UPDATE tenant_billing_recipients SET next_attempt_at=now() WHERE brand_id=$1 AND tenant_id=$2`, tn.BrandID, tn.ID); err != nil {
		e.t.Fatal(err)
	}
}

func TestBillingRecipientTransferRemovalAndClearPreserveUnmanagedCC(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a, b, c := e.person("creator@example.test"), e.person("named@example.test"), e.person("retained@example.test")
	tn := e.mustOrg(a, "Recipient lifecycle")
	other := e.mustOrg(a, "Other tenant")
	caller := e.caller(tn.Scope(), a, true)
	mc := e.join(tn.Scope(), caller, c, tenant.RoleAdmin)
	mb := e.join(tn.Scope(), caller, b, tenant.RoleAdmin)
	e.promote(tn, caller, mc)
	if !e.fake.SetAdditionalAccountEmails(e.brand.Slug, tn.PublicID(), []string{"unmanaged@example.test"}) {
		t.Fatal("missing fixture account")
	}
	e.recipient(tn, a.Email, tenant.BillingRecipientReady)
	before, _ := e.fake.Account(e.brand.Slug, tn.PublicID())
	writes := len(e.fake.Writes())
	if r := e.recipient(tn, a.Email, tenant.BillingRecipientReady); r.Changed || len(e.fake.Writes()) != writes {
		t.Fatal("unchanged primary issued a PUT")
	}
	// Access reduction commits while both engines are unavailable.
	e.fake.SetDown(true, true)
	if err := e.tx(func(tx pgx.Tx) error {
		_, _, err := tenant.TransferOwnership(ctx, tx, tn.Scope(), caller, mb.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if m := e.membership(tn.Scope(), caller.Membership.ID); m.Role != tenant.RoleAdmin {
		t.Fatal("transfer did not commit")
	}
	stored, err := tenant.Get(ctx, e.db.Pool, tn.Scope())
	if err != nil || stored.CreatedByIdentityID != a.Identity.ID || stored.BillingRecipientStatus != tenant.BillingRecipientPending {
		t.Fatalf("provenance/status changed: %+v %v", stored, err)
	}
	e.fake.SetDown(false, false)
	e.recipient(tn, b.Email, tenant.BillingRecipientReady)
	if err := e.tx(func(tx pgx.Tx) error {
		_, err := tenant.RemoveMember(ctx, tx, tn.Scope(), e.caller(tn.Scope(), c, true), mb.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	e.recipient(tn, c.Email, tenant.BillingRecipientReady)
	if _, err := e.db.Pool.Exec(ctx, `UPDATE identities SET email_verified=false WHERE brand_id=$1 AND id=$2`, tn.BrandID, c.Identity.ID); err != nil {
		t.Fatal(err)
	}
	e.recipient(tn, "", tenant.BillingRecipientNoEligibleOwner)
	after, _ := e.fake.Account(e.brand.Slug, tn.PublicID())
	before.Email = ""
	if !reflect.DeepEqual(before, after) {
		t.Fatal("primary set/clear changed unrelated account fields or CC")
	}
	untouched, _ := e.fake.Account(e.brand.Slug, other.PublicID())
	if untouched.Email != a.Email {
		t.Fatal("another tenant changed")
	}
}

func TestBillingRecipientFallbackOrderingAndRetainedBinding(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a, b, c := e.person("a@example.test"), e.person("b@example.test"), e.person("c@example.test")
	tn := e.mustOrg(a, "Ordering")
	caller := e.caller(tn.Scope(), a, true)
	mb, mc := e.join(tn.Scope(), caller, b, tenant.RoleAdmin), e.join(tn.Scope(), caller, c, tenant.RoleAdmin)
	e.promote(tn, caller, mb)
	e.promote(tn, caller, mc)
	// Same creation time makes the membership ID the decisive tie breaker.
	if _, err := e.db.Pool.Exec(ctx, `UPDATE memberships SET created_at='2020-01-01' WHERE brand_id=$1 AND tenant_id=$2`, tn.BrandID, tn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Pool.Exec(ctx, `UPDATE identities SET email_verified=false WHERE id=$1`, a.Identity.ID); err != nil {
		t.Fatal(err)
	}
	want, winner := b.Email, mb.ID
	if mc.ID.String() < mb.ID.String() {
		want, winner = c.Email, mc.ID
	}
	e.recipient(tn, want, tenant.BillingRecipientReady)
	if _, err := e.db.Pool.Exec(ctx, `UPDATE memberships SET created_at='2010-01-01' WHERE brand_id=$1 AND tenant_id=$2 AND id<>$3`, tn.BrandID, tn.ID, winner); err != nil {
		t.Fatal(err)
	}
	e.recipient(tn, want, tenant.BillingRecipientReady)
	if _, err := e.db.Pool.Exec(ctx, `UPDATE memberships SET status='suspended' WHERE id=$1`, winner); err != nil {
		t.Fatal(err)
	}
	if winner == mb.ID {
		want = c.Email
	} else {
		want = b.Email
	}
	e.recipient(tn, want, tenant.BillingRecipientReady)
	// A verified active owner whose current email is whitespace is ineligible.
	if _, err := e.db.Pool.Exec(ctx, `UPDATE identities SET email='  ' WHERE brand_id=$1 AND email=$2`, tn.BrandID, want); err != nil {
		t.Fatal(err)
	}
	e.recipient(tn, "", tenant.BillingRecipientNoEligibleOwner)
}

func TestBillingRecipientFailureBackoffAndReadySweepRepair(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.person("initial@example.test")
	tn := e.mustOrg(a, "Repair")
	e.recipient(tn, a.Email, tenant.BillingRecipientReady)
	if _, err := e.db.Pool.Exec(ctx, `UPDATE identities SET email='updated@example.test' WHERE id=$1`, a.Identity.ID); err != nil {
		t.Fatal(err)
	}
	e.fake.FailWrites("/1.0/kb/accounts/"+tn.KillBillAccountID, 1, http.StatusServiceUnavailable)
	r, err := tenant.ReconcileBillingRecipient(ctx, e.db.Pool, e.eng, tn.Scope(), "failure")
	if err == nil || r.Matches || r.Status != tenant.BillingRecipientPending {
		t.Fatalf("failure reported applied: %+v %v", r, err)
	}
	var attempts int
	var delay float64
	var safeCode string
	if err := e.db.Pool.QueryRow(ctx, `SELECT attempts, extract(epoch from next_attempt_at-now()), last_error_code FROM tenant_billing_recipients WHERE tenant_id=$1`, tn.ID).Scan(&attempts, &delay, &safeCode); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || delay <= 0 || delay > 5 || strings.Contains(safeCode, "@") {
		t.Fatalf("backoff metadata: %d %f %q", attempts, delay, safeCode)
	}
	writes := len(e.fake.Writes())
	r, err = tenant.ReconcileBillingRecipient(ctx, e.db.Pool, e.eng, tn.Scope(), "deferred")
	if err != nil || !r.Deferred || len(e.fake.Writes()) != writes {
		t.Fatalf("backoff ignored: %+v %v", r, err)
	}
	e.recipientDue(tn)
	e.fake.SetDown(true, false)
	sweep, err := (&tenant.Sweeper{DB: e.db.Pool, Engines: e.eng}).SweepOnce(ctx)
	if err != nil || sweep.Errors == 0 || sweep.RecipientsUpdated != 1 {
		t.Fatalf("Keycloak failure skipped recipient: %+v %v", sweep, err)
	}
	e.fake.SetDown(false, false)
	e.recipient(tn, "updated@example.test", tenant.BillingRecipientReady)
	creds := killbill.TenantCredentials{APIKey: e.brand.Slug, APISecret: "kb-secret-" + e.brand.Slug}
	if err := e.eng.KillBill.SetAccountEmail(ctx, creds, killbill.CallContext{CreatedBy: "test"}, tn.KillBillAccountID, "external-drift@example.test"); err != nil {
		t.Fatal(err)
	}
	sweep, err = (&tenant.Sweeper{DB: e.db.Pool, Engines: e.eng}).SweepOnce(ctx)
	if err != nil || sweep.RecipientsUpdated != 1 {
		t.Fatalf("ready row not repaired: %+v %v", sweep, err)
	}
	e.recipient(tn, "updated@example.test", tenant.BillingRecipientReady)
}

func TestBillingRecipientMutationRollbackPublishesNothing(t *testing.T) {
	e := newEnv(t)
	a, b := e.person("a@example.test"), e.person("b@example.test")
	tn := e.mustOrg(a, "Rollback")
	caller := e.caller(tn.Scope(), a, true)
	member := e.join(tn.Scope(), caller, b, tenant.RoleAdmin)
	e.recipient(tn, a.Email, tenant.BillingRecipientReady)
	var before int
	if err := e.db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM outbox`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	rollback := errors.New("rollback test")
	err := e.tx(func(tx pgx.Tx) error {
		if _, _, err := tenant.TransferOwnership(context.Background(), tx, tn.Scope(), caller, member.ID); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	var after int
	if err := e.db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM outbox`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	stored, err := tenant.Get(context.Background(), e.db.Pool, tn.Scope())
	if err != nil || after != before || stored.BillingRecipientStatus != tenant.BillingRecipientReady || e.membership(tn.Scope(), member.ID).Role != tenant.RoleAdmin {
		t.Fatal("rolled back mutation published recipient work")
	}
}

// A controlled HTTP server exercises failures the fixture intentionally does
// not model. No primary success may follow an unobserved or wrong-scope write.
func TestBillingRecipientRefusesMismatchAndUnobservedWrite(t *testing.T) {
	for _, mode := range []string{"account", "external-key", "readback", "readback-error"} {
		t.Run(mode, func(t *testing.T) {
			e := newEnv(t)
			owner := e.person("owner@example.test")
			tn := e.mustOrg(owner, "Scoped read")
			var puts, gets atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get(killbill.HeaderAPIKey) != e.brand.Slug || r.Header.Get(killbill.HeaderAPISecret) != "kb-secret-"+e.brand.Slug {
					t.Error("wrong brand credentials")
				}
				if r.Method == http.MethodPut {
					puts.Add(1)
					w.WriteHeader(http.StatusNoContent)
					return
				}
				n := gets.Add(1)
				if mode == "readback-error" && n > 1 {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				a := killbill.Account{AccountID: tn.KillBillAccountID, ExternalKey: tn.PublicID(), Email: "stale@example.test"}
				if mode == "account" {
					a.AccountID = idgen.MustNewV7().String()
				}
				if mode == "external-key" {
					a.ExternalKey = "another-tenant"
				}
				_ = json.NewEncoder(w).Encode(a)
			}))
			t.Cleanup(srv.Close)
			kb, err := killbill.New(killbill.Config{URL: srv.URL, AdminUser: "admin", AdminPassword: "pw"})
			if err != nil {
				t.Fatal(err)
			}
			e.eng.KillBill = kb
			r, err := tenant.ReconcileBillingRecipient(context.Background(), e.db.Pool, e.eng, tn.Scope(), "mismatch")
			want := int32(0)
			if strings.HasPrefix(mode, "readback") {
				want = 1
			}
			if err == nil || r.Matches || r.Status != tenant.BillingRecipientPending || puts.Load() != want {
				t.Fatalf("unsafe success: %+v puts=%d err=%v", r, puts.Load(), err)
			}
		})
	}
}

func TestBillingRecipientMembershipChangeDuringWriteRemainsPending(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	a, b := e.person("old@example.test"), e.person("new@example.test")
	tn := e.mustOrg(a, "Concurrent recipient")
	caller := e.caller(tn.Scope(), a, true)
	m := e.join(tn.Scope(), caller, b, tenant.RoleAdmin)
	if _, err := e.db.Pool.Exec(ctx, `UPDATE identities SET email='old-updated@example.test' WHERE id=$1`, a.Identity.ID); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	e.fake.AfterWrite(func(method, path string) {
		if method == http.MethodPut && path == "/1.0/kb/accounts/"+tn.KillBillAccountID {
			close(entered)
			<-release
		}
	})
	type result struct {
		r   tenant.BillingRecipientReport
		err error
	}
	done := make(chan result, 1)
	go func() {
		r, err := tenant.ReconcileBillingRecipient(ctx, e.db.Pool, e.eng, tn.Scope(), "late")
		done <- result{r, err}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("write did not start")
	}
	err := pgx.BeginFunc(ctx, e.db.Pool, func(tx pgx.Tx) error {
		_, _, err := tenant.TransferOwnership(ctx, tx, tn.Scope(), caller, m.ID)
		return err
	})
	unblock()
	if err != nil {
		t.Fatal(err)
	}
	var got result
	select {
	case got = <-done:
	case <-ctx.Done():
		t.Fatal("reconcile did not complete after release")
	}
	e.fake.AfterWrite(nil)
	if got.err != nil || got.r.Matches || !got.r.Deferred || got.r.Status != tenant.BillingRecipientPending {
		t.Fatalf("late write marked ready: %+v %v", got.r, got.err)
	}
	e.recipient(tn, b.Email, tenant.BillingRecipientReady)
}
