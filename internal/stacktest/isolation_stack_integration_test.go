//go:build integration

package stacktest

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/internal/postgrestest"
	"github.com/zsoftly/zs-core/pkg/brand"
	"github.com/zsoftly/zs-core/pkg/idgen"
)

// Brand isolation of payment providers, invoice branding, tax settings,
// email templates and senders on one stack. zs-reference (A) runs Adyen by
// default and Stripe beside it, zs-second (B) runs Stripe by default and
// Adyen beside it, each with its own configuration, invoice templates,
// seller, overdue states, email templates and sender. Mailpit shows each
// brand's sender on its own invoice email, with its own template. A change
// to A's templates, sender, plugin configuration, overdue states and tax
// plugin leaves every value of B's Kill Bill tenant the same, byte for
// byte. A's credentials read and write only A's values.
func TestBrandIsolationOnTheStack(t *testing.T) {
	s := newStack(t)
	db := postgrestest.New(t)
	deps := s.deps(t, db)
	// Use the supplied mirror so plugin installation avoids unrelated upstream downloads.
	deps.PluginMirrorURL, deps.PluginMirrorSecret = env(t, envMirrorURL), []byte(env(t, envMirrorSecret))
	restoreDeps := deps
	ctx := context.Background()
	env(t, envMailpitURL)

	// Each brand's files in a directory the test can edit.
	files := t.TempDir()
	copyDir(t, filepath.Join(brandsDir(), "zs-reference"), filepath.Join(files, "zs-reference"))
	copyDir(t, filepath.Join(brandsDir(), "zs-reference"), filepath.Join(files, "zs-second"))
	replaceInFile(t, filepath.Join(files, "zs-second", "invoices", "invoice.html"), "<main class=\"invoice\">", "<main class=\"invoice\"><p>ZS-SECOND-INVOICE</p>")
	for _, l := range []string{"en", "fr"} {
		replaceInFile(t, filepath.Join(files, "zs-second", "email", l, "InvoiceCreation.mustache"), "zs-reference email template", "zs-second email template")
		replaceInFile(t, filepath.Join(files, "zs-second", "email", l, "Translation.properties"), "invoiceCreationSubject=", "invoiceCreationSubject=[zs-second] ")
	}
	deps.Files = os.DirFS(files)

	t.Setenv("BRAND_ZS_REFERENCE__ADYEN", "org.killbill.billing.plugin.adyen.enviroment=TEST\norg.killbill.billing.plugin.adyen.merchantAccount=zs-reference")
	t.Setenv("BRAND_ZS_REFERENCE__STRIPE", "org.killbill.billing.plugin.stripe.apiKey=sk_test_zs_reference_placeholder")
	t.Setenv("BRAND_ZS_SECOND__ADYEN", "org.killbill.billing.plugin.adyen.enviroment=TEST\norg.killbill.billing.plugin.adyen.merchantAccount=zs-second")

	ref, err := brand.Load(filepath.Join(brandsDir(), "zs-reference.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	a := devVariant(t, ref)
	a.Billing.PaymentPlugins = []brand.PaymentPlugin{
		{Name: "adyen", Mode: brand.PaymentModeTest, ConfigRef: "env://BRAND_ZS_REFERENCE__ADYEN", Default: true},
		{Name: "stripe", Mode: brand.PaymentModeTest, ConfigRef: "env://BRAND_ZS_REFERENCE__STRIPE"},
	}
	t.Cleanup(func() { _, _ = brand.Apply(context.Background(), restoreDeps, ref, cli) })

	b := secondManifest(t, ref)
	b.Billing.PaymentPlugin = nil
	b.Billing.PaymentPlugins = []brand.PaymentPlugin{
		{Name: "stripe", Mode: brand.PaymentModeTest, ConfigRef: "env://BRAND_ZS_SECOND__PLUGIN_CONFIG", Default: true},
		{Name: "adyen", Mode: brand.PaymentModeTest, ConfigRef: "env://BRAND_ZS_SECOND__ADYEN"},
	}
	b.Billing.Catalogue = "zs-second/catalogue.xml"
	b.Billing.Overdue = "zs-second/overdue.xml"
	b.Billing.Seller = brand.Seller{Name: "ZSoftly Second Ltd.", Country: "CA", AddressLines: []string{"200 Sample Road", "Gatineau, Quebec"}, TaxRegistrations: []string{"QST 0000000000 TQ0001"}}
	tpl := *a.Billing.InvoiceTemplates
	tpl.Template, tpl.ManualPayTemplate = "zs-second/invoices/invoice.html", "zs-second/invoices/invoice-manual-pay.html"
	tpl.Translations = map[string]string{"en": "zs-second/invoices/translation-en.properties", "fr": "zs-second/invoices/translation-fr.properties"}
	tpl.CatalogueTranslations = map[string]string{"en": "zs-second/invoices/catalogue-en.properties", "fr": "zs-second/invoices/catalogue-fr.properties"}
	b.Billing.InvoiceTemplates = &tpl
	b.Billing.Email = &brand.Email{Sender: "billing@zs-second.example.test", SMTP: brand.SMTP{Host: "mailpit", Port: 1025}, Templates: "zs-second/email"}
	b.Identity.DefaultLocale = "en"

	apply(t, deps, a)
	apply(t, deps, b)
	credsA := killbill.TenantCredentials{APIKey: "zs-reference", APISecret: os.Getenv(refKillBillSecretVar)}
	credsB := killbill.TenantCredentials{APIKey: "zs-second", APISecret: secondKillBillSecret}

	// Each brand's providers and their configurations.
	for _, c := range []struct {
		creds    killbill.TenantCredentials
		key, has string
	}{
		{credsA, "killbill-adyen", "merchantAccount=zs-reference"},
		{credsA, "killbill-stripe", "sk_test_zs_reference_placeholder"},
		{credsB, "killbill-stripe", secondPluginConfig},
		{credsB, "killbill-adyen", "merchantAccount=zs-second"},
		{credsA, "killbill-email-notifications", "defaultSender=billing@zs-reference.example.test"},
		{credsB, "killbill-email-notifications", "defaultSender=billing@zs-second.example.test"},
	} {
		if cfg, found, err := s.kb.PluginConfig(ctx, c.creds, c.key); err != nil || !found || !strings.Contains(cfg, c.has) {
			t.Fatalf("%s %s: found %v %v", c.creds.APIKey, c.key, found, err)
		}
	}

	// Invoice email through each brand's sender and template.
	api, _ := billingAPI(t, s, db, deps)
	suffix := strings.ToLower(idgen.MustNewV7().String()[28:])
	type tenantOf struct {
		realm, client, sender, marker string
		creds                         killbill.TenantCredentials
		user, tenantID                string
	}
	brands := []*tenantOf{
		{realm: "zs-reference", client: "zs-customer", sender: "billing@zs-reference.example.test", marker: "zs-reference email template, en", creds: credsA},
		{realm: "zs-second", client: "zs-second-customer", sender: "billing@zs-second.example.test", marker: "zs-second email template, en", creds: credsB},
	}
	for _, br := range brands {
		br.user = "iso-" + br.realm + "-" + suffix
		createUserWithLocale(t, s.kc, br.realm, br.user, "pw-"+suffix, "en")
		tok := passwordToken(t, s.kcURL, br.realm, br.client, "", br.user, "pw-"+suffix)
		if status, out := api.call(t, http.MethodGet, "/v1/me", tok.access, nil, ""); status != http.StatusOK {
			t.Fatalf("%s /v1/me = %d %s", br.realm, status, mustJSON(out))
		}
		br.tenantID = newTenantFor(t, api, tok.access, "Isolation "+br.realm+" "+suffix)
		acct := accountOf(t, br.creds, br.tenantID)
		taxedInvoice(t, br.creds, acct["accountId"].(string), "Isolation charge", "HST 13% ON")
	}
	for _, br := range brands {
		msg, html := waitForMail(t, br.user+"@example.test")
		if msg.From.Address != br.sender || !strings.Contains(html, br.marker) {
			t.Fatalf("%s email from %s subject %q, marker %v", br.realm, msg.From.Address, msg.Subject, strings.Contains(html, br.marker))
		}
		t.Logf("%s email: from %s, subject %q", br.realm, msg.From.Address, msg.Subject)
	}

	// Every value of B's tenant that a brand apply writes, read back.
	keysB := []string{killbill.KeyInvoiceTemplate, killbill.KeyInvoiceManualPayTemplate,
		killbill.InvoiceTranslationKey("en"), killbill.InvoiceTranslationKey("fr"),
		killbill.CatalogTranslationKey("en"), killbill.CatalogTranslationKey("fr"),
		brand.EmailTemplateKey("INVOICE_CREATION", "en"), brand.EmailTemplateKey("INVOICE_CREATION", "fr"),
		brand.EmailTranslationKey("en"), brand.EmailTranslationKey("fr"),
		"PLUGIN_CONFIG_killbill-stripe", "PLUGIN_CONFIG_killbill-adyen", "PLUGIN_CONFIG_killbill-email-notifications", "PLUGIN_CONFIG_killbill-avatax",
		"zs-core-catalogue-sha256", "zs-core-overdue-sha256", "PUSH_NOTIFICATION_CB"}
	snapshot := func() map[string]string {
		out := map[string]string{}
		for _, k := range keysB {
			v, ok, err := s.kb.TenantValue(ctx, credsB, k)
			if err != nil {
				t.Fatal(err)
			}
			if ok {
				out[k] = v
			}
		}
		status, _, body := kbRaw(t, credsB, http.MethodGet, "/1.0/kb/overdue", nil)
		if status != http.StatusOK {
			t.Fatalf("overdue of B = %d", status)
		}
		out["overdue"] = string(body)
		return out
	}
	before := snapshot()
	if !strings.Contains(before[killbill.KeyInvoiceTemplate], "ZS-SECOND-INVOICE") || strings.Contains(before[killbill.InvoiceTranslationKey("en")], "ZSoftly Technologies Inc.") ||
		!strings.Contains(before[killbill.InvoiceTranslationKey("en")], "QST 0000000000 TQ0001") {
		t.Fatalf("B's own branding: template %v, translation %q", strings.Contains(before[killbill.KeyInvoiceTemplate], "ZS-SECOND-INVOICE"), before[killbill.InvoiceTranslationKey("en")])
	}
	if _, ok := before["PLUGIN_CONFIG_killbill-avatax"]; ok {
		t.Fatal("B has a tax plugin configuration")
	}

	// Change A's template, sender, plugin configuration, overdue states
	// and tax plugin.
	replaceInFile(t, filepath.Join(files, "zs-reference", "invoices", "invoice.html"), "<main class=\"invoice\">", "<main class=\"invoice\"><p>ZS-REFERENCE-CHANGED</p>")
	replaceInFile(t, filepath.Join(files, "zs-reference", "overdue.xml"), "<number>30</number>", "<number>31</number>")
	t.Setenv("BRAND_ZS_REFERENCE__ADYEN", "org.killbill.billing.plugin.adyen.enviroment=TEST\norg.killbill.billing.plugin.adyen.merchantAccount=zs-reference-"+suffix)
	t.Setenv("BRAND_ZS_REFERENCE__TAX", "org.killbill.billing.plugin.avatax.url=https://sandbox-rest.avatax.com/api/v2\norg.killbill.billing.plugin.avatax.companyCode=ZSREF-"+suffix)
	changed := clone(t, a)
	changed.Billing.Email.Sender = "invoices@zs-reference.example.test"
	changed.Billing.InvoicePlugin = &brand.InvoicePlugin{Name: "avatax", Mode: brand.PaymentModeTest, ConfigRef: "env://BRAND_ZS_REFERENCE__TAX"}
	t.Cleanup(func() { uninstallPlugin(t, s, pin(t, pins(t), "avatax")) })
	diff := apply(t, deps, changed)
	targets := map[string]bool{}
	for _, c := range diff.Changes {
		targets[c.Target+" "+c.Name] = true
	}
	for _, want := range []string{"killbill.invoice_template INVOICE_TEMPLATE", "killbill.overdue zs-reference/overdue.xml", "killbill.plugin_config killbill-adyen",
		"killbill.plugin_config killbill-email-notifications", "killbill.plugin avatax", "killbill.plugin_config killbill-avatax"} {
		if !targets[want] {
			t.Errorf("A's change lacks %s: %v", want, targets)
		}
	}
	// This check verifies A's new sender after Core's STOP/wait then START/wait.
	// It makes no at-once assertion.
	later := "iso-later-" + suffix
	createUserWithLocale(t, s.kc, "zs-reference", later, "pw-"+suffix, "en")
	tok := passwordToken(t, s.kcURL, "zs-reference", "zs-customer", "", later, "pw-"+suffix)
	if status, out := api.call(t, http.MethodGet, "/v1/me", tok.access, nil, ""); status != http.StatusOK {
		t.Fatalf("/v1/me = %d %s", status, mustJSON(out))
	}
	laterTenant := newTenantFor(t, api, tok.access, "Isolation later "+suffix)
	// Stop the tax plugin first, so it never rates this invoice.
	uninstallPlugin(t, s, pin(t, pins(t), "avatax"))
	taxedInvoice(t, credsA, accountOf(t, credsA, laterTenant)["accountId"].(string), "Isolation charge", "HST 13% ON")
	if msg, _ := waitForMail(t, later+"@example.test"); msg.From.Address != "invoices@zs-reference.example.test" {
		t.Fatalf("A's email after the sender change is from %s", msg.From.Address)
	}

	// The tax plugin's per-tenant configuration is A's alone.
	if cfg, found, err := s.kb.PluginConfig(ctx, credsA, "killbill-avatax"); err != nil || !found || !strings.Contains(cfg, "ZSREF") {
		t.Fatalf("A's tax configuration: %v %v", found, err)
	}
	after := snapshot()
	for k, v := range before {
		if after[k] != v {
			t.Errorf("B's %s changed when A changed", k)
		}
	}
	if len(after) != len(before) {
		t.Errorf("B's values: %d before, %d after", len(before), len(after))
	}

	// A's credentials reach A's values only. A read gives A's template, a
	// write changes A's alone, and A's key with B's secret is refused.
	if v, _, err := s.kb.TenantValue(ctx, credsA, killbill.KeyInvoiceTemplate); err != nil || !strings.Contains(v, "ZS-REFERENCE-CHANGED") || strings.Contains(v, "ZS-SECOND-INVOICE") {
		t.Fatalf("A reads %v", err)
	}
	if err := s.kb.UploadInvoiceTemplate(ctx, credsA, killbill.CallContext{CreatedBy: "stack-test"}, "<html>A only</html>"); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := s.kb.TenantValue(ctx, credsB, killbill.KeyInvoiceTemplate); v != before[killbill.KeyInvoiceTemplate] {
		t.Fatal("A's upload changed B's template")
	}
	aOnlyStripeConfig := "org.killbill.billing.plugin.stripe.apiKey=sk_test_zs_reference_isolation_" + suffix
	if err := s.kb.UploadPluginConfig(ctx, credsA, killbill.CallContext{CreatedBy: "stack-test"}, "killbill-stripe", aOnlyStripeConfig); err != nil {
		t.Fatal(err)
	}
	if cfg, found, err := s.kb.PluginConfig(ctx, credsA, "killbill-stripe"); err != nil || !found || cfg != aOnlyStripeConfig {
		t.Fatalf("A's stripe configuration: found %v, err %v, config %q", found, err, cfg)
	}
	if cfg, found, err := s.kb.PluginConfig(ctx, credsB, "killbill-stripe"); err != nil || !found || cfg != before["PLUGIN_CONFIG_killbill-stripe"] {
		t.Fatalf("A's plugin configuration changed B's stripe configuration: found %v, err %v, config %q", found, err, cfg)
	}
	mixed := killbill.TenantCredentials{APIKey: "zs-reference", APISecret: secondKillBillSecret}
	if _, _, err := s.kb.TenantValue(ctx, mixed, killbill.KeyInvoiceTemplate); err == nil {
		t.Fatal("A's key with B's secret reads a template")
	}
	status, _, body := kbRaw(t, mixed, http.MethodGet, "/1.0/kb/tenants/uploadPluginConfig/killbill-stripe", nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("A's key with B's secret reads a plugin configuration: %d %s", status, body)
	}
}
