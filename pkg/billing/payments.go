package billing

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/zsoftly/zs-core/internal/killbill"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/idempotency"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

// Payment is a Kill Bill payment of the tenant. ID is the Kill Bill payment
// ID. Gateway error codes and messages are never returned: they can hold
// provider text.
type Payment struct {
	ID              string        `json:"id"`
	Number          string        `json:"number"`
	Currency        string        `json:"currency"`
	PaymentMethodID string        `json:"payment_method_id"`
	AuthMinor       int64         `json:"auth_minor"`
	CapturedMinor   int64         `json:"captured_minor"`
	PurchasedMinor  int64         `json:"purchased_minor"`
	RefundedMinor   int64         `json:"refunded_minor"`
	CreditedMinor   int64         `json:"credited_minor"`
	Transactions    []Transaction `json:"transactions"`
}

// Transaction is one transaction of a payment.
type Transaction struct {
	ID            string `json:"id"`
	Type          string `json:"type"`
	Status        string `json:"status"`
	EffectiveDate string `json:"effective_date"`
	AmountMinor   int64  `json:"amount_minor"`
	Currency      string `json:"currency"`
}

func paymentOf(p killbill.Payment) (Payment, error) {
	out := Payment{ID: p.PaymentID, Number: p.PaymentNumber, Currency: p.Currency, PaymentMethodID: p.PaymentMethodID,
		Transactions: []Transaction{}}
	var err error
	for _, f := range []struct {
		v   *int64
		raw json.Number
	}{{&out.AuthMinor, p.AuthAmount}, {&out.CapturedMinor, p.CapturedAmount}, {&out.PurchasedMinor, p.PurchasedAmount},
		{&out.RefundedMinor, p.RefundedAmount}, {&out.CreditedMinor, p.CreditedAmount}} {
		if *f.v, err = ToMinor(f.raw, p.Currency); err != nil {
			return Payment{}, kbError(err)
		}
	}
	for _, t := range p.Transactions {
		currency := t.Currency
		if currency == "" {
			currency = p.Currency
		}
		amount, err := ToMinor(t.Amount, currency)
		if err != nil {
			return Payment{}, kbError(err)
		}
		out.Transactions = append(out.Transactions, Transaction{ID: t.TransactionID, Type: t.TransactionType,
			Status: t.Status, EffectiveDate: t.EffectiveDate, AmountMinor: amount, Currency: currency})
	}
	return out, nil
}

// ListPayments returns one page of the tenant's payments, newest first.
func (s *Service) ListPayments(ctx context.Context, sc scope.Scope, page tenant.Page) ([]Payment, string, error) {
	a, err := s.account(ctx, sc)
	if err != nil {
		return nil, "", err
	}
	list, err := s.KillBill.AccountPayments(ctx, a.creds, a.id())
	if err != nil {
		return nil, "", kbError(err)
	}
	pageItems, next, err := pageByNumber(list, func(p killbill.Payment) int64 { return numberOf(p.PaymentNumber) }, page)
	if err != nil {
		return nil, "", err
	}
	out := make([]Payment, 0, len(pageItems))
	for _, p := range pageItems {
		pay, err := paymentOf(p)
		if err != nil {
			return nil, "", err
		}
		out = append(out, pay)
	}
	return out, next, nil
}

func checkReason(reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" || utf8.RuneCountInString(reason) > 1000 || !utf8.ValidString(reason) {
		return zserrors.New(zserrors.CodeReasonRequired, "")
	}
	return nil
}

// Refund refunds amountMinor of a payment of the tenant, in the payment's
// currency. Staff with a billing grant call it. It needs a reason and is
// idempotent on idempotencyKey for the actor in the tenant. The refund
// transaction's external key is derived from the key, so Kill Bill refuses a
// second refund for one key, and a retry after a lost answer returns the
// payment.
func (s *Service) Refund(ctx context.Context, sc scope.Scope, call Call, paymentID string, amountMinor int64, idempotencyKey string) (Payment, bool, error) {
	if err := call.validate(); err != nil {
		return Payment{}, false, err
	}
	if err := checkReason(call.Reason); err != nil {
		return Payment{}, false, err
	}
	if amountMinor <= 0 {
		return Payment{}, false, validation("The amount_minor must be positive.")
	}
	if err := idempotency.ValidateKey(idempotencyKey); err != nil {
		return Payment{}, false, err
	}
	a, err := s.account(ctx, sc)
	if err != nil {
		return Payment{}, false, err
	}
	if err := checkKillBillID(paymentID); err != nil {
		return Payment{}, false, err
	}
	var out Payment
	payload := map[string]any{"payment": paymentID, "amount_minor": amountMinor, "reason": call.Reason}
	replayed, err := s.idempotent(ctx, sc, call.Actor.ID, idempotencyKey, "billing.payment.refund", payload, &out,
		func(ctx context.Context) (any, error) {
			p, err := s.KillBill.Payment(ctx, a.creds, paymentID)
			if err != nil {
				return nil, kbError(err)
			}
			if err := owned(a, p.AccountID); err != nil {
				return nil, err
			}
			amount, err := FromMinor(amountMinor, p.Currency)
			if err != nil {
				return nil, validation("The payment currency is not supported.")
			}
			loaded, err := paymentOf(p)
			if err != nil {
				return nil, err
			}
			key := derivedKey("refund", a, call.Actor.ID, idempotencyKey)
			done := refundTransaction(p, key)
			// A refund of this key that Kill Bill already made counts in
			// RefundedMinor, so the bound applies to new refunds only.
			if done == "" && amountMinor > refundable(loaded) {
				return nil, zserrors.New(zserrors.CodeBillingRejected, "The refund is larger than the amount captured and not yet refunded: refund_exceeds_payment.")
			}
			if done == "" {
				err = s.KillBill.Refund(ctx, a.creds, call.kb(), paymentID, killbill.PaymentTransaction{
					Amount: amount, Currency: p.Currency, TransactionExternalKey: key,
				})
				if err != nil && !errors.Is(err, killbill.ErrTransactionKeyExists) {
					return nil, kbError(err)
				}
			}
			after, err := s.KillBill.Payment(ctx, a.creds, paymentID)
			if err != nil {
				return nil, kbError(err)
			}
			details := map[string]any{"killbill_account_id": a.id(), "killbill_payment_id": paymentID, "amount_minor": amountMinor, "currency": p.Currency}
			if id := refundTransaction(after, key); id != "" {
				details["killbill_transaction_id"] = id
			}
			if err := s.audit(ctx, sc, call, "billing.payment.refunded", details); err != nil {
				return nil, err
			}
			return paymentOf(after)
		})
	return out, replayed, err
}

// refundable is what a payment can still refund: the amount captured or
// purchased less the amount already refunded.
func refundable(p Payment) int64 {
	return p.CapturedMinor + p.PurchasedMinor - p.RefundedMinor
}

// refundTransaction returns the ID of the refund of a payment whose
// transaction external key is key, or "". A refund that failed does not
// count: a retry may refund again.
func refundTransaction(p killbill.Payment, key string) string {
	for _, t := range p.Transactions {
		failed := t.Status == "PAYMENT_FAILURE" || t.Status == "PLUGIN_FAILURE" || t.Status == "PAYMENT_SYSTEM_OFF"
		if t.TransactionType == "REFUND" && t.TransactionExternalKey == key && !failed {
			return t.TransactionID
		}
	}
	return ""
}

// PaymentMethod is a payment method of the tenant. ID is the Kill Bill
// payment method ID. Card data and plugin details are never returned.
type PaymentMethod struct {
	ID         string `json:"id"`
	PluginName string `json:"plugin_name"`
	IsDefault  bool   `json:"is_default"`
}

func paymentMethodOf(pm killbill.PaymentMethod) PaymentMethod {
	return PaymentMethod{ID: pm.PaymentMethodID, PluginName: pm.PluginName, IsDefault: pm.IsDefault}
}

// ListPaymentMethods returns the tenant's payment methods.
func (s *Service) ListPaymentMethods(ctx context.Context, sc scope.Scope) ([]PaymentMethod, error) {
	a, err := s.account(ctx, sc)
	if err != nil {
		return nil, err
	}
	list, err := s.KillBill.AccountPaymentMethods(ctx, a.creds, a.id())
	if err != nil {
		return nil, kbError(err)
	}
	out := make([]PaymentMethod, 0, len(list))
	for _, pm := range list {
		out = append(out, paymentMethodOf(pm))
	}
	return out, nil
}

// Property is a key and value passed to the brand's payment plugin, such as
// a hosted page field. Properties are never logged.
type Property struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

var propertyKey = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

func checkProperties(field string, props []Property) ([]killbill.PluginProperty, error) {
	if len(props) > 20 {
		return nil, validation("Give at most 20 " + field + ".")
	}
	out := make([]killbill.PluginProperty, 0, len(props))
	for _, p := range props {
		if !propertyKey.MatchString(p.Key) || len(p.Value) > 2048 || !utf8.ValidString(p.Value) {
			return nil, validation("Each of " + field + " needs a key of 1 to 64 letters, digits, dots, dashes or underscores and a value of at most 2048 bytes.")
		}
		out = append(out, killbill.PluginProperty{Key: p.Key, Value: p.Value})
	}
	return out, nil
}

// NewPaymentMethod asks the brand's payment plugin for a payment method.
// With HostedPage set, the plugin returns the form of its hosted payment
// page and the customer completes it with the provider. Otherwise
// ExternalPaymentMethodID carries the token the provider's own client
// library made in the browser, such as a Stripe payment method ID. Core
// never sees card data either way.
type NewPaymentMethod struct {
	IdempotencyKey          string
	SetDefault              bool
	ExternalPaymentMethodID string
	PluginProperties        []Property
	HostedPage              bool
	HostedPageFields        []Property
}

// HostedForm is what the plugin returns to send the customer to its hosted
// payment page: a URL, a method and the fields to send, which can hold the
// provider's client-side token. A plugin without a hosted page returns an
// empty form.
type HostedForm struct {
	Method string         `json:"method,omitempty"`
	URL    string         `json:"url,omitempty"`
	Fields map[string]any `json:"fields"`
}

// AddedPaymentMethod is the result of AddPaymentMethod.
type AddedPaymentMethod struct {
	PaymentMethod *PaymentMethod `json:"payment_method"`
	Form          *HostedForm    `json:"form,omitempty"`
}

// AddPaymentMethod adds a payment method to the tenant through the brand's
// payment plugin. It is idempotent on the idempotency key for the actor in
// the tenant, and the Kill Bill external key of the payment method is
// derived from it.
func (s *Service) AddPaymentMethod(ctx context.Context, sc scope.Scope, call Call, req NewPaymentMethod) (AddedPaymentMethod, bool, error) {
	if err := call.validate(); err != nil {
		return AddedPaymentMethod{}, false, err
	}
	if err := idempotency.ValidateKey(req.IdempotencyKey); err != nil {
		return AddedPaymentMethod{}, false, err
	}
	if len(req.ExternalPaymentMethodID) > 255 || strings.ContainsAny(req.ExternalPaymentMethodID, "\x00\r\n") {
		return AddedPaymentMethod{}, false, validation("The external_payment_method_id must be at most 255 characters.")
	}
	if req.HostedPage && req.ExternalPaymentMethodID != "" {
		return AddedPaymentMethod{}, false, validation("Give either hosted_page or external_payment_method_id.")
	}
	props, err := checkProperties("plugin_properties", req.PluginProperties)
	if err != nil {
		return AddedPaymentMethod{}, false, err
	}
	fields, err := checkProperties("hosted_page_fields", req.HostedPageFields)
	if err != nil {
		return AddedPaymentMethod{}, false, err
	}
	a, err := s.account(ctx, sc)
	if err != nil {
		return AddedPaymentMethod{}, false, err
	}
	// The fingerprint covers every field; the values are hashed, never
	// stored.
	payload := map[string]any{"set_default": req.SetDefault, "external_payment_method_id": req.ExternalPaymentMethodID,
		"plugin_properties": req.PluginProperties, "hosted_page": req.HostedPage, "hosted_page_fields": req.HostedPageFields}
	var out AddedPaymentMethod
	replayed, err := s.idempotent(ctx, sc, call.Actor.ID, req.IdempotencyKey, "billing.payment_method.add", payload, &out,
		func(ctx context.Context) (any, error) {
			externalKey := derivedKey("pm", a, call.Actor.ID, req.IdempotencyKey)
			if pm, ok, err := s.paymentMethodByKey(ctx, a, externalKey); err != nil || ok {
				if err != nil {
					return nil, err
				}
				// An earlier attempt made it and lost the answer.
				if err := s.auditPaymentMethod(ctx, sc, call, "billing.payment_method.added", a, pm.ID); err != nil {
					return nil, err
				}
				return AddedPaymentMethod{PaymentMethod: &pm}, nil
			}
			newPM := killbill.PaymentMethod{PluginName: a.pluginName(), ExternalKey: externalKey}
			var form *HostedForm
			if req.HostedPage {
				desc, err := s.KillBill.HostedPaymentPage(ctx, a.creds, call.kb(), a.id(), newPM, fields, props)
				if err != nil {
					return nil, kbError(err)
				}
				form = &HostedForm{Method: desc.FormMethod, URL: desc.FormURL, Fields: desc.FormFields}
				if form.Fields == nil {
					form.Fields = map[string]any{}
				}
			} else {
				newPM.PluginInfo = &killbill.PaymentMethodPluginInfo{ExternalPaymentMethodID: req.ExternalPaymentMethodID, Properties: props}
				if _, err := s.KillBill.AddPaymentMethod(ctx, a.creds, call.kb(), a.id(), newPM, req.SetDefault); err != nil {
					return nil, kbError(err)
				}
			}
			pm, ok, err := s.paymentMethodByKey(ctx, a, externalKey)
			if err != nil {
				return nil, err
			}
			if !ok {
				// The plugin adds the payment method once the customer
				// completes its hosted page.
				if err := s.auditPaymentMethod(ctx, sc, call, "billing.payment_method.added", a, ""); err != nil {
					return nil, err
				}
				return AddedPaymentMethod{Form: form}, nil
			}
			if req.HostedPage && req.SetDefault && !pm.IsDefault {
				if err := s.KillBill.SetDefaultPaymentMethod(ctx, a.creds, call.kb(), a.id(), pm.ID); err != nil {
					return nil, kbError(err)
				}
				pm.IsDefault = true
			}
			if err := s.auditPaymentMethod(ctx, sc, call, "billing.payment_method.added", a, pm.ID); err != nil {
				return nil, err
			}
			return AddedPaymentMethod{PaymentMethod: &pm, Form: form}, nil
		})
	return out, replayed, err
}

// auditPaymentMethod audits a payment method write with the Kill Bill IDs
// only.
func (s *Service) auditPaymentMethod(ctx context.Context, sc scope.Scope, call Call, action string, a account, id string) error {
	details := map[string]any{"killbill_account_id": a.id()}
	if id != "" {
		details["killbill_payment_method_id"] = id
	}
	return s.audit(ctx, sc, call, action, details)
}

func (s *Service) paymentMethodByKey(ctx context.Context, a account, externalKey string) (PaymentMethod, bool, error) {
	list, err := s.KillBill.AccountPaymentMethods(ctx, a.creds, a.id())
	if err != nil {
		return PaymentMethod{}, false, kbError(err)
	}
	for _, pm := range list {
		if pm.ExternalKey == externalKey {
			return paymentMethodOf(pm), true, nil
		}
	}
	return PaymentMethod{}, false, nil
}

func (s *Service) ownedPaymentMethod(ctx context.Context, a account, id string) (killbill.PaymentMethod, error) {
	if err := checkKillBillID(id); err != nil {
		return killbill.PaymentMethod{}, err
	}
	pm, err := s.KillBill.PaymentMethod(ctx, a.creds, id)
	if err != nil {
		return killbill.PaymentMethod{}, kbError(err)
	}
	if err := owned(a, pm.AccountID); err != nil {
		return killbill.PaymentMethod{}, err
	}
	return pm, nil
}

// SetDefaultPaymentMethod makes a payment method of the tenant its default.
func (s *Service) SetDefaultPaymentMethod(ctx context.Context, sc scope.Scope, call Call, id string) (PaymentMethod, error) {
	if err := call.validate(); err != nil {
		return PaymentMethod{}, err
	}
	a, err := s.account(ctx, sc)
	if err != nil {
		return PaymentMethod{}, err
	}
	pm, err := s.ownedPaymentMethod(ctx, a, id)
	if err != nil {
		return PaymentMethod{}, err
	}
	if err := s.KillBill.SetDefaultPaymentMethod(ctx, a.creds, call.kb(), a.id(), id); err != nil {
		return PaymentMethod{}, kbError(err)
	}
	if err := s.auditPaymentMethod(ctx, sc, call, "billing.payment_method.default_set", a, id); err != nil {
		return PaymentMethod{}, err
	}
	pm.IsDefault = true
	return paymentMethodOf(pm), nil
}

// DeletePaymentMethod deletes a payment method of the tenant. Kill Bill
// refuses to delete the default one: billing_rejected.
func (s *Service) DeletePaymentMethod(ctx context.Context, sc scope.Scope, call Call, id string) error {
	if err := call.validate(); err != nil {
		return err
	}
	a, err := s.account(ctx, sc)
	if err != nil {
		return err
	}
	if _, err := s.ownedPaymentMethod(ctx, a, id); err != nil {
		return err
	}
	if err := s.KillBill.DeletePaymentMethod(ctx, a.creds, call.kb(), id); err != nil {
		return kbError(err)
	}
	return s.auditPaymentMethod(ctx, sc, call, "billing.payment_method.deleted", a, id)
}
