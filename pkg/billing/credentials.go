package billing

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

// CredentialTTL is how long a billing credential lasts.
const CredentialTTL = 15 * time.Minute

// CredentialPrefix starts every billing credential. It never collides with
// an API key, which starts with zs_, or a JWT.
const CredentialPrefix = "zsbc_"

// ProxyPermission is what the holder of a billing credential needs, at
// issue and at every use.
const ProxyPermission = "billing:manage"

// Credential is a short-lived, brand-scoped credential for a product that
// calls Kill Bill directly. Kill Bill has no short-lived tenant credentials,
// so Core issues its own token and exchanges it on the proxy route for a
// call with the brand's Kill Bill credentials, limited to the tenant's own
// Kill Bill account. AccountID and AccountExternalKey are what proxied
// requests must name.
type Credential struct {
	Token              string    `json:"token"`
	ExpiresAt          time.Time `json:"expires_at"`
	ProxyPath          string    `json:"proxy_path"`
	AccountID          string    `json:"account_id"`
	AccountExternalKey string    `json:"account_external_key"`
}

// Holder is who a credential is issued to: a membership, and the API key it
// acts through, if any.
type Holder struct {
	MembershipID idgen.UUID
	APIKeyID     idgen.UUID
}

type credentialClaims struct {
	Version    int    `json:"v"`
	Brand      string `json:"b"`
	Tenant     string `json:"t"`
	Membership string `json:"m"`
	APIKey     string `json:"k,omitempty"`
	Expires    int64  `json:"e"`
}

// credentialKey derives the signing key of billing credentials from the
// deployment secret, so it differs from the webhook tokens' key.
func (s *Service) credentialKey() []byte {
	mac := hmac.New(sha256.New, s.WebhookSecret)
	mac.Write([]byte("zs-core.billing-credential.v1"))
	return mac.Sum(nil)
}

func (s *Service) sign(payload []byte) []byte {
	mac := hmac.New(sha256.New, s.credentialKey())
	mac.Write(payload)
	return mac.Sum(nil)
}

// ProxyPath is the proxy route of a tenant.
func ProxyPath(tenantID idgen.UUID) string {
	return "/v1/tenants/" + idgen.Format(idgen.Tenant, tenantID) + "/billing/proxy/"
}

// IssueCredential issues a billing credential to the holder for the tenant
// and audits billing.credential.issued as call. The holder must hold
// billing:manage. The HTTP route checks it first.
func (s *Service) IssueCredential(ctx context.Context, sc scope.Scope, call Call, h Holder) (Credential, error) {
	if err := call.validate(); err != nil {
		return Credential{}, err
	}
	if len(s.WebhookSecret) == 0 {
		return Credential{}, zserrors.New(zserrors.CodeNotReady, "Billing credentials need ZS_KILLBILL_WEBHOOK_SECRET.")
	}
	if h.MembershipID.IsNil() {
		return Credential{}, scope.ErrPermissionDenied
	}
	a, err := s.account(ctx, sc)
	if err != nil {
		return Credential{}, err
	}
	expires := s.now().Add(CredentialTTL).Truncate(time.Second)
	claims := credentialClaims{Version: 1, Brand: sc.Brand.String(), Tenant: sc.Tenant.String(),
		Membership: h.MembershipID.String(), Expires: expires.Unix()}
	if !h.APIKeyID.IsNil() {
		claims.APIKey = h.APIKeyID.String()
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return Credential{}, err
	}
	token := CredentialPrefix + base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(s.sign(payload))
	if err := s.audit(ctx, sc, call, "billing.credential.issued", map[string]any{"killbill_account_id": a.id()}); err != nil {
		return Credential{}, err
	}
	return Credential{Token: token, ExpiresAt: expires, ProxyPath: ProxyPath(sc.Tenant), AccountID: a.id(),
		AccountExternalKey: a.tenant.PublicID()}, nil
}

var errTokenInvalid = zserrors.New(zserrors.CodeBillingTokenInvalid, "")

// verifyCredential checks a credential's signature and expiry and returns
// its scope and holder. The proxy then checks the holder again.
func (s *Service) verifyCredential(token string) (scope.Scope, Holder, error) {
	if len(s.WebhookSecret) == 0 {
		return scope.Scope{}, Holder{}, zserrors.New(zserrors.CodeNotReady, "Billing credentials need ZS_KILLBILL_WEBHOOK_SECRET.")
	}
	rest, ok := strings.CutPrefix(token, CredentialPrefix)
	if !ok || len(token) > 1024 {
		return scope.Scope{}, Holder{}, errTokenInvalid
	}
	enc, sig, ok := strings.Cut(rest, ".")
	if !ok {
		return scope.Scope{}, Holder{}, errTokenInvalid
	}
	payload, err1 := base64.RawURLEncoding.DecodeString(enc)
	mac, err2 := base64.RawURLEncoding.DecodeString(sig)
	if err1 != nil || err2 != nil || !hmac.Equal(mac, s.sign(payload)) {
		return scope.Scope{}, Holder{}, errTokenInvalid
	}
	var c credentialClaims
	if json.Unmarshal(payload, &c) != nil || c.Version != 1 || s.now().Unix() >= c.Expires {
		return scope.Scope{}, Holder{}, errTokenInvalid
	}
	b, errB := idgen.ParseUUID(c.Brand)
	t, errT := idgen.ParseUUID(c.Tenant)
	m, errM := idgen.ParseUUID(c.Membership)
	if errB != nil || errT != nil || errM != nil {
		return scope.Scope{}, Holder{}, errTokenInvalid
	}
	h := Holder{MembershipID: m}
	if c.APIKey != "" {
		k, err := idgen.ParseUUID(c.APIKey)
		if err != nil {
			return scope.Scope{}, Holder{}, errTokenInvalid
		}
		h.APIKeyID = k
	}
	return scope.Scope{Brand: b, Tenant: t}, h, nil
}

// ProxyRequest is a request a product sends to Kill Bill through Core. Path
// is the Kill Bill path after the proxy prefix, such as
// 1.0/kb/accounts/<id>/invoices, and RawQuery its query. CorrelationID,
// usually the request ID, goes to Kill Bill and to the audit of a write.
type ProxyRequest struct {
	Token         string
	CorrelationID string
	Tenant        idgen.UUID
	Method        string
	Path          string
	RawQuery      string
	ContentType   string
	Body          []byte
}

// ProxyResponse is Kill Bill's successful answer. Location, when set, is
// rewritten to the proxy route.
type ProxyResponse struct {
	Status      int
	ContentType string
	Location    string
	Body        []byte
}

// Proxy forwards a request to Kill Bill with the brand's credentials after
// checking the credential, its holder and the account-scoping rule. The
// credential must be for the tenant in the path, unexpired and correctly
// signed, or the answer is billing_token_invalid. Its holder must still be
// an active member, or an unrevoked API key, holding billing:manage in a
// tenant that is not closed, and active for a write, or the answer is
// tenant_not_found or permission_denied. The request must target the
// tenant's own Kill Bill account by ID or external key, as AllowProxy
// describes, or the answer is not_found. A Kill Bill refusal is
// billing_rejected with Kill Bill's error code only.
func (s *Service) Proxy(ctx context.Context, req ProxyRequest) (ProxyResponse, error) {
	sc, holder, err := s.verifyCredential(req.Token)
	if err != nil {
		return ProxyResponse{}, err
	}
	if sc.Tenant != req.Tenant {
		return ProxyResponse{}, errTokenInvalid
	}
	a, err := s.account(ctx, sc)
	if err != nil {
		return ProxyResponse{}, err
	}
	write := req.Method != http.MethodGet
	if write && a.tenant.Status != tenant.StatusActive {
		return ProxyResponse{}, tenant.ErrNotFound
	}
	caller, err := tenant.CurrentCaller(ctx, s.DB, sc, tenant.Caller{
		Membership: tenant.Membership{ID: holder.MembershipID}, APIKeyID: holder.APIKeyID,
	})
	if err != nil {
		return ProxyResponse{}, err
	}
	if !caller.Has(ProxyPermission) {
		return ProxyResponse{}, scope.ErrPermissionDenied
	}
	if err := AllowProxy(req.Method, req.Path, req.RawQuery, req.Body, a.id(), a.tenant.PublicID(), a.pluginName()); err != nil {
		return ProxyResponse{}, err
	}
	if write && req.Path == "1.0/kb/subscriptions" {
		if err := s.checkBundleKey(ctx, a, req.Body); err != nil {
			return ProxyResponse{}, err
		}
	}
	path := "/" + req.Path
	if req.RawQuery != "" {
		path += "?" + req.RawQuery
	}
	actor := events.Actor{Type: events.ActorUser, ID: idgen.Format(idgen.Membership, holder.MembershipID)}
	if !holder.APIKeyID.IsNil() {
		actor = events.Actor{Type: events.ActorAPIKey, ID: idgen.Format(idgen.APIKey, holder.APIKeyID)}
	}
	correlationID := req.CorrelationID
	if correlationID == "" {
		correlationID = idgen.MustNewV7().String()
	}
	call := Call{Actor: actor, Reason: "billing proxy", CorrelationID: correlationID}
	resp, err := s.KillBill.Forward(ctx, a.creds, call.kb(), req.Method, path, req.ContentType, req.Body)
	if err != nil {
		return ProxyResponse{}, kbError(err)
	}
	out := ProxyResponse{Status: resp.Status, ContentType: resp.Header.Get("Content-Type"), Body: resp.Body}
	var objectID string
	if loc := resp.Header.Get("Location"); loc != "" {
		if u, err := url.Parse(loc); err == nil {
			if rest, ok := strings.CutPrefix(u.Path, "/1.0/kb/"); ok {
				out.Location = ProxyPath(sc.Tenant) + "1.0/kb/" + rest
				segs := strings.Split(strings.Trim(rest, "/"), "/")
				objectID = segs[len(segs)-1]
			}
		}
	}
	if write {
		// The path holds fixed segments and the tenant's own account ID
		// only, as AllowProxy requires. The body is never recorded.
		details := map[string]any{"method": req.Method, "killbill_path": req.Path, "killbill_account_id": a.id(), "status": resp.Status}
		if _, err := idgen.ParseUUID(objectID); err == nil {
			details["killbill_object_id"] = objectID
		}
		if err := s.audit(ctx, sc, call, "billing.proxy.write", details); err != nil {
			return ProxyResponse{}, err
		}
	}
	return out, nil
}

// checkBundleKey refuses a subscription whose bundleExternalKey is not one
// of the tenant's own bundles, as Kill Bill reports them. Kill Bill adds a
// subscription to the bundle that key names, so another account's key would
// reach another account. An absent or null key is accepted.
func (s *Service) checkBundleKey(ctx context.Context, a account, body []byte) error {
	fields, ok := jsonObject(body)
	if !ok {
		return errNotFound
	}
	raw, present := fields["bundleExternalKey"]
	if !present || string(raw) == "null" {
		return nil
	}
	var key string
	if json.Unmarshal(raw, &key) != nil || key == "" {
		return errNotFound
	}
	bundles, err := s.KillBill.AccountBundles(ctx, a.creds, a.id())
	if err != nil {
		return kbError(err)
	}
	for _, b := range bundles {
		if b.ExternalKey == key && strings.EqualFold(b.AccountID, a.id()) {
			return nil
		}
	}
	return errNotFound
}

// Proxy allow-list fields.
var (
	proxyPathChars     = regexp.MustCompile(`^[A-Za-z0-9._~/-]{1,512}$`)
	accountLookupQuery = map[string]bool{"externalKey": true, "accountWithBalance": true, "accountWithBalanceAndCBA": true, "audit": true}
	subscriptionFields = map[string]bool{"accountId": true, "planName": true, "externalKey": true, "bundleExternalKey": true,
		"productName": true, "productCategory": true, "billingPeriod": true, "priceList": true}
	paymentMethodFields = map[string]bool{"pluginName": true, "externalKey": true, "pluginInfo": true, "isDefault": true}
)

// AllowProxy is the account-scoping rule of the proxy. It accepts only:
//
//   - GET 1.0/kb/accounts?externalKey=<tenant ID>, the tenant's own account
//     by external key.
//   - GET 1.0/kb/accounts/<account ID> and any path below it, for the
//     tenant's own account ID.
//   - POST 1.0/kb/accounts/<account ID>/paymentMethods, for the tenant's own
//     account, with the brand's payment plugin, no query except isDefault,
//     and only the fields pluginName, externalKey, pluginInfo and isDefault.
//   - POST 1.0/kb/subscriptions with no query, whose JSON object names the
//     tenant's own account in accountId and has only the plan fields
//     accountId, planName, externalKey, bundleExternalKey, productName,
//     productCategory, billingPeriod and priceList. Price overrides, billing
//     dates and every other field are refused.
//
// Everything else, including a path that names another account, writes to
// blocking states, payments, credits or usage, and any path with an empty,
// "." or ".." segment, is not_found.
func AllowProxy(method, path, rawQuery string, body []byte, accountID, externalKey, pluginName string) error {
	notFound := zserrors.New(zserrors.CodeNotFound, "")
	if !proxyPathChars.MatchString(path) {
		return notFound
	}
	segs := strings.Split(path, "/")
	for _, seg := range segs {
		if seg == "" || seg == "." || seg == ".." {
			return notFound
		}
	}
	if len(segs) < 3 || segs[0] != "1.0" || segs[1] != "kb" {
		return notFound
	}
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		return notFound
	}
	segs = segs[2:]
	switch {
	case method == http.MethodGet && len(segs) == 1 && segs[0] == "accounts":
		keys := q["externalKey"]
		if len(keys) != 1 || keys[0] != externalKey {
			return notFound
		}
		for k := range q {
			if !accountLookupQuery[k] {
				return notFound
			}
		}
		return nil
	case method == http.MethodGet && len(segs) >= 2 && segs[0] == "accounts":
		if !strings.EqualFold(segs[1], accountID) {
			return notFound
		}
		return nil
	case method == http.MethodPost && len(segs) == 3 && segs[0] == "accounts" && segs[2] == "paymentMethods":
		if !strings.EqualFold(segs[1], accountID) {
			return notFound
		}
		for k := range q {
			if k != "isDefault" {
				return notFound
			}
		}
		fields, ok := jsonObject(body)
		if !ok || !onlyFields(fields, paymentMethodFields) {
			return notFound
		}
		var name string
		if json.Unmarshal(fields["pluginName"], &name) != nil || name != pluginName {
			return notFound
		}
		return nil
	case method == http.MethodPost && len(segs) == 1 && segs[0] == "subscriptions":
		if rawQuery != "" {
			return notFound
		}
		fields, ok := jsonObject(body)
		if !ok || !onlyFields(fields, subscriptionFields) {
			return notFound
		}
		var acct string
		if json.Unmarshal(fields["accountId"], &acct) != nil || !strings.EqualFold(acct, accountID) {
			return notFound
		}
		return nil
	}
	return notFound
}

// jsonObject decodes one JSON object and refuses a repeated key, so the
// field Core checks is the field Kill Bill reads.
func jsonObject(body []byte) (map[string]json.RawMessage, bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, false
	}
	fields := map[string]json.RawMessage{}
	for dec.More() {
		tok, err := dec.Token()
		key, ok := tok.(string)
		if err != nil || !ok {
			return nil, false
		}
		if _, dup := fields[key]; dup {
			return nil, false
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, false
		}
		fields[key] = v
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return nil, false
	}
	if _, err := dec.Token(); err == nil {
		return nil, false
	}
	return fields, true
}

func onlyFields(fields map[string]json.RawMessage, allowed map[string]bool) bool {
	for k := range fields {
		if !allowed[k] {
			return false
		}
	}
	return true
}
