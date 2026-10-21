package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/zsoftly/zs-core/internal/httpapi/gen"
	"github.com/zsoftly/zs-core/pkg/billing"
	"github.com/zsoftly/zs-core/pkg/brand"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/identity"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

// writeAs writes src after checking that it decodes into the generated type
// dst with no unknown field, so every billing response matches the
// OpenAPI document.
func (s *server) writeAs(w http.ResponseWriter, r *http.Request, status int, src, dst any) {
	encoded, err := json.Marshal(src)
	if err == nil {
		dec := json.NewDecoder(bytes.NewReader(encoded))
		dec.DisallowUnknownFields()
		err = dec.Decode(dst)
	}
	if err != nil {
		s.writeError(w, r, zserrors.Wrap(zserrors.CodeInternal, "", err))
		return
	}
	s.writeJSON(w, r, status, dst)
}

// billingService returns the billing service, or not_ready.
func (s *server) billingService() (*billing.Service, error) {
	if !s.billing.Ready() {
		return nil, zserrors.New(zserrors.CodeNotReady, "Billing needs the Kill Bill admin settings.")
	}
	return s.billing, nil
}

// billingCall builds the Kill Bill audit context of a tenant route.
func billingCall(r *http.Request, caller tenant.Caller, reason string) billing.Call {
	return billing.Call{Actor: caller.Actor, Reason: strings.TrimSpace(reason), CorrelationID: RequestID(r.Context())}
}

// tenantBilling is the common start of a billing tenant route: the
// permission, the scope, the caller and the service.
func (s *server) tenantBilling(w http.ResponseWriter, r *http.Request, permission string) (*billing.Service, scope.Scope, tenant.Caller, bool) {
	ctx := r.Context()
	if err := scope.Require(ctx, permission); err != nil {
		s.writeError(w, r, err)
		return nil, scope.Scope{}, tenant.Caller{}, false
	}
	svc, err := s.billingService()
	if err != nil {
		s.writeError(w, r, err)
		return nil, scope.Scope{}, tenant.Caller{}, false
	}
	sc, _ := scope.FromContext(ctx)
	caller, err := tenantCaller(ctx)
	if err != nil {
		s.writeError(w, r, err)
		return nil, scope.Scope{}, tenant.Caller{}, false
	}
	return svc, sc, caller, true
}

func checkReasonLength(reason string) error {
	if len(reason) > 1000 {
		return zserrors.New(zserrors.CodeValidation, "The reason is too long.")
	}
	return nil
}

func (s *server) getCatalogue(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, ok := callerAPIKey(ctx); ok {
		s.writeError(w, r, errTokenOnly)
		return
	}
	p, err := identity.PrincipalFromContext(ctx)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	id, err := s.brandFromPath(r, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	svc, err := s.billingService()
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	c, err := svc.Catalogue(ctx, id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeAs(w, r, http.StatusOK, c, &gen.Catalogue{})
}

func (s *server) getBillingSummary(w http.ResponseWriter, r *http.Request) {
	svc, sc, _, ok := s.tenantBilling(w, r, "billing:read")
	if !ok {
		return
	}
	sum, err := svc.Summary(r.Context(), sc)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeAs(w, r, http.StatusOK, sum, &gen.BillingSummary{})
}

func (s *server) listSubscriptions(w http.ResponseWriter, r *http.Request) {
	svc, sc, _, ok := s.tenantBilling(w, r, "billing:read")
	if !ok {
		return
	}
	list, err := svc.ListSubscriptions(r.Context(), sc)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeAs(w, r, http.StatusOK, map[string]any{"data": list}, &gen.SubscriptionList{})
}

func (s *server) createSubscription(w http.ResponseWriter, r *http.Request) {
	svc, sc, caller, ok := s.tenantBilling(w, r, "billing:manage")
	if !ok {
		return
	}
	var body gen.SubscriptionCreate
	if err := decodeBody(r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	reason := deref(body.Reason)
	if err := checkReasonLength(reason); err != nil {
		s.writeError(w, r, err)
		return
	}
	sub, _, err := svc.CreateSubscription(r.Context(), sc, billingCall(r, caller, reason), body.Plan, body.IdempotencyKey)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeAs(w, r, http.StatusCreated, sub, &gen.Subscription{})
}

func (s *server) changeSubscriptionPlan(w http.ResponseWriter, r *http.Request) {
	svc, sc, caller, ok := s.tenantBilling(w, r, "billing:manage")
	if !ok {
		return
	}
	var body gen.SubscriptionChange
	if err := decodeBody(r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	reason := deref(body.Reason)
	if err := checkReasonLength(reason); err != nil {
		s.writeError(w, r, err)
		return
	}
	sub, err := svc.ChangePlan(r.Context(), sc, billingCall(r, caller, reason), r.PathValue("sub"), body.Plan, string(body.Policy))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeAs(w, r, http.StatusOK, sub, &gen.Subscription{})
}

func (s *server) cancelSubscription(w http.ResponseWriter, r *http.Request) {
	svc, sc, caller, ok := s.tenantBilling(w, r, "billing:manage")
	if !ok {
		return
	}
	q := r.URL.Query()
	if len(q["policy"]) > 1 || len(q["reason"]) > 1 {
		s.writeError(w, r, zserrors.New(zserrors.CodeValidation, "A query parameter is repeated."))
		return
	}
	policy := q.Get("policy")
	if policy == "" {
		policy = billing.PolicyEndOfTerm
	}
	if err := checkReasonLength(q.Get("reason")); err != nil {
		s.writeError(w, r, err)
		return
	}
	sub, err := svc.CancelSubscription(r.Context(), sc, billingCall(r, caller, q.Get("reason")), r.PathValue("sub"), policy)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeAs(w, r, http.StatusOK, sub, &gen.Subscription{})
}

func (s *server) listInvoices(w http.ResponseWriter, r *http.Request) {
	svc, sc, _, ok := s.tenantBilling(w, r, "billing:read")
	if !ok {
		return
	}
	page, err := pageParams(r.URL.Query())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	list, next, err := svc.ListInvoices(r.Context(), sc, page)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeAs(w, r, http.StatusOK, map[string]any{"data": list, "next_cursor": nextCursor(next)}, &gen.InvoicePage{})
}

func (s *server) getInvoice(w http.ResponseWriter, r *http.Request) {
	svc, sc, _, ok := s.tenantBilling(w, r, "billing:read")
	if !ok {
		return
	}
	inv, err := svc.GetInvoice(r.Context(), sc, r.PathValue("inv"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeAs(w, r, http.StatusOK, inv, &gen.Invoice{})
}

// invoiceHTMLPolicy keeps an invoice page inert in a browser: no script,
// no fetch, no form, inline styles and data images only.
const invoiceHTMLPolicy = "default-src 'none'; style-src 'unsafe-inline'; img-src data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'; sandbox"

func (s *server) getInvoiceHTML(w http.ResponseWriter, r *http.Request) {
	svc, sc, _, ok := s.tenantBilling(w, r, "billing:read")
	if !ok {
		return
	}
	html, err := svc.InvoiceHTML(r.Context(), sc, r.PathValue("inv"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", invoiceHTMLPolicy)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(html)
}

func (s *server) getInvoicePDF(w http.ResponseWriter, r *http.Request) {
	svc, sc, _, ok := s.tenantBilling(w, r, "billing:read")
	if !ok {
		return
	}
	pdf, err := svc.InvoicePDF(r.Context(), sc, r.PathValue("inv"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	defer pdf.Body.Close()
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", `inline; filename="invoice.pdf"`)
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, pdf.Body)
}

func (s *server) listPayments(w http.ResponseWriter, r *http.Request) {
	svc, sc, _, ok := s.tenantBilling(w, r, "billing:read")
	if !ok {
		return
	}
	page, err := pageParams(r.URL.Query())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	list, next, err := svc.ListPayments(r.Context(), sc, page)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeAs(w, r, http.StatusOK, map[string]any{"data": list, "next_cursor": nextCursor(next)}, &gen.PaymentPage{})
}

// staffBilling is the start of a staff billing route: a staff token with an
// active billing grant in the tenant's brand, or 404.
func (s *server) staffBilling(w http.ResponseWriter, r *http.Request) (*billing.Service, scope.Scope, billing.Call, bool) {
	ctx := r.Context()
	p, err := s.requireStaff(ctx, identity.StaffBilling)
	if err != nil {
		s.writeError(w, r, err)
		return nil, scope.Scope{}, billing.Call{}, false
	}
	tenantID, err := pathID(r, "id", idgen.Tenant, tenant.ErrNotFound)
	if err != nil {
		s.writeError(w, r, err)
		return nil, scope.Scope{}, billing.Call{}, false
	}
	svc, err := s.billingService()
	if err != nil {
		s.writeError(w, r, err)
		return nil, scope.Scope{}, billing.Call{}, false
	}
	actorID := "sub:" + p.Subject
	if ident, ok := callerIdentity(ctx); ok {
		actorID = ident.PublicID()
	}
	call := billing.Call{Actor: events.Actor{Type: events.ActorStaff, ID: actorID}, CorrelationID: RequestID(ctx)}
	return svc, scope.Scope{Brand: p.Brand.ID, Tenant: tenantID}, call, true
}

func (s *server) refundPayment(w http.ResponseWriter, r *http.Request) {
	svc, sc, call, ok := s.staffBilling(w, r)
	if !ok {
		return
	}
	var body gen.RefundRequest
	if err := decodeBody(r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := checkReasonLength(body.Reason); err != nil {
		s.writeError(w, r, err)
		return
	}
	call.Reason = body.Reason
	pay, _, err := svc.Refund(r.Context(), sc, call, r.PathValue("pay"), body.AmountMinor, body.IdempotencyKey)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeAs(w, r, http.StatusCreated, pay, &gen.Payment{})
}

func (s *server) grantCredit(w http.ResponseWriter, r *http.Request) {
	svc, sc, call, ok := s.staffBilling(w, r)
	if !ok {
		return
	}
	var body gen.CreditGrant
	if err := decodeBody(r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := checkReasonLength(body.Reason); err != nil {
		s.writeError(w, r, err)
		return
	}
	call.Reason = body.Reason
	c, created, err := svc.GrantCredit(r.Context(), sc, call, billing.StaffCredit{
		AmountMinor: body.AmountMinor, Currency: body.Currency, ExpiresAt: body.ExpiresAt, IdempotencyKey: body.IdempotencyKey,
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	s.writeAs(w, r, status, c, &gen.Credit{})
}

func (s *server) redeemCreditCode(w http.ResponseWriter, r *http.Request) {
	svc, sc, caller, ok := s.tenantBilling(w, r, "billing:manage")
	if !ok {
		return
	}
	var body gen.CreditRedeem
	if err := decodeBody(r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	c, created, err := svc.Redeem(r.Context(), sc, billingCall(r, caller, ""), body.Code)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	s.writeAs(w, r, status, c, &gen.Credit{})
}

func (s *server) listPaymentMethods(w http.ResponseWriter, r *http.Request) {
	svc, sc, _, ok := s.tenantBilling(w, r, "billing:read")
	if !ok {
		return
	}
	list, err := svc.ListPaymentMethods(r.Context(), sc)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeAs(w, r, http.StatusOK, map[string]any{"data": list}, &gen.PaymentMethodList{})
}

func properties(in *[]gen.PluginProperty) []billing.Property {
	if in == nil {
		return nil
	}
	out := make([]billing.Property, 0, len(*in))
	for _, p := range *in {
		out = append(out, billing.Property{Key: p.Key, Value: p.Value})
	}
	return out
}

func (s *server) addPaymentMethod(w http.ResponseWriter, r *http.Request) {
	svc, sc, caller, ok := s.tenantBilling(w, r, "billing:manage")
	if !ok {
		return
	}
	var body gen.PaymentMethodCreate
	if err := decodeBody(r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	req := billing.NewPaymentMethod{IdempotencyKey: body.IdempotencyKey, ExternalPaymentMethodID: deref(body.ExternalPaymentMethodId),
		PluginProperties: properties(body.PluginProperties), HostedPageFields: properties(body.HostedPageFields)}
	if body.SetDefault != nil {
		req.SetDefault = *body.SetDefault
	}
	if body.HostedPage != nil {
		req.HostedPage = *body.HostedPage
	}
	added, _, err := svc.AddPaymentMethod(r.Context(), sc, billingCall(r, caller, ""), req)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeAs(w, r, http.StatusCreated, added, &gen.PaymentMethodAdded{})
}

func (s *server) setDefaultPaymentMethod(w http.ResponseWriter, r *http.Request) {
	svc, sc, caller, ok := s.tenantBilling(w, r, "billing:manage")
	if !ok {
		return
	}
	pm, err := svc.SetDefaultPaymentMethod(r.Context(), sc, billingCall(r, caller, ""), r.PathValue("pm"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeAs(w, r, http.StatusOK, pm, &gen.PaymentMethod{})
}

func (s *server) deletePaymentMethod(w http.ResponseWriter, r *http.Request) {
	svc, sc, caller, ok := s.tenantBilling(w, r, "billing:manage")
	if !ok {
		return
	}
	if err := svc.DeletePaymentMethod(r.Context(), sc, billingCall(r, caller, ""), r.PathValue("pm")); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) getBillingCredentials(w http.ResponseWriter, r *http.Request) {
	svc, sc, caller, ok := s.tenantBilling(w, r, billing.ProxyPermission)
	if !ok {
		return
	}
	c, err := svc.IssueCredential(r.Context(), sc, billingCall(r, caller, ""), billing.Holder{MembershipID: caller.Membership.ID, APIKeyID: caller.APIKeyID})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeAs(w, r, http.StatusOK, c, &gen.BillingCredential{})
}

// proxyBilling forwards a request authenticated by a billing credential.
// It does not pass through authenticate: the credential is not a token or
// an API key.
func (s *server) proxyBilling(w http.ResponseWriter, r *http.Request) {
	raw, ok := bearerToken(r)
	if !ok || !strings.HasPrefix(raw, billing.CredentialPrefix) {
		w.Header().Set("WWW-Authenticate", `Bearer`)
		s.writeError(w, r, zserrors.New(zserrors.CodeUnauthenticated, ""))
		return
	}
	svc, err := s.billingService()
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	tenantID, err := pathID(r, "id", idgen.Tenant, tenant.ErrNotFound)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	req := billing.ProxyRequest{Token: raw, CorrelationID: RequestID(r.Context()), Tenant: tenantID, Method: r.Method, Path: r.PathValue("path"), RawQuery: r.URL.RawQuery}
	if r.Method == http.MethodPost {
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/json" {
			s.writeError(w, r, zserrors.New(zserrors.CodeUnsupportedMediaType, "Send the body as application/json."))
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			s.writeError(w, r, zserrors.Wrap(zserrors.CodeBadRequest, "", err))
			return
		}
		req.ContentType, req.Body = "application/json", body
	}
	resp, err := svc.Proxy(r.Context(), req)
	if err != nil {
		if zserrors.IsCode(err, zserrors.CodeBillingTokenInvalid) {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		}
		s.writeError(w, r, err)
		return
	}
	if resp.ContentType != "" {
		w.Header().Set("Content-Type", resp.ContentType)
	}
	if resp.Location != "" {
		w.Header().Set("Location", resp.Location)
	}
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
}

// receiveKillBillNotification takes a Kill Bill push notification for a
// brand. The brand and its callback token are in the path, because Kill
// Bill cannot add headers to its callbacks. A wrong brand or token is 404.
func (s *server) receiveKillBillNotification(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	notFound := zserrors.New(zserrors.CodeNotFound, "")
	svc, err := s.billingService()
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	brandID, err := idgen.ParseAs(idgen.Brand, r.PathValue("brand"))
	if err != nil || !svc.VerifyWebhookToken(brandID, r.PathValue("token")) {
		s.writeError(w, r, notFound)
		return
	}
	rec, err := brand.Get(ctx, s.db, brandID)
	if err != nil || rec.Status != brand.StatusActive {
		if err != nil && !zserrors.IsCode(err, zserrors.CodeNotFound) {
			s.writeError(w, r, err)
			return
		}
		s.writeError(w, r, notFound)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, billing.MaxNotificationBytes+1))
	if err != nil {
		s.writeError(w, r, zserrors.Wrap(zserrors.CodeBadRequest, "", err))
		return
	}
	result, err := svc.HandleNotification(ctx, brandID, body)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.log.InfoContext(ctx, "kill bill notification", "request_id", RequestID(ctx), "brand", rec.Slug,
		"duplicate", result.Duplicate, "ignored", result.Ignored, "event", result.Event)
	w.WriteHeader(http.StatusNoContent)
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
