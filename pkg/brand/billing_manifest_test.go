package brand

import (
	"strings"
	"testing"

	"github.com/zsoftly/zs-core/internal/components"
	"github.com/zsoftly/zs-core/internal/secrets"
)

// withBilling returns the reference manifest with extra YAML lines after its
// currencies line.
func withBilling(t *testing.T, extra string) string {
	t.Helper()
	doc := referenceYAML(t)
	out := strings.Replace(doc, "  currencies: [CAD, USD]\n", "  currencies: [CAD, USD]\n"+extra, 1)
	if out == doc {
		t.Fatal("the reference manifest has no currencies line to extend")
	}
	return out
}

func TestPaymentPluginsList(t *testing.T) {
	m, err := Parse([]byte(withBilling(t, `  payment_plugins:
    - {name: stripe, mode: test, config_ref: env://BRAND_ZS_REFERENCE__STRIPE}
    - {name: adyen, mode: test, config_ref: env://BRAND_ZS_REFERENCE__ADYEN, default: true}
`)))
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Billing.PaymentPluginName(); got != "killbill-adyen" || len(m.Billing.Plugins()) != 2 {
		t.Fatalf("default = %s, plugins %+v", got, m.Billing.Plugins())
	}
	// The one-plugin form is a list of that plugin, the default.
	m, err = Parse([]byte(withBilling(t, "  payment_plugin: {name: stripe, mode: test, config_ref: env://BRAND_ZS_REFERENCE__STRIPE}\n")))
	if err != nil {
		t.Fatal(err)
	}
	if p := m.Billing.Plugins(); len(p) != 1 || !p[0].Default || m.Billing.PaymentPluginName() != "killbill-stripe" {
		t.Fatalf("alias = %+v", p)
	}
	if got := m.Billing.pluginsPath(0); got != "/billing/payment_plugin" {
		t.Fatalf("alias path = %s", got)
	}
	cases := map[string]string{
		"no default": `  payment_plugins:
    - {name: stripe, mode: test, config_ref: env://BRAND_ZS_REFERENCE__STRIPE}
`,
		"two defaults": `  payment_plugins:
    - {name: stripe, mode: test, config_ref: env://BRAND_ZS_REFERENCE__STRIPE, default: true}
    - {name: adyen, mode: test, config_ref: env://BRAND_ZS_REFERENCE__ADYEN, default: true}
`,
		"listed twice": `  payment_plugins:
    - {name: stripe, mode: test, config_ref: env://BRAND_ZS_REFERENCE__STRIPE, default: true}
    - {name: stripe, mode: test, config_ref: env://BRAND_ZS_REFERENCE__OTHER}
`,
		"live in the list": `  payment_plugins:
    - {name: stripe, mode: live, config_ref: env://BRAND_ZS_REFERENCE__STRIPE, default: true}
`,
		"both forms": `  payment_plugin: {name: stripe, mode: test, config_ref: env://BRAND_ZS_REFERENCE__STRIPE}
  payment_plugins:
    - {name: adyen, mode: test, config_ref: env://BRAND_ZS_REFERENCE__ADYEN, default: true}
`,
		"another plugin's key": `  payment_plugins:
    - {name: stripe, mode: test, config_key: killbill-adyen, config_ref: env://BRAND_ZS_REFERENCE__STRIPE, default: true}
`,
		"empty list":                             "  payment_plugins: []\n",
		"live invoice plugin":                    "  invoice_plugin: {name: avatax, mode: live, config_ref: env://BRAND_ZS_REFERENCE__TAX}\n",
		"invoice plugin key":                     "  invoice_plugin: {name: avatax, mode: test, config_key: killbill-stripe, config_ref: env://BRAND_ZS_REFERENCE__TAX}\n",
		"invoice plugin without a configuration": "  invoice_plugin: {name: avatax, mode: test}\n",
	}
	for name, extra := range cases {
		if _, err := Parse([]byte(withBilling(t, extra))); !isInvalid(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestInvoiceTemplatesAndEmailRules(t *testing.T) {
	ref := reference(t)
	if ref.Billing.InvoiceTemplates == nil || ref.Billing.Email == nil || ref.Billing.Email.SMTP.Host != "mailpit" {
		t.Fatalf("reference billing = %+v", ref.Billing)
	}
	cases := map[string]string{
		"translation of another locale": "  invoice_templates:\n    translations: {de: zs-reference/invoices/translation-de.properties}\n",
		"template outside":              "  invoice_templates:\n    template: ../invoice.html\n",
		"port":                          "  email: {sender: a@example.test, smtp: {host: mailpit, port: 0}}\n",
		"user without password":         "  email: {sender: a@example.test, smtp: {host: mailpit, port: 25, username_ref: env://BRAND_ZS_REFERENCE__U}}\n",
		"unknown event":                 "  email: {sender: a@example.test, events: [ACCOUNT_CHANGE], smtp: {host: mailpit, port: 25}}\n",
		"sender":                        "  email: {sender: not-an-address, smtp: {host: mailpit, port: 25}}\n",
		"host":                          "  email: {sender: a@example.test, smtp: {host: 'mail pit', port: 25}}\n",
	}
	for name, extra := range cases {
		doc := withBilling(t, extra)
		// The reference manifest has its own sections: drop them so the
		// case is the only one.
		doc = dropSection(doc, "  invoice_templates:\n", "  email:\n")
		doc = dropSection(doc, "  email:\n", "  wallet:\n")
		if !strings.Contains(doc, extra) {
			doc = strings.Replace(doc, "  currencies: [CAD, USD]\n", "  currencies: [CAD, USD]\n"+extra, 1)
		}
		if _, err := Parse([]byte(doc)); !isInvalid(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// dropSection removes the first block that starts with start and ends
// before end, after the extension point, so a case replaces the reference's
// own section.
func dropSection(doc, start, end string) string {
	i := strings.LastIndex(doc, start)
	if i < 0 {
		return doc
	}
	j := strings.Index(doc[i:], end)
	if j < 0 {
		return doc
	}
	return doc[:i] + doc[i+j:]
}

func TestSecretRefsCoverPluginsAndEmail(t *testing.T) {
	m, err := Parse([]byte(withBilling(t, `  payment_plugins:
    - {name: stripe, mode: test, config_ref: env://BRAND_ZS_SECOND__STRIPE, default: true}
  invoice_plugin: {name: avatax, mode: test, config_ref: env://BRAND_ZS_SECOND__TAX}
`)))
	if err != nil {
		t.Fatal(err)
	}
	m.Billing.Email.SMTP.UsernameRef, m.Billing.Email.SMTP.PasswordRef = "env://BRAND_ZS_SECOND__U", "env://BRAND_ZS_SECOND__P"
	err = m.CheckSecretRefs(secrets.Default{})
	for _, path := range []string{"/billing/payment_plugins/0/config_ref", "/billing/invoice_plugin/config_ref", "/billing/email/smtp/username_ref", "/billing/email/smtp/password_ref"} {
		if codeOf(err) != "secret_ref_invalid" || !strings.Contains(messageOf(err), path) {
			t.Errorf("%s: %v", path, err)
		}
	}
}

func TestEmailKeysAndConfiguration(t *testing.T) {
	// The email-notifications plugin 0.8.1 appends _<language>_<country>.
	if got := EmailTemplateKey("INVOICE_CREATION", "en"); got != "killbill-email-notifications:INVOICE_CREATION_en_" {
		t.Fatalf("en key = %s", got)
	}
	if got := EmailTemplateKey("INVOICE_CREATION", "fr-CA"); got != "killbill-email-notifications:INVOICE_CREATION_fr_CA" {
		t.Fatalf("fr-CA key = %s", got)
	}
	if got := EmailTranslationKey("fr"); got != "killbill-email-notifications:TEMPLATE_TRANSLATION_fr_" {
		t.Fatalf("translation key = %s", got)
	}
	if KillBillLocale("fr-CA") != "fr_CA" || KillBillLocale("en") != "en" {
		t.Fatal("Kill Bill locales")
	}
	e := &Email{Sender: "billing@example.test", SMTP: SMTP{Host: "smtp.example.test", Port: 587, UseSSL: true, UsernameRef: "env://U", PasswordRef: "env://P"}}
	password := " p=a:s#s!w\\o\nr\td é ü"
	config, err := emailPluginConfig(e, resolved{"env://U": "user@example.test", "env://P": password})
	if err != nil {
		t.Fatal(err)
	}
	props, err := parseProperties(config)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"defaultEvents": "INVOICE_CREATION", "smtp.host": "smtp.example.test", "smtp.port": "587",
		"smtp.useAuthentication": "true", "smtp.userName": "user@example.test", "smtp.password": password,
		"smtp.useSSL": "true", "smtp.defaultSender": "billing@example.test", "smtp.sendHTMLEmail": "true",
	}
	for k, v := range want {
		if got := props.last[emailConfigPrefix+k]; got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if len(props.last) != len(want) {
		t.Errorf("properties = %v", props.last)
	}
	// Without credentials no user name or password is written.
	e = &Email{Sender: "billing@example.test", Events: []string{"INVOICE_CREATION", "INVOICE_PAYMENT_FAILED"}, SMTP: SMTP{Host: "mailpit", Port: 1025}}
	config, _ = emailPluginConfig(e, resolved{})
	if strings.Contains(config, "userName") || !strings.Contains(config, "useAuthentication=false") || !strings.Contains(config, "defaultEvents=INVOICE_CREATION,INVOICE_PAYMENT_FAILED") {
		t.Fatalf("config = %s", config)
	}
}

func TestAvaTaxTestModeRule(t *testing.T) {
	sandbox := "org.killbill.billing.plugin.avatax.url=https://sandbox-rest.avatax.com/api/v2\norg.killbill.billing.plugin.avatax.accountId=1\n"
	if err := CheckTestMode("avatax", sandbox); err != nil {
		t.Fatalf("sandbox: %v", err)
	}
	for name, config := range map[string]string{
		"production":     "org.killbill.billing.plugin.avatax.url=https://rest.avatax.com/api/v2\n",
		"plain http":     "org.killbill.billing.plugin.avatax.url=http://sandbox-rest.avatax.com/api/v2\n",
		"lookalike host": "org.killbill.billing.plugin.avatax.url=https://sandbox-rest.avatax.com.example.test/api/v2\n",
		"tax rates":      sandbox + "org.killbill.billing.plugin.avatax.taxratesapi.url=https://taxrates.example.test\n",
		"unset":          "org.killbill.billing.plugin.avatax.accountId=1\n",
	} {
		if err := CheckTestMode("avatax", config); codeOf(err) != "brand_apply_rejected" {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Every rule runs against every configuration: a production AvaTax
	// endpoint is refused under another plugin's name too.
	if err := CheckTestMode("stripe", "org.killbill.billing.plugin.stripe.apiKey=sk_test_x\norg.killbill.billing.plugin.avatax.url=https://rest.avatax.com/api/v2\n"); codeOf(err) != "brand_apply_rejected" {
		t.Fatalf("AvaTax production under Stripe: %v", err)
	}
}

func TestApprovedPinLists(t *testing.T) {
	m, err := components.Load("../../components.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if p, err := approvedPin(m.KillBillPlugins.ApprovedInvoicePlugins, "invoice", "approved_invoice_plugins", "/billing/invoice_plugin", "avatax", ""); err != nil || p.Version != "0.9.3" {
		t.Fatalf("avatax = %+v %v", p, err)
	}
	if _, err := approvedPin(m.KillBillPlugins.ApprovedInvoicePlugins, "invoice", "approved_invoice_plugins", "/billing/invoice_plugin", "stripe", ""); codeOf(err) != "plugin_unavailable" {
		t.Fatalf("a payment plugin as invoice plugin: %v", err)
	}
	if p, err := approvedPin(m.KillBillPlugins.Installable, "email", "installable", "/billing/email", EmailPluginName, ""); err != nil || p.Version != "0.8.1" || len(p.Tables) != 1 {
		t.Fatalf("email-notifications = %+v %v", p, err)
	}
	if _, err := approvedPin(nil, "email", "installable", "/billing/email", EmailPluginName, ""); codeOf(err) != "plugin_unavailable" {
		t.Fatalf("no components.yaml: %v", err)
	}
}
