package killbill

import (
	"context"
	"errors"
	"net/http"
	"net/url"
)

// Tenant keys under which Kill Bill 0.24.22 stores the invoice templates and
// translations of a tenant. Each upload endpoint below writes one of them,
// and Core reads them back through the user key-value endpoint to compare.
const (
	KeyInvoiceTemplate          = "INVOICE_TEMPLATE"
	KeyInvoiceManualPayTemplate = "INVOICE_MP_TEMPLATE"
	keyInvoiceTranslation       = "INVOICE_TRANSLATION_"
	keyCatalogTranslation       = "CATALOG_TRANSLATION_"
)

// InvoiceTranslationKey is the tenant key of the invoice translation of a
// Kill Bill locale, such as fr or en_CA.
func InvoiceTranslationKey(locale string) string { return keyInvoiceTranslation + locale }

// CatalogTranslationKey is the tenant key of the catalogue translation of a
// Kill Bill locale. The email-notifications plugin reads the same key.
func CatalogTranslationKey(locale string) string { return keyCatalogTranslation + locale }

// TenantValue returns the last value of a tenant key, and whether it has
// one. It reads system keys, such as INVOICE_TEMPLATE, as well as user keys.
func (c *Client) TenantValue(ctx context.Context, creds TenantCredentials, key string) (string, bool, error) {
	values, err := c.UserKeyValues(ctx, creds, key)
	if errors.Is(err, ErrNotFound) {
		return "", false, nil
	}
	if err != nil || len(values) == 0 {
		return "", false, err
	}
	return values[len(values)-1], true, nil
}

// uploadText posts a text document with deleteIfExists=true, which replaces
// any earlier one. Kill Bill 0.24.22 answers 400 to a second upload without
// it.
func (c *Client) uploadText(ctx context.Context, op, path, contentType string, creds TenantCredentials, cc CallContext, body string) error {
	q := url.Values{"deleteIfExists": {"true"}}
	_, err := c.do(ctx, request{op: op, method: http.MethodPost, path: path + "?" + q.Encode(), tenant: &creds, call: &cc,
		contentType: contentType, accept: contentType, body: []byte(body)})
	return err
}

// UploadInvoiceTemplate replaces the tenant's HTML invoice template, a
// Mustache template Kill Bill renders for GET /1.0/kb/invoices/{id}/html.
func (c *Client) UploadInvoiceTemplate(ctx context.Context, creds TenantCredentials, cc CallContext, html string) error {
	return c.uploadText(ctx, "upload invoice template", "/1.0/kb/invoices/template", "text/html", creds, cc, html)
}

// UploadManualPayTemplate replaces the template Kill Bill renders for an
// account that pays manually.
func (c *Client) UploadManualPayTemplate(ctx context.Context, creds TenantCredentials, cc CallContext, html string) error {
	return c.uploadText(ctx, "upload manual pay template", "/1.0/kb/invoices/manualPayTemplate", "text/html", creds, cc, html)
}

// UploadInvoiceTranslation replaces the invoice translation of a Kill Bill
// locale: Java properties whose keys the template reads as text.<key>.
func (c *Client) UploadInvoiceTranslation(ctx context.Context, creds TenantCredentials, cc CallContext, locale, properties string) error {
	return c.uploadText(ctx, "upload invoice translation", "/1.0/kb/invoices/translation/"+url.PathEscape(locale), "text/plain", creds, cc, properties)
}

// UploadCatalogTranslation replaces the catalogue translation of a Kill Bill
// locale: Java properties that give plan, product and phase names.
func (c *Client) UploadCatalogTranslation(ctx context.Context, creds TenantCredentials, cc CallContext, locale, properties string) error {
	return c.uploadText(ctx, "upload catalogue translation", "/1.0/kb/invoices/catalogTranslation/"+url.PathEscape(locale), "text/plain", creds, cc, properties)
}

// InvoiceHTML returns Kill Bill's HTML rendering of an invoice, in the
// locale of the invoice's account, with the tenant's template and
// translations, or Kill Bill's defaults when the tenant has none.
func (c *Client) InvoiceHTML(ctx context.Context, creds TenantCredentials, invoiceID string) ([]byte, error) {
	resp, err := c.do(ctx, request{op: "get invoice html", method: http.MethodGet,
		path: "/1.0/kb/invoices/" + url.PathEscape(invoiceID) + "/html", tenant: &creds, accept: "text/html"})
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}
