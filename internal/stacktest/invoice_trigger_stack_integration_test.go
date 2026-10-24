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

// TestInvoiceTriggerOnTheStack checks the answers of an invoice run on the
// real Kill Bill 0.24.22: an account with nothing to invoice is Tomcat's bare
// 404 and is read as nothing to invoice, an unknown account is a JSON 404
// and stays an error, and wrong tenant credentials stay an error. It needs
// only Kill Bill, and creates its own tenant and account.
func TestInvoiceTriggerOnTheStack(t *testing.T) {
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
	key := "trigger-" + idgen.MustNewV7().String()
	creds := killbill.TenantCredentials{APIKey: key, APISecret: "secret-" + key}
	if _, err := kb.CreateTenant(ctx, cc, creds, key); err != nil {
		t.Fatal(err)
	}
	accountID, err := kb.CreateAccount(ctx, creds, cc, killbill.Account{ExternalKey: "acct-" + key, Name: "Trigger Co", Currency: "CAD", Email: "trigger@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	trigger, err := kb.TriggerInvoice(ctx, creds, cc, accountID, "2026-12-31")
	if err != nil || trigger.Created || trigger.InvoiceID != "" {
		t.Fatalf("account with nothing to invoice = %+v %v", trigger, err)
	}
	if trigger, err = kb.TriggerInvoice(ctx, creds, cc, idgen.MustNewV7().String(), "2026-12-31"); err == nil || trigger.Created || !errors.Is(err, killbill.ErrNotFound) {
		t.Fatalf("unknown account = %+v %v, want an ErrNotFound error", trigger, err)
	}
	wrong := killbill.TenantCredentials{APIKey: key, APISecret: "wrong"}
	if trigger, err = kb.TriggerInvoice(ctx, wrong, cc, accountID, "2026-12-31"); err == nil || trigger.Created {
		t.Fatalf("wrong credentials = %+v %v, want an error", trigger, err)
	}
}
