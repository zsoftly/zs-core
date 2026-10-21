//go:build integration

package stacktest

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/internal/postgrestest"
	"github.com/zsoftly/zs-core/pkg/brand"
	"github.com/zsoftly/zs-core/pkg/idgen"
)

// Invoice templates, HTML and PDF on Kill Bill 0.24.22 and the pinned PDF
// renderer. zs-reference uploads its template, its manual pay template and
// its translations in EN and FR, with the seller of the manifest added. A
// tenant created by an FR user gets a Kill Bill account in FR, one created
// by an EN user an account in EN. An invoice with a charge and a TAX item
// renders through Core in each locale, as HTML and as PDF, with the invoice
// number, the seller's name, address and registration number, the tax line
// and the currency. Invoice numbers are unique and increase within a Kill
// Bill tenant.
func TestInvoicesOnTheStack(t *testing.T) {
	s := newStack(t)
	db := postgrestest.New(t)
	deps := s.deps(t, db)
	ctx := context.Background()
	env(t, envRendererURL)
	if _, err := exec.LookPath("pdftotext"); err != nil {
		t.Fatal("pdftotext is required to read the PDF text: install poppler-utils in the test container (docs/development.md)")
	}

	ref, err := brand.Load(filepath.Join(brandsDir(), "zs-reference.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	dev := devVariant(t, ref)
	apply(t, deps, dev)
	t.Cleanup(func() { _, _ = brand.Apply(context.Background(), s.deps(t, db), ref, cli) })
	if again := apply(t, deps, dev); len(again.Changes) != 0 {
		t.Fatalf("second apply = %s", mustJSON(again))
	}
	creds := killbill.TenantCredentials{APIKey: "zs-reference", APISecret: os.Getenv(refKillBillSecretVar)}

	// What Kill Bill holds is the file, and the translations carry the
	// seller of the manifest.
	tpl, err := os.ReadFile(filepath.Join(brandsDir(), "zs-reference", "invoices", "invoice.html"))
	if err != nil {
		t.Fatal(err)
	}
	if v, ok, err := s.kb.TenantValue(ctx, creds, killbill.KeyInvoiceTemplate); err != nil || !ok || v != string(tpl) {
		t.Fatalf("invoice template in Kill Bill differs from the file: %v %v", ok, err)
	}
	for _, l := range []string{"en", "fr"} {
		v, ok, err := s.kb.TenantValue(ctx, creds, killbill.InvoiceTranslationKey(l))
		if err != nil || !ok || !strings.Contains(v, "sellerTaxRegistrations=GST/HST 000000000 RT0001") || !strings.Contains(v, "sellerName=ZSoftly Technologies Inc.") {
			t.Fatalf("translation %s = %q %v", l, v, err)
		}
	}

	api, _ := billingAPI(t, s, db, deps)
	suffix := strings.ToLower(idgen.MustNewV7().String()[28:])
	type party struct {
		locale, title, user string
		token, tenantID     string
		accountID, invoice  string
	}
	parties := []*party{{locale: "en", title: "Invoice"}, {locale: "fr", title: "Facture"}}
	for _, p := range parties {
		p.user = "inv-" + p.locale + "-" + suffix
		createUserWithLocale(t, s.kc, "zs-reference", p.user, "pw-"+suffix, p.locale)
		tok := passwordToken(t, s.kcURL, "zs-reference", "zs-customer", "", p.user, "pw-"+suffix)
		if got := claimOf(t, tok.access, "locale"); got != p.locale {
			t.Fatalf("locale claim of %s = %q", p.user, got)
		}
		p.token = tok.access
		if status, out := api.call(t, http.MethodGet, "/v1/me", p.token, nil, ""); status != http.StatusOK {
			t.Fatalf("/v1/me = %d %s", status, mustJSON(out))
		}
		p.tenantID = newTenantFor(t, api, p.token, "Invoices "+p.locale+" "+suffix)
		acct := accountOf(t, creds, p.tenantID)
		if acct["locale"] != p.locale || acct["email"] != p.user+"@example.test" {
			t.Fatalf("Kill Bill account of the %s tenant = locale %v email %v", p.locale, acct["locale"], acct["email"])
		}
		p.accountID = acct["accountId"].(string)
		p.invoice = taxedInvoice(t, creds, p.accountID, "Compute "+p.locale, "HST 13% ON")
	}

	for _, p := range parties {
		inv, err := s.kb.Invoice(ctx, creds, p.invoice)
		if err != nil {
			t.Fatal(err)
		}
		status, h, html := api.raw(t, http.MethodGet, "/v1/tenants/"+p.tenantID+"/invoices/"+p.invoice+"/html", p.token)
		if status != http.StatusOK || !strings.HasPrefix(h.Get("Content-Type"), "text/html") || h.Get("Cache-Control") != "no-store" || !strings.Contains(h.Get("Content-Security-Policy"), "default-src 'none'") {
			t.Fatalf("%s html = %d %v %s", p.locale, status, h, html)
		}
		for _, want := range []string{p.title, inv.InvoiceNumber, "ZSoftly Technologies Inc.", "100 Example Street, Suite 1, Ottawa, Ontario", "GST/HST 000000000 RT0001", "HST 13% ON", "TAX", "CAD"} {
			if !strings.Contains(string(html), want) {
				t.Errorf("%s html lacks %q:\n%s", p.locale, want, html)
			}
		}
		status, h, pdf := api.raw(t, http.MethodGet, "/v1/tenants/"+p.tenantID+"/invoices/"+p.invoice+"/pdf", p.token)
		if status != http.StatusOK || h.Get("Content-Type") != "application/pdf" || h.Get("Cache-Control") != "no-store" || !bytes.HasPrefix(pdf, []byte("%PDF")) || len(pdf) < 5000 {
			t.Fatalf("%s pdf = %d %v, %d bytes", p.locale, status, h, len(pdf))
		}
		file := filepath.Join(t.TempDir(), p.locale+".pdf")
		if err := os.WriteFile(file, pdf, 0o600); err != nil {
			t.Fatal(err)
		}
		text, err := exec.Command("pdftotext", "-layout", file, "-").Output()
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{p.title, inv.InvoiceNumber, "ZSoftly Technologies Inc.", "100 Example Street, Suite 1, Ottawa, Ontario", "GST/HST 000000000 RT0001", "HST 13% ON", "CAD"} {
			if !strings.Contains(string(text), want) {
				t.Errorf("%s pdf text lacks %q:\n%s", p.locale, want, text)
			}
		}
		t.Logf("%s invoice %s: html %d bytes, pdf %d bytes", p.locale, inv.InvoiceNumber, len(html), len(pdf))
	}
	// Another tenant's invoice is not found, as HTML and as PDF.
	for _, kind := range []string{"html", "pdf"} {
		if status, _, body := api.raw(t, http.MethodGet, "/v1/tenants/"+parties[0].tenantID+"/invoices/"+parties[1].invoice+"/"+kind, parties[0].token); status != http.StatusNotFound || !strings.Contains(string(body), "not_found") {
			t.Fatalf("another tenant's %s = %d %s", kind, status, body)
		}
	}

	// Invoice numbers: unique and increasing within the Kill Bill tenant.
	// Kill Bill 0.24.22 numbers invoices with one sequence for the whole
	// server, so another Kill Bill tenant's invoices leave gaps.
	second := secondManifest(t, ref)
	apply(t, deps, second)
	secondCreds := killbill.TenantCredentials{APIKey: "zs-second", APISecret: secondKillBillSecret}
	status, hdr, _ := kbRaw(t, secondCreds, http.MethodPost, "/1.0/kb/accounts", map[string]any{"name": "numbers", "externalKey": "numbers-" + suffix, "currency": "CAD"})
	if status != http.StatusCreated {
		t.Fatalf("zs-second account = %d", status)
	}
	secondAccount := hdr.Get("Location")[strings.LastIndex(hdr.Get("Location"), "/")+1:]
	var refNumbers, secondNumbers []int
	charge := func(c killbill.TenantCredentials, account string) int {
		status, _, body := kbRaw(t, c, http.MethodPost, "/1.0/kb/invoices/charges/"+account+"?autoCommit=true", []map[string]any{{"accountId": account, "amount": 1, "currency": "CAD"}})
		var items []killbill.InvoiceItem
		if status != http.StatusOK || json.Unmarshal(body, &items) != nil {
			t.Fatalf("charge = %d %s", status, body)
		}
		inv, err := s.kb.Invoice(ctx, c, items[0].InvoiceID)
		if err != nil {
			t.Fatal(err)
		}
		n, _ := strconv.Atoi(inv.InvoiceNumber)
		return n
	}
	for i := 0; i < 3; i++ {
		refNumbers = append(refNumbers, charge(creds, parties[0].accountID))
		secondNumbers = append(secondNumbers, charge(secondCreds, secondAccount))
	}
	all, err := s.kb.AccountInvoices(ctx, creds, parties[0].accountID, false)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	var listed []int
	for _, inv := range all {
		if seen[inv.InvoiceNumber] {
			t.Fatalf("invoice number %s twice", inv.InvoiceNumber)
		}
		seen[inv.InvoiceNumber] = true
		n, _ := strconv.Atoi(inv.InvoiceNumber)
		listed = append(listed, n)
	}
	for _, numbers := range [][]int{refNumbers, secondNumbers} {
		for i, number := range numbers {
			if number <= 0 || i > 0 && number <= numbers[i-1] {
				t.Fatalf("invoice numbers zs-reference %v zs-second %v", refNumbers, secondNumbers)
			}
		}
	}
	t.Logf("invoice numbers: zs-reference %v, zs-second %v, all of the account %v", refNumbers, secondNumbers, listed)
}
