package brand

import (
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"
)

// Email template files of the email-notifications plugin 0.8.1, by the
// template type the plugin stores them under. A brand's templates directory
// holds them per locale, <templates>/<locale>/<file>, with the plugin's own
// file names. Each file is optional: the plugin uses its built-in template
// for a type the brand does not override.
var EmailTemplateFiles = map[string]string{
	"UPCOMING_INVOICE":                    "UpcomingInvoice.mustache",
	"SUCCESSFUL_PAYMENT":                  "SuccessfulPayment.mustache",
	"FAILED_PAYMENT":                      "FailedPayment.mustache",
	"PAYMENT_REFUND":                      "PaymentRefund.mustache",
	"SUBSCRIPTION_CANCELLATION_REQUESTED": "SubscriptionCancellationRequested.mustache",
	"SUBSCRIPTION_CANCELLATION_EFFECTIVE": "SubscriptionCancellationEffective.mustache",
	"INVOICE_CREATION":                    "InvoiceCreation.mustache",
}

// EmailTranslationFile is the translation file of a locale in a brand's
// email templates directory. The plugin reads its keys as text.<key>.
const EmailTranslationFile = "Translation.properties"

// EmailPluginName is the name of the email-notifications plugin in
// components.yaml and KPM.
const EmailPluginName = "email-notifications"

// emailPluginService is the service name the email-notifications plugin
// registers, under which its per-tenant configuration and templates live.
const emailPluginService = "killbill-email-notifications"

// KillBillLocale turns a brand locale, such as fr or en-CA, into the locale
// string Kill Bill stores on an account and in its tenant keys, fr or en_CA.
func KillBillLocale(locale string) string { return strings.ReplaceAll(locale, "-", "_") }

// emailLocaleSuffix is the suffix the email-notifications plugin 0.8.1 adds
// to a tenant key for a locale: _<language>_<country>, with an empty
// country for a locale without one, as java.util.Locale gives them. A
// template for an account in locale fr is read from <key>_fr_.
func emailLocaleSuffix(locale string) string {
	lang, country, _ := strings.Cut(locale, "-")
	return "_" + lang + "_" + country
}

// EmailTemplateKey is the tenant key the email-notifications plugin 0.8.1
// reads a template of a type and a brand locale from.
func EmailTemplateKey(templateType, locale string) string {
	return emailPluginService + ":" + templateType + emailLocaleSuffix(locale)
}

// EmailTranslationKey is the tenant key the plugin reads the translation of
// a brand locale from.
func EmailTranslationKey(locale string) string {
	return emailPluginService + ":TEMPLATE_TRANSLATION" + emailLocaleSuffix(locale)
}

// sortedLocales returns the keys of a locale map, sorted.
func sortedLocales(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// filePaths lists every file the billing section names, by manifest path.
func (b Billing) filePaths() map[string]string {
	out := map[string]string{
		"/billing/catalogue": b.Catalogue,
		"/billing/overdue":   b.Overdue,
		"/billing/templates": b.Templates,
	}
	if t := b.InvoiceTemplates; t != nil {
		out["/billing/invoice_templates/template"] = t.Template
		out["/billing/invoice_templates/manual_pay_template"] = t.ManualPayTemplate
		for l, f := range t.Translations {
			out["/billing/invoice_templates/translations/"+l] = f
		}
		for l, f := range t.CatalogueTranslations {
			out["/billing/invoice_templates/catalogue_translations/"+l] = f
		}
	}
	if e := b.Email; e != nil {
		out["/billing/email/templates"] = e.Templates
	}
	return out
}

// emailTemplatePaths lists the files of the brand's email templates
// directory that apply could upload, by manifest path: every template file
// and the translation file of every locale of the brand.
func (m *Manifest) emailTemplatePaths() map[string]string {
	out := map[string]string{}
	e := m.Billing.Email
	if e == nil || e.Templates == "" {
		return out
	}
	for _, l := range m.Identity.Locales {
		for typ, file := range EmailTemplateFiles {
			out["/billing/email/templates/"+l+"/"+typ] = path.Join(e.Templates, l, file)
		}
		out["/billing/email/templates/"+l+"/translation"] = path.Join(e.Templates, l, EmailTranslationFile)
	}
	return out
}

// billingPluginProblems checks the plugins, templates and email of the
// billing section: the rules the schema cannot express.
func (m *Manifest) billingPluginProblems() []string {
	var problems []string
	fail := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	b := m.Billing
	if b.Seller.Address != "" && len(b.Seller.AddressLines) > 0 {
		fail("/billing/seller: set address_lines or its one-line form address, not both")
	}
	if b.PaymentPlugin != nil && len(b.PaymentPlugins) > 0 {
		fail("/billing/payment_plugins: set payment_plugins or its one-plugin form payment_plugin, not both")
	}
	if b.PaymentPlugin != nil && b.PaymentPlugin.Default {
		fail("/billing/payment_plugin/default: the one plugin of payment_plugin is the default; remove default or use payment_plugins")
	}
	names := map[string]bool{}
	defaults := 0
	for i, pp := range b.Plugins() {
		at := b.pluginsPath(i)
		if pp.Mode != PaymentModeTest {
			fail("%s/mode: must be test, because v1 refuses live and no payment plugin may take real money", at)
		}
		// Kill Bill hands a plugin the configuration stored under its own
		// service name. Another key would upload a configuration that
		// another plugin reads, past that plugin's test-mode rule.
		if pp.ConfigKey != "" && pp.ConfigKey != "killbill-"+pp.Name {
			fail("%s/config_key: must be empty or killbill-%s, the service name of the plugin", at, pp.Name)
		}
		if names[pp.Name] {
			fail("%s/name: %s is listed twice", at, pp.Name)
		}
		names[pp.Name] = true
		if pp.Default {
			defaults++
		}
	}
	if len(b.PaymentPlugins) > 0 && defaults != 1 {
		fail("/billing/payment_plugins: exactly one plugin must be default: true, found %d", defaults)
	}
	if ip := b.InvoicePlugin; ip != nil {
		if ip.Mode != PaymentModeTest {
			fail("/billing/invoice_plugin/mode: must be test, because v1 accepts only the test or sandbox mode of a plugin")
		}
		if ip.ConfigKey != "" && ip.ConfigKey != ip.Key() {
			fail("/billing/invoice_plugin/config_key: must be empty or %s, the service name of the plugin", ip.Key())
		}
		if names[ip.Name] {
			fail("/billing/invoice_plugin/name: %s is also a payment plugin of the brand", ip.Name)
		}
	}
	if t := b.InvoiceTemplates; t != nil {
		for _, set := range []struct {
			path string
			m    map[string]string
		}{{"/billing/invoice_templates/translations", t.Translations}, {"/billing/invoice_templates/catalogue_translations", t.CatalogueTranslations}} {
			for _, l := range sortedLocales(set.m) {
				if !slices.Contains(m.Identity.Locales, l) {
					fail("%s/%s: %s is not a locale of /identity/locales", set.path, l, l)
				}
			}
		}
	}
	if e := b.Email; e != nil {
		if e.SMTP.Port < 1 || e.SMTP.Port > 65535 {
			fail("/billing/email/smtp/port: must be 1 to 65535")
		}
		if (e.SMTP.UsernameRef == "") != (e.SMTP.PasswordRef == "") {
			fail("/billing/email/smtp: username_ref and password_ref are set together or not at all")
		}
		seen := map[string]bool{}
		for i, ev := range e.Events {
			if !slices.Contains(EmailEvents, ev) || seen[ev] {
				fail("/billing/email/events/%d: must be one of %s, once", i, strings.Join(EmailEvents, ", "))
			}
			seen[ev] = true
		}
		if names[EmailPluginName] || (b.InvoicePlugin != nil && b.InvoicePlugin.Name == EmailPluginName) {
			fail("/billing/email: %s is also named as a payment or invoice plugin", EmailPluginName)
		}
	}
	return problems
}

// sellerTranslation is the block of properties apply appends to every
// invoice and email translation it uploads, so Kill Bill and the
// email-notifications plugin print the legal seller of the manifest. The
// company keys are the ones Kill Bill's and the plugin's own templates
// read. The values of the manifest win over the same keys in a file.
func sellerTranslation(s Seller) string {
	lines := s.Lines()
	props := [][2]string{
		{"sellerName", s.Name},
		{"sellerAddress", strings.Join(lines, ", ")},
		{"sellerCountry", s.Country},
		{"sellerTaxRegistrations", strings.Join(s.TaxRegistrations, "; ")},
		{"companyName", s.Name},
		{"companyCountry", s.Country},
	}
	for i, l := range lines {
		props = append(props, [2]string{fmt.Sprintf("sellerAddressLine%d", i+1), l})
	}
	for i, r := range s.TaxRegistrations {
		props = append(props, [2]string{fmt.Sprintf("sellerTaxRegistration%d", i+1), r})
	}
	first, rest := "", ""
	if len(lines) > 0 {
		first, rest = lines[0], strings.Join(lines[1:], ", ")
	}
	props = append(props, [2]string{"companyAddress", first}, [2]string{"companyCityProvincePostalCode", rest})
	var b strings.Builder
	b.WriteString("\n# Written by zs-core brand apply from billing.seller of the brand manifest.\n")
	for _, p := range props {
		fmt.Fprintf(&b, "%s=%s\n", p[0], escapePropertyValue(p[1]))
	}
	return b.String()
}
