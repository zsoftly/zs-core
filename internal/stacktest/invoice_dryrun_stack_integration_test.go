//go:build integration

package stacktest

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/pkg/idgen"
)

// TestInvoiceDryRunOnTheStack checks the answers Core relies on for a closure
// invoice on the real Kill Bill 0.24.22. The invoice dry run answers 204 for an
// account with nothing to invoice, and is what decides that. A real invoice run
// for the same account answers only a bare 404, which Core never reads as
// nothing to invoice, and an unknown account or wrong credentials stay errors.
// It needs only Kill Bill, and creates its own tenant and account.
func TestInvoiceDryRunOnTheStack(t *testing.T) {
	url, password := os.Getenv("ZS_TEST_KILLBILL_URL"), os.Getenv("ZS_TEST_KILLBILL_ADMIN_PASSWORD")
	if url == "" || password == "" {
		t.Skip("ZS_TEST_KILLBILL_URL and ZS_TEST_KILLBILL_ADMIN_PASSWORD are not set")
	}
	ctx := context.Background()
	kb, err := killbill.New(killbill.Config{URL: url, AdminUser: "admin", AdminPassword: password})
	if err != nil {
		t.Fatal(err)
	}
	cc := killbill.CallContext{CreatedBy: "stack-test"}
	key := "dryrun-" + idgen.MustNewV7().String()
	creds := killbill.TenantCredentials{APIKey: key, APISecret: "secret-" + key}
	if _, err := kb.CreateTenant(ctx, cc, creds, key); err != nil {
		t.Fatal(err)
	}
	accountID, err := kb.CreateAccount(ctx, creds, cc, killbill.Account{ExternalKey: "acct-" + key, Name: "Dry Run Co", Currency: "CAD", Email: "dryrun@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := kb.DryRunInvoice(ctx, creds, cc, accountID, "2026-12-31"); err != nil || found {
		t.Fatalf("dry run of an account with nothing to invoice = found %v, %v; want 204, not found", found, err)
	}
	if id, err := kb.TriggerInvoice(ctx, creds, cc, accountID, "2026-12-31"); err == nil || id != "" || !errors.Is(err, killbill.ErrNotFound) {
		t.Fatalf("real run of an account with nothing to invoice = %q %v; want an ErrNotFound error", id, err)
	}
	if id, err := kb.TriggerInvoice(ctx, creds, cc, idgen.MustNewV7().String(), "2026-12-31"); err == nil || id != "" {
		t.Fatalf("real run of an unknown account = %q %v; want an error", id, err)
	}
	if _, found, err := kb.DryRunInvoice(ctx, creds, cc, idgen.MustNewV7().String(), "2026-12-31"); err == nil || found {
		t.Fatalf("dry run of an unknown account = found %v, %v; want an error", found, err)
	}
	wrong := killbill.TenantCredentials{APIKey: key, APISecret: "wrong"}
	if _, found, err := kb.DryRunInvoice(ctx, wrong, cc, accountID, "2026-12-31"); err == nil || found {
		t.Fatalf("dry run with wrong credentials = found %v, %v; want an error", found, err)
	}
}
