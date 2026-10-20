package doctor

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/zsoftly/zs-core/internal/components"
)

// checkKeycloak covers reachability, the realm and the server version.
// Reachability uses the master realm's public endpoint, which is served on the
// main port. The version comes from the Admin API server info, which needs the
// admin credentials: Keycloak does not expose its version anonymously.
func checkKeycloak(ctx context.Context, opts Options) (ComponentReport, []Finding) {
	const name = components.Keycloak
	t := opts.Keycloak
	report := ComponentReport{Name: name}
	var findings []Finding
	add := func(check string, status Status, format string, args ...any) {
		findings = append(findings, Finding{name, check, status, fmt.Sprintf(format, args...)})
	}

	pin, _ := opts.Manifest.Component(name)
	report.Expected = pin.Version
	if t.URL == "" {
		add("reachable", Fail, "ZS_KEYCLOAK_URL is not set")
		return report, findings
	}

	status, err := getJSON(ctx, opts.HTTPClient, t.URL+"/realms/master", "", "", nil)
	if err != nil {
		return report, append(findings, unreachable(name, err))
	}
	if status != http.StatusOK {
		add("reachable", Fail, "GET /realms/master returned HTTP %d", status)
		return report, findings
	}
	report.Reachable = true
	add("reachable", OK, "reachable at %s", redactURL(t.URL))

	if t.Realm == "" {
		add("realm", Skip, "realm not checked (ZS_KEYCLOAK_REALM is not set)")
	} else {
		status, err := getJSON(ctx, opts.HTTPClient, t.URL+"/realms/"+url.PathEscape(t.Realm), "", "", nil)
		switch {
		case err != nil:
			add("realm", Fail, "cannot check realm %s: %s", t.Realm, shorten(err.Error()))
		case status == http.StatusOK:
			add("realm", OK, "realm %s present", t.Realm)
		default:
			add("realm", Fail, "realm %s not found (HTTP %d)", t.Realm, status)
		}
	}

	if t.AdminUser == "" || t.AdminPassword == "" {
		add("version", Skip, "version not checked (no admin credentials): set ZS_KEYCLOAK_ADMIN_USER and ZS_KEYCLOAK_ADMIN_PASSWORD (pin is %s)", pin.Version)
		return report, findings
	}
	version, err := keycloakVersion(ctx, opts.HTTPClient, t)
	if err != nil {
		add("version", Fail, "cannot read the server version: %s", shorten(err.Error()))
		return report, findings
	}
	report.Version = version
	f, matches := versionFinding(name, version, pin.Version)
	report.MatchesPin = matches
	findings = append(findings, f)
	return report, findings
}

func keycloakVersion(ctx context.Context, client *http.Client, t Target) (string, error) {
	form := url.Values{
		"grant_type": {"password"},
		"client_id":  {"admin-cli"},
		"username":   {t.AdminUser},
		"password":   {t.AdminPassword},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.URL+"/realms/master/protocol/openid-connect/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var token struct {
		AccessToken string `json:"access_token"`
	}
	status, err := do(client, req, &token)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK || token.AccessToken == "" {
		return "", fmt.Errorf("admin login returned HTTP %d", status)
	}

	req, err = http.NewRequestWithContext(ctx, http.MethodGet, t.URL+"/admin/serverinfo", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	req.Header.Set("Accept", "application/json")
	var info struct {
		SystemInfo struct {
			Version string `json:"version"`
		} `json:"systemInfo"`
	}
	status, err = do(client, req, &info)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK || info.SystemInfo.Version == "" {
		return "", fmt.Errorf("server info returned HTTP %d", status)
	}
	return info.SystemInfo.Version, nil
}
