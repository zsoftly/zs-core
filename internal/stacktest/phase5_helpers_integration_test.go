//go:build integration

package stacktest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zsoftly/zs-core/internal/components"
	"github.com/zsoftly/zs-core/internal/doctor"
	"github.com/zsoftly/zs-core/internal/httpapi"
	"github.com/zsoftly/zs-core/internal/kbplugin"
	"github.com/zsoftly/zs-core/internal/keycloak"
	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/internal/pdfrender"
	"github.com/zsoftly/zs-core/internal/postgrestest"
	"github.com/zsoftly/zs-core/internal/secrets"
	"github.com/zsoftly/zs-core/migrations"
	"github.com/zsoftly/zs-core/pkg/billing"
	"github.com/zsoftly/zs-core/pkg/brand"
	"github.com/zsoftly/zs-core/pkg/identity"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

// The Phase 5 stack tests read these besides the variables of newStack.
// See docs/development.md.
const (
	envKillBillDB    = "ZS_TEST_KILLBILL_DATABASE_URL"
	envMirrorURL     = "ZS_TEST_PLUGIN_MIRROR_URL"
	envMirrorSecret  = "ZS_TEST_PLUGIN_MIRROR_SECRET"
	envCoreURL       = "ZS_TEST_CORE_URL"
	envRendererURL   = "ZS_TEST_PDF_RENDERER_URL"
	envMailpitURL    = "ZS_TEST_MAILPIT_URL"
	nodeCommandStop  = "STOP_PLUGIN"
	nodeCommandUnins = "UNINSTALL_PLUGIN"
)

func pins(t *testing.T) components.Manifest {
	t.Helper()
	m, err := components.Load(filepath.Join("..", "..", "components.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func pin(t *testing.T, m components.Manifest, name string) components.Plugin {
	t.Helper()
	p, ok := m.PinnedPlugin(name)
	if !ok {
		t.Fatalf("plugin %s is not pinned", name)
	}
	return p
}

// killBillDB opens the stack's Kill Bill database as the killbill role.
func killBillDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), env(t, envKillBillDB))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func tablesIn(kb kbplugin.Querier) func(context.Context, components.Plugin) ([]string, error) {
	return func(ctx context.Context, p components.Plugin) ([]string, error) {
		return kbplugin.MissingTables(ctx, kb, p)
	}
}

// pluginOnNodes returns the plugin's version and state on the first node,
// or "" when it is not installed.
func pluginOnNodes(t *testing.T, s stack, name string) (string, string) {
	t.Helper()
	nodes, err := s.kb.NodesInfo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		for _, p := range n.PluginsInfo {
			if p.PluginKey == name {
				return p.Version, p.State
			}
		}
	}
	return "", ""
}

// uninstallPlugin stops and uninstalls a plugin through KPM, so a test
// starts from a stack where the plugin is absent.
func uninstallPlugin(t *testing.T, s stack, p components.Plugin) {
	t.Helper()
	version, _ := pluginOnNodes(t, s, p.Name)
	if version == "" {
		return
	}
	cc := killbill.CallContext{CreatedBy: "stack-test"}
	props := []killbill.NodeCommandProperty{{Key: "pluginKey", Value: p.Name}, {Key: "pluginVersion", Value: version}}
	ctx := context.Background()
	if err := s.kb.SendNodeCommand(ctx, cc, killbill.NodeCommand{NodeCommandType: nodeCommandStop, NodeCommandProperties: props}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	if err := s.kb.SendNodeCommand(ctx, cc, killbill.NodeCommand{NodeCommandType: nodeCommandUnins, IsSystemCommandType: true, NodeCommandProperties: props}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if v, _ := pluginOnNodes(t, s, p.Name); v == "" {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("plugin %s is still installed", p.Name)
}

// killBillFindings runs doctor's Kill Bill checks with the table check.
func killBillFindings(t *testing.T, m components.Manifest, tables func(context.Context, components.Plugin) ([]string, error)) []doctor.Finding {
	t.Helper()
	r := doctor.Run(context.Background(), doctor.Options{Manifest: m, PluginTables: tables, HTTPClient: http.DefaultClient,
		KillBill: doctor.Target{URL: os.Getenv("ZS_TEST_KILLBILL_URL"), AdminUser: "admin", AdminPassword: os.Getenv("ZS_TEST_KILLBILL_ADMIN_PASSWORD")}})
	var out []doctor.Finding
	for _, f := range r.Findings {
		if f.Component == "killbill" {
			out = append(out, f)
		}
	}
	return out
}

func findingLines(fs []doctor.Finding) string {
	var lines []string
	for _, f := range fs {
		lines = append(lines, f.String())
	}
	return strings.Join(lines, "\n")
}

// copyDir copies a directory tree, so a test can edit a brand's files.
func copyDir(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, path)
		target := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func replaceInFile(t *testing.T, path, old, new string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), old) {
		t.Fatalf("%s does not hold %q", path, old)
	}
	writeFile(t, path, strings.Replace(string(data), old, new, 1))
}

// createUserWithLocale creates a user whose tokens carry a locale claim.
func createUserWithLocale(t *testing.T, kc *keycloak.Client, realm, username, password, locale string) {
	t.Helper()
	ctx := context.Background()
	id, err := kc.CreateUser(ctx, realm, keycloak.Object{
		"username": username, "email": username + "@example.test", "emailVerified": true, "enabled": true,
		"firstName": "Stack", "lastName": "Test", "requiredActions": []string{},
		"attributes":  map[string][]string{"locale": {locale}},
		"credentials": []map[string]any{{"type": "password", "value": password, "temporary": false}},
	})
	if err != nil {
		t.Fatalf("create user %s in %s: %v", username, realm, err)
	}
	t.Cleanup(func() { _ = kc.DeleteUser(context.Background(), realm, id) })
}

func claimOf(t *testing.T, token, claim string) string {
	t.Helper()
	parts := strings.Split(token, ".")
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var c map[string]any
	_ = json.Unmarshal(data, &c)
	v, _ := c[claim].(string)
	return v
}

// billingAPI serves Core's API for billing tests, with the PDF renderer of
// ZS_TEST_PDF_RENDERER_URL when it is set.
func billingAPI(t *testing.T, s stack, db *postgrestest.DB, deps brand.Deps) (apiServer, *billing.Service) {
	t.Helper()
	svc := &billing.Service{DB: db.Pool, KillBill: s.kb, Secrets: secrets.Default{}, WebhookSecret: []byte(strings.Repeat("w", 40))}
	if u := os.Getenv(envRendererURL); u != "" {
		r, err := pdfrender.New(u, nil)
		if err != nil {
			t.Fatal(err)
		}
		svc.PDF = r
	}
	reg := brand.NewRegistry(db.Pool, time.Second, nil)
	srv := httptest.NewServer(httpapi.New(httpapi.Options{DB: db.Pool, Migrations: migrations.FS, Brands: reg, BrandDeps: deps,
		Tenants: tenant.Engines{Keycloak: s.kc, KillBill: s.kb, Secrets: secrets.Default{}}, Billing: svc,
		Verifier: &identity.Verifier{Brands: reg, Keys: keycloak.NewKeySet(s.kcURL, keycloak.KeySetOptions{})}}))
	t.Cleanup(srv.Close)
	return apiServer{srv}, svc
}

// raw makes a request to Core and returns the status, headers and body.
func (a apiServer) raw(t *testing.T, method, path, token string) (int, http.Header, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, a.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, data
}

// newTenantFor creates a tenant through Core as the token's user.
func newTenantFor(t *testing.T, api apiServer, token, name string) string {
	t.Helper()
	status, out := api.call(t, http.MethodPost, "/v1/tenants", token, jsonBody(t, map[string]any{"idempotency_key": "p5-" + strings.ReplaceAll(name, " ", "-"), "name": name}), "application/json")
	if status != http.StatusCreated {
		t.Fatalf("create tenant %s = %d %s", name, status, mustJSON(out))
	}
	return out["id"].(string)
}

// taxedInvoice makes a committed invoice on an account with a charge and a
// TAX item, as an invoice plugin would add, through Kill Bill's API.
func taxedInvoice(t *testing.T, creds killbill.TenantCredentials, accountID, charge, tax string) string {
	t.Helper()
	status, _, body := kbRaw(t, creds, http.MethodPost, "/1.0/kb/invoices/charges/"+accountID+"?autoCommit=false",
		[]map[string]any{{"accountId": accountID, "amount": 100.00, "currency": "CAD", "description": charge}})
	var items []killbill.InvoiceItem
	if status != http.StatusOK || json.Unmarshal(body, &items) != nil || len(items) != 1 {
		t.Fatalf("charge = %d %s", status, body)
	}
	inv := items[0].InvoiceID
	status, _, body = kbRaw(t, creds, http.MethodPost, "/1.0/kb/invoices/taxes/"+accountID+"?autoCommit=false",
		[]map[string]any{{"accountId": accountID, "invoiceId": inv, "amount": 13.00, "currency": "CAD", "description": tax}})
	if status != http.StatusOK {
		t.Fatalf("tax item = %d %s", status, body)
	}
	if status, _, body := kbRaw(t, creds, http.MethodPut, "/1.0/kb/invoices/"+inv+"/commitInvoice", nil); status != http.StatusNoContent {
		t.Fatalf("commit = %d %s", status, body)
	}
	return inv
}

// accountOf returns the Kill Bill account of a Core tenant.
func accountOf(t *testing.T, creds killbill.TenantCredentials, tenantID string) map[string]any {
	t.Helper()
	status, _, body := kbRaw(t, creds, http.MethodGet, "/1.0/kb/accounts?externalKey="+url.QueryEscape(tenantID), nil)
	var a map[string]any
	if status != http.StatusOK || json.Unmarshal(body, &a) != nil {
		t.Fatalf("account of %s = %d %s", tenantID, status, body)
	}
	return a
}

// mailpitMessage is a message Mailpit caught.
type mailpitMessage struct {
	ID      string `json:"ID"`
	Subject string `json:"Subject"`
	From    struct {
		Address string `json:"Address"`
	} `json:"From"`
	To []struct {
		Address string `json:"Address"`
	} `json:"To"`
}

// waitForMail waits for a message to an address and returns it with its
// HTML body.
func waitForMail(t *testing.T, to string) (mailpitMessage, string) {
	t.Helper()
	base := strings.TrimRight(env(t, envMailpitURL), "/")
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/api/v1/search?query=" + url.QueryEscape("to:"+to))
		if err == nil {
			var list struct {
				Messages []mailpitMessage `json:"messages"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&list)
			resp.Body.Close()
			if len(list.Messages) > 0 {
				m := list.Messages[0]
				resp, err := http.Get(base + "/api/v1/message/" + m.ID)
				if err != nil {
					t.Fatal(err)
				}
				var full struct {
					HTML string `json:"HTML"`
				}
				_ = json.NewDecoder(resp.Body).Decode(&full)
				resp.Body.Close()
				return m, full.HTML
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("no email to %s in Mailpit", to)
	return mailpitMessage{}, ""
}
