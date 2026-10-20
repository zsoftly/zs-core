// Package doctor checks that a running stack matches the Core release pinned
// in components.yaml. It reports one finding per fact it could establish and
// never changes anything.
package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/zsoftly/zs-core/internal/components"
)

// Status of a finding.
type Status string

const (
	// OK means the fact was checked and is as expected.
	OK Status = "ok"
	// Warn means the fact could not be fully checked or is worth a look. It
	// does not fail the run.
	Warn Status = "warn"
	// Fail means the stack does not match the release. It fails the run.
	Fail Status = "fail"
	// Skip means a pin could not be checked because the configuration does
	// not allow it. It fails the run unless skips are allowed.
	Skip Status = "skip"
)

// DefaultMaxClockSkew is the largest accepted difference between the clock of
// Core and the clock of the database.
const DefaultMaxClockSkew = 5 * time.Second

// Finding is one line of the report.
type Finding struct {
	Component string `json:"component"`
	Check     string `json:"check"`
	Status    Status `json:"status"`
	Message   string `json:"message"`
}

// String renders the finding as one line.
func (f Finding) String() string {
	return fmt.Sprintf("%-4s %s: %s", strings.ToUpper(string(f.Status)), f.Component, f.Message)
}

// PluginInfo is a Kill Bill plugin reported by the server.
type PluginInfo struct {
	Key     string `json:"key,omitempty"`
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	State   string `json:"state"`
}

// ComponentReport summarises one component.
type ComponentReport struct {
	Name      string `json:"name"`
	Reachable bool   `json:"reachable"`
	Version   string `json:"version,omitempty"`
	Expected  string `json:"expected,omitempty"`
	// MatchesPin is nil when the version could not be determined.
	MatchesPin *bool          `json:"matches_pin,omitempty"`
	Plugins    []PluginInfo   `json:"plugins,omitempty"`
	Extra      map[string]any `json:"extra,omitempty"`
}

// Report is the result of a run.
type Report struct {
	CoreRelease string            `json:"core_release"`
	OK          bool              `json:"ok"`
	Components  []ComponentReport `json:"components"`
	Findings    []Finding         `json:"findings"`
}

// Failed reports whether any finding is a failure.
func (r Report) Failed() bool {
	for _, f := range r.Findings {
		if f.Status == Fail {
			return true
		}
	}
	return false
}

// Skipped reports whether any finding is a skip.
func (r Report) Skipped() bool {
	for _, f := range r.Findings {
		if f.Status == Skip {
			return true
		}
	}
	return false
}

// WriteText writes one line per finding.
func (r Report) WriteText(w io.Writer) error {
	for _, f := range r.Findings {
		if _, err := fmt.Fprintln(w, f.String()); err != nil {
			return err
		}
	}
	return nil
}

// WriteJSON writes the report as indented JSON.
func (r Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// Target describes how to reach one HTTP component.
type Target struct {
	URL           string
	AdminUser     string
	AdminPassword string
	// Realm is the Keycloak realm that must exist. Unused for other components.
	Realm string
}

// Options configure a run.
type Options struct {
	Manifest     components.Manifest
	Database     Database
	Keycloak     Target
	KillBill     Target
	KAUI         Target
	HTTPClient   *http.Client
	Now          func() time.Time
	MaxClockSkew time.Duration
}

// Run performs every check and returns the report. Checks of different
// components are independent: one failing does not stop the others.
func Run(ctx context.Context, opts Options) Report {
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.MaxClockSkew <= 0 {
		opts.MaxClockSkew = DefaultMaxClockSkew
	}
	r := Report{CoreRelease: opts.Manifest.CoreRelease}
	add := func(cs []ComponentReport, fs []Finding) {
		r.Components = append(r.Components, cs...)
		r.Findings = append(r.Findings, fs...)
	}
	one := func(c ComponentReport, fs []Finding) ([]ComponentReport, []Finding) {
		return []ComponentReport{c}, fs
	}
	add(checkCore(ctx, opts))
	add(one(checkKeycloak(ctx, opts)))
	add(one(checkKillBill(ctx, opts)))
	add(one(checkKAUI(ctx, opts)))
	r.OK = !r.Failed() && !r.Skipped()
	return r
}

func boolPtr(b bool) *bool { return &b }

// versionFinding compares a reported version with the pin.
func versionFinding(component, got, pinned string) (Finding, *bool) {
	return compareVersion(component, got, got, pinned)
}

// compareVersion compares comparable with the pin and shows shown in the
// message. They differ for Postgres, where the pin is a major version.
func compareVersion(component, shown, comparable, pinned string) (Finding, *bool) {
	if comparable == pinned {
		return Finding{component, "version", OK, fmt.Sprintf("version %s matches the pin %s", shown, pinned)}, boolPtr(true)
	}
	return Finding{component, "version", Fail, fmt.Sprintf("version %s does not match the pin %s in components.yaml", shown, pinned)}, boolPtr(false)
}

// getJSON performs a GET and decodes a JSON body. It returns the status code
// and any transport or decode error. A non-2xx status is not an error here.
func getJSON(ctx context.Context, client *http.Client, url, user, password string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	if user != "" {
		req.SetBasicAuth(user, password)
	}
	req.Header.Set("Accept", "application/json")
	return do(client, req, out)
}

func do(client *http.Client, req *http.Request, out any) (int, error) {
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if out != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if err := json.Unmarshal(body, out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode response: %w", err)
		}
	}
	return resp.StatusCode, nil
}

// redactURL returns raw with any password masked, for printing. Configuration
// rejects URLs with user information, so this is a second line of defence.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<invalid URL>"
	}
	return u.Redacted()
}

// unreachable builds the failure finding for a transport error. The error text
// can hold the URL, which is operator supplied and holds no credentials.
func unreachable(component string, err error) Finding {
	return Finding{component, "reachable", Fail, "not reachable: " + shorten(err.Error())}
}

func shorten(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}
