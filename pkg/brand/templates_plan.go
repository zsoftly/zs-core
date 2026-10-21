package brand

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/zsoftly/zs-core/internal/killbill"
)

// Targets of the template changes of an apply.
const (
	TargetInvoiceTemplate      = "killbill.invoice_template"
	TargetInvoiceTranslation   = "killbill.invoice_translation"
	TargetCatalogueTranslation = "killbill.catalogue_translation"
	TargetEmailTemplate        = "killbill.email_template"
)

// textDocument is one text Kill Bill stores under a tenant key, which apply
// uploads when its SHA-256 differs from the text's.
type textDocument struct {
	target string
	field  string
	key    string
	text   string
	upload func(ctx context.Context, cc killbill.CallContext, text string) error
}

// fileText returns the text of a referenced file and whether the manifest
// names it and it was read.
func fileText(fs files, path string) (string, bool) {
	if path == "" {
		return "", false
	}
	data, ok := fs[path]
	return string(data), ok
}

// planTextDocuments compares each document with what Kill Bill holds under
// its key and plans an upload of each one that differs.
func planTextDocuments(ctx context.Context, kb *killbill.Client, creds killbill.TenantCredentials, exists bool, docs []textDocument) ([]Change, error) {
	var changes []Change
	for _, d := range docs {
		want := fileDigest([]byte(d.text))
		var have string
		action := ActionCreate
		if exists {
			value, found, err := kb.TenantValue(ctx, creds, d.key)
			if err != nil {
				return nil, err
			}
			if found {
				have = fileDigest([]byte(value))
				action = ActionUpdate
			}
		}
		if have == want {
			continue
		}
		d, text := d, d.text
		changes = append(changes, Change{
			Target: d.target, Name: d.key, Action: action,
			Fields: []FieldChange{{Field: d.field + ".sha256", Before: nilIfEmpty(have), After: want}},
			run: func(ctx context.Context, st *runState) error {
				return d.upload(ctx, st.callContext(), text)
			},
		})
	}
	return changes, nil
}

// planInvoiceDocuments plans the invoice template, the manual pay template
// and the invoice and catalogue translations of every locale the manifest
// names.
func planInvoiceDocuments(ctx context.Context, kb *killbill.Client, creds killbill.TenantCredentials, exists bool, m *Manifest, fs files) ([]Change, error) {
	t := m.Billing.InvoiceTemplates
	if t == nil {
		return nil, nil
	}
	var docs []textDocument
	if text, ok := fileText(fs, t.Template); ok {
		docs = append(docs, textDocument{target: TargetInvoiceTemplate, field: "billing.invoice_templates.template", key: killbill.KeyInvoiceTemplate, text: text,
			upload: func(ctx context.Context, cc killbill.CallContext, text string) error {
				return kb.UploadInvoiceTemplate(ctx, creds, cc, text)
			}})
	}
	if text, ok := fileText(fs, t.ManualPayTemplate); ok {
		docs = append(docs, textDocument{target: TargetInvoiceTemplate, field: "billing.invoice_templates.manual_pay_template", key: killbill.KeyInvoiceManualPayTemplate, text: text,
			upload: func(ctx context.Context, cc killbill.CallContext, text string) error {
				return kb.UploadManualPayTemplate(ctx, creds, cc, text)
			}})
	}
	// Every locale of the brand gets a translation, so every invoice shows
	// the seller of the manifest, with the brand's file for the locale
	// when it has one.
	seller := sellerTranslation(m.Billing.Seller)
	for _, l := range m.Identity.Locales {
		locale := KillBillLocale(l)
		text, _ := fileText(fs, t.Translations[l])
		docs = append(docs, textDocument{target: TargetInvoiceTranslation, field: "billing.invoice_templates.translations." + l, key: killbill.InvoiceTranslationKey(locale), text: text + seller,
			upload: func(ctx context.Context, cc killbill.CallContext, text string) error {
				return kb.UploadInvoiceTranslation(ctx, creds, cc, locale, text)
			}})
	}
	for _, l := range sortedLocales(t.CatalogueTranslations) {
		locale := KillBillLocale(l)
		text, ok := fileText(fs, t.CatalogueTranslations[l])
		if !ok {
			continue
		}
		docs = append(docs, textDocument{target: TargetCatalogueTranslation, field: "billing.invoice_templates.catalogue_translations." + l, key: killbill.CatalogTranslationKey(locale), text: text,
			upload: func(ctx context.Context, cc killbill.CallContext, text string) error {
				return kb.UploadCatalogTranslation(ctx, creds, cc, locale, text)
			}})
	}
	return planTextDocuments(ctx, kb, creds, exists, docs)
}

// planEmailTemplates plans the email templates and translations of the
// brand's email templates directory. Each locale of the brand may override
// any template type and the translation. A file that is absent is not
// uploaded, and the plugin keeps its built-in template for that type.
func planEmailTemplates(ctx context.Context, kb *killbill.Client, creds killbill.TenantCredentials, exists bool, m *Manifest, fs files) ([]Change, error) {
	e := m.Billing.Email
	if e == nil || e.Templates == "" {
		return nil, nil
	}
	var docs []textDocument
	for _, l := range m.Identity.Locales {
		types := make([]string, 0, len(EmailTemplateFiles))
		for typ := range EmailTemplateFiles {
			types = append(types, typ)
		}
		sort.Strings(types)
		for _, typ := range types {
			text, ok := fileText(fs, emailFilePath(e, l, EmailTemplateFiles[typ]))
			if !ok {
				continue
			}
			key := EmailTemplateKey(typ, l)
			docs = append(docs, textDocument{target: TargetEmailTemplate, field: "billing.email.templates." + l + "." + typ, key: key,
				text: text, upload: userKeyUpload(kb, creds, key)})
		}
		// A translation replaces every text of the plugin for the locale,
		// so Core adds the seller only to a translation the brand provides.
		if text, ok := fileText(fs, emailFilePath(e, l, EmailTranslationFile)); ok {
			key := EmailTranslationKey(l)
			docs = append(docs, textDocument{target: TargetEmailTemplate, field: "billing.email.templates." + l + ".translation", key: key,
				text: text + sellerTranslation(m.Billing.Seller), upload: userKeyUpload(kb, creds, key)})
		}
	}
	return planTextDocuments(ctx, kb, creds, exists, docs)
}

func emailFilePath(e *Email, locale, file string) string {
	return strings.TrimRight(e.Templates, "/") + "/" + locale + "/" + file
}

func userKeyUpload(kb *killbill.Client, creds killbill.TenantCredentials, key string) func(context.Context, killbill.CallContext, string) error {
	return func(ctx context.Context, cc killbill.CallContext, text string) error {
		return kb.SetUserKeyValue(ctx, creds, cc, key, text)
	}
}

// emailConfigPrefix is the prefix of every per-tenant property of the
// email-notifications plugin 0.8.1.
const emailConfigPrefix = "org.killbill.billing.plugin.email-notifications."

// emailPluginConfig builds the per-tenant configuration of the
// email-notifications plugin from the manifest and the resolved SMTP
// credentials. The keys are the ones the plugin 0.8.1 reads.
func emailPluginConfig(e *Email, sec resolved) (string, error) {
	auth := e.SMTP.UsernameRef != ""
	props := [][2]string{
		{"defaultEvents", strings.Join(e.EventsOrDefault(), ",")},
		{"smtp.host", e.SMTP.Host},
		{"smtp.port", strconv.Itoa(e.SMTP.Port)},
		{"smtp.useAuthentication", strconv.FormatBool(auth)},
	}
	if auth {
		props = append(props, [2]string{"smtp.userName", sec[e.SMTP.UsernameRef]}, [2]string{"smtp.password", sec[e.SMTP.PasswordRef]})
	}
	props = append(props,
		[2]string{"smtp.useSSL", strconv.FormatBool(e.SMTP.UseSSL)},
		[2]string{"smtp.defaultSender", e.Sender},
		[2]string{"smtp.sendHTMLEmail", "true"},
	)
	var b strings.Builder
	for _, p := range props {
		fmt.Fprintf(&b, "%s%s=%s\n", emailConfigPrefix, p[0], escapePropertyValue(p[1]))
	}
	return b.String(), nil
}

// escapePropertyValue escapes a value for a Java properties text, so
// java.util.Properties.load reads back exactly the value: backslashes,
// line breaks, tabs, form feeds, a leading blank and every character
// outside printable ASCII.
func escapePropertyValue(v string) string {
	var b strings.Builder
	for i, r := range v {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\f':
			b.WriteString(`\f`)
		case r == ' ' && i == 0:
			b.WriteString(`\ `)
		case r == '=' || r == ':' || r == '#' || r == '!':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r > 0x7e:
			if r > 0xffff {
				for _, s := range utf16Pair(r) {
					fmt.Fprintf(&b, `\u%04X`, s)
				}
			} else {
				fmt.Fprintf(&b, `\u%04X`, r)
			}
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func utf16Pair(r rune) [2]rune {
	r -= 0x10000
	return [2]rune{0xd800 + (r>>10)&0x3ff, 0xdc00 + r&0x3ff}
}
