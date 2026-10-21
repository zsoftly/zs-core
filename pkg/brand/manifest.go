// Package brand loads, validates and applies brand manifests.
//
// A brand is one product on a Core deployment: one Keycloak realm, one Kill
// Bill tenant and one manifest, brands/<slug>.yaml, validated against
// api/schemas/brand-manifest.json. Apply converges both engines towards the
// manifest and records the brand in Core. It is explicit and idempotent: it
// computes the desired state, reads the current state, produces a diff,
// applies that diff, audits every change and returns the diff. Secrets are
// references that are resolved at apply time and never stored, logged or
// put in a diff.
package brand

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"

	"github.com/zsoftly/zs-core/api/schemas"
	"github.com/zsoftly/zs-core/internal/secrets"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
)

// MaxManifestBytes caps the size of a manifest file or request body.
const MaxManifestBytes = 1 << 20

// MFA policies.
const (
	MFAOptional         = "optional"
	MFARequired         = "required"
	MFARequiredForStaff = "required_for_staff"
)

// Manifest is a brand manifest. Field names follow the YAML keys.
type Manifest struct {
	SchemaVersion int            `json:"schema_version"`
	Slug          string         `json:"slug"`
	Name          string         `json:"name"`
	Identity      Identity       `json:"identity"`
	Billing       Billing        `json:"billing"`
	Notifications *Notifications `json:"notifications,omitempty"`
	// Entitlements are the brand's quotas. Nil leaves the quotas to the
	// HTTP route; a list, even empty, is what apply converges to.
	Entitlements *[]Entitlement `json:"entitlements,omitempty"`
	Permissions  []string       `json:"permissions,omitempty"`
}

// Identity is the Keycloak side of a brand.
type Identity struct {
	Realm             string             `json:"realm"`
	DisplayName       string             `json:"display_name"`
	LoginTheme        string             `json:"login_theme,omitempty"`
	EmailTheme        string             `json:"email_theme,omitempty"`
	Locales           []string           `json:"locales"`
	DefaultLocale     string             `json:"default_locale"`
	SSLRequired       string             `json:"ssl_required,omitempty"`
	TokenLifetimes    *TokenLifetimes    `json:"token_lifetimes,omitempty"`
	MFA               string             `json:"mfa"`
	MFAACRValues      []string           `json:"mfa_acr_values,omitempty"`
	RequiredActions   []string           `json:"required_actions,omitempty"`
	CustomerClient    CustomerClient     `json:"customer_client"`
	StaffClient       StaffClient        `json:"staff_client"`
	ServiceClients    []ServiceClient    `json:"service_clients,omitempty"`
	IdentityProviders []IdentityProvider `json:"identity_providers,omitempty"`
}

// TokenLifetimes are realm token and session lifetimes in seconds.
type TokenLifetimes struct {
	AccessTokenSeconds    *int `json:"access_token_seconds,omitempty"`
	SSOSessionIdleSeconds *int `json:"sso_session_idle_seconds,omitempty"`
	SSOSessionMaxSeconds  *int `json:"sso_session_max_seconds,omitempty"`
}

// CustomerClient is the public OIDC client customers sign in with.
type CustomerClient struct {
	ClientID           string   `json:"client_id"`
	Name               string   `json:"name,omitempty"`
	RedirectURIs       []string `json:"redirect_uris"`
	WebOrigins         []string `json:"web_origins,omitempty"`
	DirectAccessGrants bool     `json:"direct_access_grants,omitempty"`
}

// StaffClient is the confidential client staff sign in with.
type StaffClient struct {
	ClientID           string   `json:"client_id"`
	Name               string   `json:"name,omitempty"`
	SecretRef          string   `json:"secret_ref"`
	RedirectURIs       []string `json:"redirect_uris"`
	WebOrigins         []string `json:"web_origins,omitempty"`
	DirectAccessGrants bool     `json:"direct_access_grants,omitempty"`
	ServiceAccounts    bool     `json:"service_accounts,omitempty"`
}

// ServiceClient is a confidential client with a service account.
type ServiceClient struct {
	ClientID  string `json:"client_id"`
	Name      string `json:"name,omitempty"`
	SecretRef string `json:"secret_ref"`
}

// IdentityProvider is a brokered provider. Which fields apply depends on
// Type: google, microsoft, github, oidc or saml.
type IdentityProvider struct {
	Alias              string `json:"alias"`
	Type               string `json:"type"`
	DisplayName        string `json:"display_name,omitempty"`
	Enabled            *bool  `json:"enabled,omitempty"`
	ClientID           string `json:"client_id,omitempty"`
	ClientSecretRef    string `json:"client_secret_ref,omitempty"`
	DefaultScopes      string `json:"default_scopes,omitempty"`
	Issuer             string `json:"issuer,omitempty"`
	AuthorizationURL   string `json:"authorization_url,omitempty"`
	TokenURL           string `json:"token_url,omitempty"`
	JWKSURL            string `json:"jwks_url,omitempty"`
	IDPEntityID        string `json:"idp_entity_id,omitempty"`
	SSOURL             string `json:"sso_url,omitempty"`
	SPEntityID         string `json:"sp_entity_id,omitempty"`
	NameIDFormat       string `json:"name_id_format,omitempty"`
	SigningCertificate string `json:"signing_certificate,omitempty"`
}

// IsEnabled defaults to true.
func (p IdentityProvider) IsEnabled() bool { return p.Enabled == nil || *p.Enabled }

// Billing is the Kill Bill side of a brand.
type Billing struct {
	KillBillTenantKey    string   `json:"killbill_tenant_key"`
	KillBillAPISecretRef string   `json:"killbill_api_secret_ref"`
	Catalogue            string   `json:"catalogue,omitempty"`
	Overdue              string   `json:"overdue,omitempty"`
	Currencies           []string `json:"currencies"`
	Seller               Seller   `json:"seller"`
	// PaymentPlugin is the one-plugin form of PaymentPlugins, kept as an
	// alias: it is a list of that plugin, the default. A manifest sets one
	// of the two. Plugins returns the list either way.
	PaymentPlugin    *PaymentPlugin    `json:"payment_plugin,omitempty"`
	PaymentPlugins   []PaymentPlugin   `json:"payment_plugins,omitempty"`
	InvoicePlugin    *InvoicePlugin    `json:"invoice_plugin,omitempty"`
	InvoiceTemplates *InvoiceTemplates `json:"invoice_templates,omitempty"`
	Email            *Email            `json:"email,omitempty"`
	Templates        string            `json:"templates,omitempty"`
	Wallet           *WalletPolicy     `json:"wallet,omitempty"`
	UsageWindow      *UsageWindow      `json:"usage_window,omitempty"`
	// Meters are the brand's meters. Nil leaves the meters to the HTTP
	// route; a list, even empty, is what apply converges to.
	Meters *[]Meter `json:"meters,omitempty"`
}

// WalletPolicy is how the wallet view treats a tenant of the brand. Amounts
// are minor units of the account currency, the brand's first currency.
type WalletPolicy struct {
	OverdraftMinor  int64 `json:"overdraft_minor,omitempty"`
	LowBalanceMinor int64 `json:"low_balance_minor,omitempty"`
}

// Overdraft returns how far below zero the available credit may go.
func (b Billing) Overdraft() int64 {
	if b.Wallet == nil {
		return 0
	}
	return b.Wallet.OverdraftMinor
}

// LowBalance returns the low-balance threshold, 0 when it is off.
func (b Billing) LowBalance() int64 {
	if b.Wallet == nil {
		return 0
	}
	return b.Wallet.LowBalanceMinor
}

// UsageWindow bounds the occurred_at of a usage event.
type UsageWindow struct {
	BackfillHours int `json:"backfill_hours,omitempty"`
	FutureSeconds int `json:"future_seconds,omitempty"`
}

// The default usage window: seven days back, five minutes forward.
const (
	DefaultBackfillHours = 7 * 24
	DefaultFutureSeconds = 300
)

// Backfill returns how far back a usage event may have occurred.
func (b Billing) Backfill() time.Duration {
	if b.UsageWindow == nil || b.UsageWindow.BackfillHours == 0 {
		return DefaultBackfillHours * time.Hour
	}
	return time.Duration(b.UsageWindow.BackfillHours) * time.Hour
}

// Future returns how far ahead a usage event may be dated. An explicit 0 is
// not expressible: the schema treats 0 as the default.
func (b Billing) Future() time.Duration {
	if b.UsageWindow == nil || b.UsageWindow.FutureSeconds == 0 {
		return DefaultFutureSeconds * time.Second
	}
	return time.Duration(b.UsageWindow.FutureSeconds) * time.Second
}

// Meter units.
var MeterUnits = []string{"minute", "second", "byte", "gib_hour", "request", "token", "count"}

// Meter is a meter of the brand. KillBillUnit is the catalogue usage unit
// its usage is forwarded to. Dimensions are the keys a usage event may
// carry.
type Meter struct {
	Slug         string   `json:"slug"`
	Unit         string   `json:"unit"`
	KillBillUnit string   `json:"killbill_unit"`
	Dimensions   []string `json:"dimensions,omitempty"`
}

// Quota windows.
const (
	WindowNone       = "none"
	WindowDay        = "day"
	WindowMonth      = "month"
	WindowConcurrent = "concurrent"
)

// AnyPlan is the plan of a quota that applies to every plan.
const AnyPlan = "*"

// Entitlement is a quota: the limit of a feature for a plan, or for every
// plan with AnyPlan. Meter names the meter whose usage counts against a day
// or month limit.
type Entitlement struct {
	Feature string `json:"feature"`
	Plan    string `json:"plan"`
	Limit   int64  `json:"limit"`
	Window  string `json:"window"`
	Meter   string `json:"meter,omitempty"`
}

// Seller is the legal seller of a brand. Address is the one-line form of
// AddressLines. TaxRegistrations are the seller's tax registration numbers,
// such as "GST/HST 123456789 RT0001". Apply puts them on every invoice and
// email through the translations it uploads.
type Seller struct {
	Name             string   `json:"name"`
	Country          string   `json:"country"`
	Address          string   `json:"address,omitempty"`
	AddressLines     []string `json:"address_lines,omitempty"`
	TaxRegistrations []string `json:"tax_registrations,omitempty"`
}

// Lines returns the seller's address lines: address_lines, or address.
func (s Seller) Lines() []string {
	if len(s.AddressLines) > 0 {
		return s.AddressLines
	}
	if s.Address != "" {
		return []string{s.Address}
	}
	return nil
}

// PaymentPlugin names a payment plugin of the brand and its configuration.
// Mode is test, the only mode v1 accepts. Default marks the plugin of the
// brand's payment methods; a list has exactly one.
type PaymentPlugin struct {
	Name      string `json:"name"`
	Version   string `json:"version,omitempty"`
	Mode      string `json:"mode"`
	ConfigKey string `json:"config_key,omitempty"`
	ConfigRef string `json:"config_ref"`
	Default   bool   `json:"default,omitempty"`
}

// Plugins returns the brand's payment plugins: payment_plugins, or the one
// plugin of payment_plugin as the default.
func (b Billing) Plugins() []PaymentPlugin {
	if b.PaymentPlugin != nil {
		p := *b.PaymentPlugin
		p.Default = true
		return []PaymentPlugin{p}
	}
	return b.PaymentPlugins
}

// DefaultPaymentPlugin returns the plugin marked default, nil when the brand
// has no payment plugin.
func (b Billing) DefaultPaymentPlugin() *PaymentPlugin {
	for _, p := range b.Plugins() {
		if p.Default {
			return &p
		}
	}
	return nil
}

// pluginsPath is the manifest path of the payment plugin list.
func (b Billing) pluginsPath(i int) string {
	if b.PaymentPlugin != nil {
		return "/billing/payment_plugin"
	}
	return fmt.Sprintf("/billing/payment_plugins/%d", i)
}

// Payment plugin modes. v1 refuses live: no payment plugin may take real
// money.
const (
	PaymentModeTest = "test"
	PaymentModeLive = "live"
)

// ExternalPaymentPlugin is Kill Bill's built-in payment plugin. It records
// payments made outside Kill Bill and is what a brand without a payment
// plugin uses.
const ExternalPaymentPlugin = "__EXTERNAL_PAYMENT__"

// PaymentPluginName is the Kill Bill plugin name of the brand's payment
// methods: the payment plugin's service name, or ExternalPaymentPlugin.
func (b Billing) PaymentPluginName() string {
	p := b.DefaultPaymentPlugin()
	if p == nil {
		return ExternalPaymentPlugin
	}
	return p.Key()
}

// Key is the plugin service name its per-tenant configuration is stored
// under: killbill-<name>. Validate refuses any other config_key.
func (p PaymentPlugin) Key() string {
	return "killbill-" + p.Name
}

// InvoicePlugin names an invoice plugin, for tax, and its configuration.
// Mode is test, the only mode v1 accepts.
type InvoicePlugin struct {
	Name      string `json:"name"`
	Version   string `json:"version,omitempty"`
	Mode      string `json:"mode"`
	ConfigKey string `json:"config_key,omitempty"`
	ConfigRef string `json:"config_ref"`
}

// Key is the service name the plugin's per-tenant configuration is stored
// under: killbill-<name>.
func (p InvoicePlugin) Key() string { return "killbill-" + p.Name }

// InvoiceTemplates are the files Kill Bill renders invoices with. Template
// and ManualPayTemplate are HTML Mustache templates. Translations and
// CatalogueTranslations are Java properties per locale of the brand.
type InvoiceTemplates struct {
	Template              string            `json:"template,omitempty"`
	ManualPayTemplate     string            `json:"manual_pay_template,omitempty"`
	Translations          map[string]string `json:"translations,omitempty"`
	CatalogueTranslations map[string]string `json:"catalogue_translations,omitempty"`
}

// Email is the brand's billing email through the email-notifications
// plugin: the sender, the Kill Bill events that send email, the SMTP server
// and the directory of templates, <templates>/<locale>/<file>.
type Email struct {
	Sender    string   `json:"sender"`
	Events    []string `json:"events,omitempty"`
	SMTP      SMTP     `json:"smtp"`
	Templates string   `json:"templates,omitempty"`
}

// SMTP is the SMTP server of the brand's billing email. The user name and
// password are secret references.
type SMTP struct {
	Host        string `json:"host"`
	Port        int    `json:"port"`
	UseSSL      bool   `json:"use_ssl,omitempty"`
	UsernameRef string `json:"username_ref,omitempty"`
	PasswordRef string `json:"password_ref,omitempty"`
}

// EmailEvents are the Kill Bill events the email-notifications plugin 0.8.1
// sends email for.
var EmailEvents = []string{"INVOICE_CREATION", "INVOICE_NOTIFICATION", "INVOICE_PAYMENT_SUCCESS", "INVOICE_PAYMENT_FAILED", "SUBSCRIPTION_CANCEL"}

// DefaultEmailEvents is the event list of an email section without events.
var DefaultEmailEvents = []string{"INVOICE_CREATION"}

// EventsOrDefault returns the events, or DefaultEmailEvents.
func (e Email) EventsOrDefault() []string {
	if len(e.Events) == 0 {
		return DefaultEmailEvents
	}
	return e.Events
}

// Notifications declares the sender identity and webhook endpoints.
type Notifications struct {
	Sender   *Sender   `json:"sender,omitempty"`
	ReplyTo  string    `json:"reply_to,omitempty"`
	Webhooks []Webhook `json:"webhooks,omitempty"`
}

// Sender is the From identity of brand email.
type Sender struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
}

// Webhook is an outbound webhook endpoint.
type Webhook struct {
	URL       string   `json:"url"`
	SecretRef string   `json:"secret_ref"`
	Events    []string `json:"events,omitempty"`
}

var (
	schemaOnce sync.Once
	schema     *jsonschema.Schema
	schemaErr  error
)

// refuseLoader stops the schema compiler from fetching anything: the brand
// manifest schema is self-contained and the 2020-12 meta-schema is built in.
type refuseLoader struct{}

func (refuseLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("schema loading disabled: %s", url)
}

func compiledSchema() (*jsonschema.Schema, error) {
	schemaOnce.Do(func() {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemas.BrandManifest))
		if err != nil {
			schemaErr = fmt.Errorf("brand manifest schema: %w", err)
			return
		}
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft2020)
		c.UseLoader(refuseLoader{})
		if err := c.AddResource(schemas.BrandManifestURL, doc); err != nil {
			schemaErr = fmt.Errorf("brand manifest schema: %w", err)
			return
		}
		schema, schemaErr = c.Compile(schemas.BrandManifestURL)
	})
	return schema, schemaErr
}

// invalid returns brand_manifest_invalid with the problems in the message.
// Manifests hold no secret values, so the problems are safe to show.
func invalid(problems []string) error {
	sort.Strings(problems)
	if len(problems) > 20 {
		problems = append(problems[:20], fmt.Sprintf("and %d more", len(problems)-20))
	}
	return zserrors.New(zserrors.CodeBrandManifestInvalid, "The brand manifest is invalid: "+strings.Join(problems, "; ")+".")
}

// Load reads and validates the manifest at path.
func Load(path string) (*Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open brand manifest: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxManifestBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read brand manifest: %w", err)
	}
	return Parse(data)
}

// Parse decodes a YAML or JSON manifest, validates it against the schema
// and the rules the schema cannot express, and returns it. Unknown keys,
// duplicate keys and anything that is not a single document are rejected.
func Parse(data []byte) (*Manifest, error) {
	if len(data) > MaxManifestBytes {
		return nil, invalid([]string{"the manifest is larger than 1 MiB"})
	}
	var doc any
	dec := yaml.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&doc); err != nil {
		return nil, invalid([]string{"not valid YAML or JSON: " + yamlMessage(err)})
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, invalid([]string{"the file must hold exactly one document"})
	}
	// Round-trip through JSON so the schema sees JSON types only.
	encoded, err := json.Marshal(normalizeYAML(doc))
	if err != nil {
		return nil, invalid([]string{"the manifest holds values that are not JSON: " + err.Error()})
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil {
		return nil, invalid([]string{"not valid JSON"})
	}
	sch, err := compiledSchema()
	if err != nil {
		return nil, err
	}
	if err := sch.Validate(instance); err != nil {
		var ve *jsonschema.ValidationError
		if errors.As(err, &ve) {
			return nil, invalid(schemaProblems(ve))
		}
		return nil, invalid([]string{err.Error()})
	}
	var m Manifest
	jd := json.NewDecoder(bytes.NewReader(encoded))
	jd.DisallowUnknownFields()
	if err := jd.Decode(&m); err != nil {
		return nil, invalid([]string{err.Error()})
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

func yamlMessage(err error) string {
	msg := strings.TrimPrefix(err.Error(), "yaml: ")
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}

// normalizeYAML turns the map[string]any and map[any]any values yaml.v3 can
// produce into JSON-encodable maps.
func normalizeYAML(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = normalizeYAML(val)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[fmt.Sprint(k)] = normalizeYAML(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = normalizeYAML(val)
		}
		return out
	default:
		return v
	}
}

func schemaProblems(ve *jsonschema.ValidationError) []string {
	var problems []string
	var walk func(u jsonschema.OutputUnit)
	walk = func(u jsonschema.OutputUnit) {
		if u.Error != nil {
			loc := u.InstanceLocation
			if loc == "" {
				loc = "/"
			}
			problems = append(problems, loc+": "+u.Error.String())
		}
		for _, c := range u.Errors {
			walk(c)
		}
	}
	walk(*ve.BasicOutput())
	if len(problems) == 0 {
		problems = append(problems, "does not match the schema")
	}
	return dedupe(problems)
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// Validate checks the rules the JSON Schema cannot express. Parse calls it.
func (m *Manifest) Validate() error {
	var problems []string
	fail := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if m.SchemaVersion != 1 {
		fail("/schema_version: must be 1")
	}
	if !secrets.ValidSlug(m.Slug) {
		fail("/slug: must be 2 to 40 lowercase letters and digits in words joined by single hyphens")
	}
	id := m.Identity
	if !containsString(id.Locales, id.DefaultLocale) {
		fail("/identity/default_locale: must be one of /identity/locales")
	}
	switch id.MFA {
	case MFAOptional, MFARequired, MFARequiredForStaff:
	default:
		fail("/identity/mfa: must be optional, required or required_for_staff")
	}
	// The password grant never runs a browser flow, so it would issue tokens
	// without the second factor the policy requires.
	if id.MFA == MFARequired && id.CustomerClient.DirectAccessGrants {
		fail("/identity/customer_client/direct_access_grants: must be false when /identity/mfa is required, because the password grant skips the second factor")
	}
	if (id.MFA == MFARequired || id.MFA == MFARequiredForStaff) && id.StaffClient.DirectAccessGrants {
		fail("/identity/staff_client/direct_access_grants: must be false when /identity/mfa is %s, because the password grant skips the second factor", id.MFA)
	}
	clients := map[string]string{}
	addClient := func(path, clientID string) {
		if prev, ok := clients[clientID]; ok {
			fail("%s: client_id %q is also used by %s", path, clientID, prev)
			return
		}
		clients[clientID] = path
	}
	addClient("/identity/customer_client", id.CustomerClient.ClientID)
	addClient("/identity/staff_client", id.StaffClient.ClientID)
	for i, sc := range id.ServiceClients {
		addClient(fmt.Sprintf("/identity/service_clients/%d", i), sc.ClientID)
	}
	aliases := map[string]bool{}
	for i, p := range id.IdentityProviders {
		if aliases[p.Alias] {
			fail("/identity/identity_providers/%d: alias %q is used twice", i, p.Alias)
		}
		aliases[p.Alias] = true
	}
	for path, file := range m.Billing.filePaths() {
		if file != "" && !filepath.IsLocal(filepath.FromSlash(file)) {
			fail("%s: must be a relative path inside the manifest directory", path)
		}
	}
	problems = append(problems, m.billingPluginProblems()...)
	if id.Realm == "master" || !realmName.MatchString(id.Realm) {
		fail("/identity/realm: must match %s and must not be master", realmName)
	}
	for i, v := range id.MFAACRValues {
		if strings.TrimSpace(v) == "" || len(v) > 64 || v == "0" || v == "1" {
			fail("/identity/mfa_acr_values/%d: must be a non-empty value of at most 64 characters, other than 0 and 1, that the realm issues only after a second factor", i)
		}
	}
	type clientURIs struct {
		path               string
		redirects, origins []string
	}
	for _, c := range []clientURIs{
		{"/identity/customer_client", id.CustomerClient.RedirectURIs, id.CustomerClient.WebOrigins},
		{"/identity/staff_client", id.StaffClient.RedirectURIs, id.StaffClient.WebOrigins},
	} {
		for i, u := range c.redirects {
			if err := checkRedirectURI(u); err != nil {
				fail("%s/redirect_uris/%d: %v", c.path, i, err)
			}
		}
		for i, o := range c.origins {
			if err := checkWebOrigin(o); err != nil {
				fail("%s/web_origins/%d: %v", c.path, i, err)
			}
		}
	}
	problems = append(problems, m.extensionProblems()...)
	if len(problems) > 0 {
		return invalid(problems)
	}
	return nil
}

// realmName is the form of a brand realm name.
var realmName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)

// SecretRefChecker decides whether a brand may use a secret reference.
// secrets.Default implements it.
type SecretRefChecker interface {
	Check(slug, ref string) error
}

// CheckSecretRefs returns secret_ref_invalid when any secret reference of the
// manifest is outside the brand's namespace: env:// references must start
// with BRAND_<SLUG>__ and file:// references must resolve inside
// ZS_SECRETS_DIR/<slug>/. Apply runs it before it resolves anything.
func (m *Manifest) CheckSecretRefs(c SecretRefChecker) error {
	var problems []string
	for _, ref := range m.secretRefs() {
		if err := c.Check(m.Slug, ref.ref); err != nil {
			problems = append(problems, ref.path+": "+refProblem(err))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return zserrors.New(zserrors.CodeSecretRefInvalid, "Secret references outside the brand's namespace: "+strings.Join(problems, "; ")+".")
}

// refProblem returns the rule a reference breaks. The error of a checker
// names the rule and the reference, never a value.
func refProblem(err error) string {
	return strings.TrimPrefix(err.Error(), secrets.ErrNotAllowed.Error()+": ")
}

type secretRefAt struct{ path, ref string }

// secretRefs lists every secret reference in the manifest with its path.
func (m *Manifest) secretRefs() []secretRefAt {
	refs := []secretRefAt{
		{"/identity/staff_client/secret_ref", m.Identity.StaffClient.SecretRef},
		{"/billing/killbill_api_secret_ref", m.Billing.KillBillAPISecretRef},
	}
	for i, sc := range m.Identity.ServiceClients {
		refs = append(refs, secretRefAt{fmt.Sprintf("/identity/service_clients/%d/secret_ref", i), sc.SecretRef})
	}
	for i, p := range m.Identity.IdentityProviders {
		if p.ClientSecretRef != "" {
			refs = append(refs, secretRefAt{fmt.Sprintf("/identity/identity_providers/%d/client_secret_ref", i), p.ClientSecretRef})
		}
	}
	for i, pp := range m.Billing.Plugins() {
		refs = append(refs, secretRefAt{m.Billing.pluginsPath(i) + "/config_ref", pp.ConfigRef})
	}
	if ip := m.Billing.InvoicePlugin; ip != nil {
		refs = append(refs, secretRefAt{"/billing/invoice_plugin/config_ref", ip.ConfigRef})
	}
	if e := m.Billing.Email; e != nil {
		if e.SMTP.UsernameRef != "" {
			refs = append(refs, secretRefAt{"/billing/email/smtp/username_ref", e.SMTP.UsernameRef})
		}
		if e.SMTP.PasswordRef != "" {
			refs = append(refs, secretRefAt{"/billing/email/smtp/password_ref", e.SMTP.PasswordRef})
		}
	}
	if m.Notifications != nil {
		for i, w := range m.Notifications.Webhooks {
			refs = append(refs, secretRefAt{fmt.Sprintf("/notifications/webhooks/%d/secret_ref", i), w.SecretRef})
		}
	}
	return refs
}

// Digest returns "sha256:<hex>" of the manifest's canonical JSON encoding.
// Two manifests with the same content have the same digest whatever their
// key order or formatting.
func (m *Manifest) Digest() string {
	encoded, err := json.Marshal(m)
	if err != nil {
		// A Manifest holds only strings, numbers, booleans and slices.
		panic(err)
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ClientIDs returns every client the brand declares.
func (m *Manifest) ClientIDs() []string {
	ids := []string{m.Identity.CustomerClient.ClientID, m.Identity.StaffClient.ClientID}
	for _, sc := range m.Identity.ServiceClients {
		ids = append(ids, sc.ClientID)
	}
	return ids
}

func containsString(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// LoadDir loads every *.yaml and *.yml manifest in dir. The file name must be
// <slug>.yaml. It fails on the first invalid manifest, naming the file.
func LoadDir(dir string) ([]*Manifest, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read brands directory: %w", err)
	}
	var out []*Manifest
	seen := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		ext := filepath.Ext(name)
		if e.IsDir() || (ext != ".yaml" && ext != ".yml") {
			continue
		}
		m, err := Load(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if strings.TrimSuffix(name, ext) != m.Slug {
			return nil, fmt.Errorf("%s: the file name must be %s%s", name, m.Slug, ext)
		}
		if prev, ok := seen[m.Slug]; ok {
			return nil, fmt.Errorf("%s: slug %s is also declared in %s", name, m.Slug, prev)
		}
		seen[m.Slug] = name
		out = append(out, m)
	}
	return out, nil
}
