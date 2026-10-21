package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/zsoftly/zs-core/internal/components"
	"github.com/zsoftly/zs-core/internal/kbplugin"
	"github.com/zsoftly/zs-core/internal/pdfrender"
)

// MirrorOptions describe the plugin mirror of the deployment.
type MirrorOptions struct {
	// Dir is ZS_PLUGIN_MIRROR_DIR, the mirror this Core serves.
	Dir string
	// URL is ZS_PLUGIN_MIRROR_URL, where Kill Bill downloads plugins.
	URL string
	// Secret is ZS_PLUGIN_MIRROR_SECRET, which signs the mirror manifest.
	Secret []byte
}

// Component names of the plugin checks.
const (
	componentPluginMirror = "plugin-mirror"
)

// checkPluginTables checks that the Kill Bill database has the tables of
// every pinned plugin that is installed, or that a stack must have. It
// needs ZS_KILLBILL_DATABASE_URL: without it the check is a skip.
func checkPluginTables(ctx context.Context, opts Options, installed map[string]bool) []Finding {
	const name = components.KillBill
	var pins []components.Plugin
	for _, p := range opts.Manifest.Plugins() {
		if len(p.Tables) > 0 && (installed[p.Name] || p.Required()) {
			pins = append(pins, p)
		}
	}
	if opts.PluginTables == nil {
		return []Finding{{name, "plugin_tables", Skip, "plugin tables not checked: ZS_KILLBILL_DATABASE_URL is not set"}}
	}
	if len(pins) == 0 {
		return []Finding{{name, "plugin_tables", OK, "no installed plugin has tables of its own"}}
	}
	var findings []Finding
	for _, p := range pins {
		missing, err := opts.PluginTables(ctx, p)
		switch {
		case err != nil:
			findings = append(findings, Finding{name, "plugin_tables", Fail, fmt.Sprintf("plugin %s %s tables not readable: %s", p.Name, p.Version, shorten(err.Error()))})
		case len(missing) > 0:
			findings = append(findings, Finding{name, "plugin_tables", Fail, fmt.Sprintf("plugin %s %s tables missing: %s; run zs-core plugin ddl apply --operator <operator> %s", p.Name, p.Version, strings.Join(missing, ", "), p.Name)})
		default:
			findings = append(findings, Finding{name, "plugin_tables", OK, fmt.Sprintf("plugin %s %s tables present: %s", p.Name, p.Version, strings.Join(p.Tables, ", "))})
		}
	}
	return findings
}

// checkMirror reports the state of the plugin mirror: where KPM installs
// plugins from and, when this Core serves the mirror, whether it holds
// every pinned jar with its pinned digest.
func checkMirror(opts Options) (ComponentReport, []Finding) {
	report := ComponentReport{Name: componentPluginMirror}
	add := func(findings []Finding, status Status, format string, args ...any) []Finding {
		return append(findings, Finding{componentPluginMirror, "mirror", status, fmt.Sprintf(format, args...)})
	}
	var findings []Finding
	m := opts.Mirror
	if m.Dir == "" {
		if m.URL == "" {
			return report, add(findings, OK, "mirror not configured: new plugin installation requires a configured approved mirror")
		}
		return report, add(findings, OK, "installs use the mirror at %s; this Core does not serve it", redactURL(m.URL))
	}
	report.Reachable = true
	if _, err := os.Stat(m.Dir); err != nil {
		status := Warn
		if m.URL != "" {
			status = Fail
		}
		return report, add(findings, status, "mirror directory %s is not readable: %s", m.Dir, shorten(err.Error()))
	}
	signed, err := kbplugin.ReadManifest(m.Dir)
	switch {
	case err != nil:
		findings = add(findings, Warn, "mirror manifest not readable (%s): run zs-core plugin mirror sync", shorten(err.Error()))
	case len(m.Secret) == 0:
		findings = add(findings, Warn, "mirror manifest signature not checked: ZS_PLUGIN_MIRROR_SECRET is not set")
	case signed.Verify(m.Secret) != nil:
		findings = add(findings, Fail, "mirror manifest is not signed with ZS_PLUGIN_MIRROR_SECRET; the mirror serves nothing until zs-core plugin mirror sync signs it again")
	default:
		findings = add(findings, OK, "mirror manifest signed, synced %s, %d jars", signed.SyncedAt.Format("2006-01-02T15:04:05Z"), len(signed.Plugins))
	}
	statuses := kbplugin.Check(m.Dir, opts.Manifest)
	sort.SliceStable(statuses, func(i, j int) bool { return statuses[i].Plugin.Name < statuses[j].Plugin.Name })
	for _, st := range statuses {
		label := st.Plugin.Name + " " + st.Plugin.Version
		switch {
		case st.Match:
			findings = add(findings, OK, "jar %s present and matches its sha256", label)
		case st.Present:
			findings = add(findings, Fail, "jar %s does not match its sha256 in components.yaml; the mirror refuses to serve it", label)
		case m.URL != "":
			findings = add(findings, Fail, "jar %s is missing and installs use the mirror: run zs-core plugin mirror sync", label)
		default:
			findings = add(findings, Warn, "jar %s is missing: run zs-core plugin mirror sync", label)
		}
	}
	return report, findings
}

// checkPDFRenderer checks the optional PDF renderer: reachable and at the
// pinned version. Without ZS_PDF_RENDERER_URL there is nothing to check.
func checkPDFRenderer(ctx context.Context, opts Options) (ComponentReport, []Finding) {
	const name = components.Gotenberg
	report := ComponentReport{Name: name}
	pin, pinned := opts.Manifest.Component(name)
	report.Expected = pin.Version
	if opts.PDFRendererURL == "" {
		return report, []Finding{{name, "reachable", OK, "not configured: ZS_PDF_RENDERER_URL is not set, so invoice PDFs answer 404 invoice_pdf_unavailable"}}
	}
	client, err := pdfrender.New(opts.PDFRendererURL, opts.HTTPClient)
	if err != nil {
		return report, []Finding{{name, "reachable", Fail, shorten(err.Error())}}
	}
	version, err := client.Version(ctx)
	if err != nil {
		if errors.Is(err, pdfrender.ErrUnavailable) {
			return report, []Finding{unreachable(name, err)}
		}
		return report, []Finding{{name, "reachable", Fail, shorten(err.Error())}}
	}
	report.Reachable = true
	report.Version = version
	if !pinned {
		return report, []Finding{{name, "version", Warn, fmt.Sprintf("version %s; components.yaml pins no %s", version, name)}}
	}
	f, matches := versionFinding(name, version, pin.Version)
	report.MatchesPin = matches
	return report, []Finding{f}
}
