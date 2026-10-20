package doctor

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/zsoftly/zs-core/internal/components"
)

// checkKillBill covers the health check, the server version and the plugin
// list. /1.0/healthcheck is served without authentication. /1.0/kb/nodesInfo
// needs the admin credentials. The pinned image has no /1.0/kb/healthcheck.
func checkKillBill(ctx context.Context, opts Options) (ComponentReport, []Finding) {
	const name = components.KillBill
	t := opts.KillBill
	report := ComponentReport{Name: name}
	var findings []Finding
	add := func(check string, status Status, format string, args ...any) {
		findings = append(findings, Finding{name, check, status, fmt.Sprintf(format, args...)})
	}

	pin, _ := opts.Manifest.Component(name)
	report.Expected = pin.Version
	if t.URL == "" {
		add("reachable", Fail, "ZS_KILLBILL_URL is not set")
		return report, findings
	}

	var health map[string]struct {
		Healthy *bool `json:"healthy"`
	}
	status, err := getJSON(ctx, opts.HTTPClient, t.URL+"/1.0/healthcheck", "", "", &health)
	if err != nil {
		return report, append(findings, unreachable(name, err))
	}
	report.Reachable = true
	if status == http.StatusOK {
		var unhealthy []string
		for check, result := range health {
			if result.Healthy != nil && !*result.Healthy {
				unhealthy = append(unhealthy, check)
			}
		}
		sort.Strings(unhealthy)
		if len(unhealthy) > 0 {
			add("health", Fail, "unhealthy checks: %s", strings.Join(unhealthy, ", "))
		} else {
			add("health", OK, "healthy at %s", t.URL)
		}
	} else {
		add("health", Fail, "GET /1.0/healthcheck returned HTTP %d", status)
	}

	if t.AdminUser == "" {
		add("version", Warn, "version and plugins not verified: set ZS_KILLBILL_ADMIN_USER and ZS_KILLBILL_ADMIN_PASSWORD (pin is %s)", pin.Version)
		return report, findings
	}

	var nodes []struct {
		KBVersion   string `json:"kbVersion"`
		PluginsInfo []struct {
			BundleSymbolicName string `json:"bundleSymbolicName"`
			PluginKey          string `json:"pluginKey"`
			PluginName         string `json:"pluginName"`
			Version            string `json:"version"`
			State              string `json:"state"`
		} `json:"pluginsInfo"`
	}
	status, err = getJSON(ctx, opts.HTTPClient, t.URL+"/1.0/kb/nodesInfo", t.AdminUser, t.AdminPassword, &nodes)
	switch {
	case err != nil:
		add("version", Fail, "cannot read node info: %s", shorten(err.Error()))
		return report, findings
	case status != http.StatusOK:
		add("version", Fail, "GET /1.0/kb/nodesInfo returned HTTP %d", status)
		return report, findings
	case len(nodes) == 0:
		add("version", Fail, "node info lists no nodes")
		return report, findings
	}

	versions := map[string]bool{}
	for _, n := range nodes {
		versions[n.KBVersion] = true
	}
	if len(versions) > 1 {
		add("version", Fail, "nodes run different versions: %s", joinKeys(versions))
	} else {
		report.Version = nodes[0].KBVersion
		f, matches := versionFinding(name, report.Version, pin.Version)
		report.MatchesPin = matches
		findings = append(findings, f)
	}

	for _, p := range nodes[0].PluginsInfo {
		info := PluginInfo{Key: p.PluginKey, Name: p.PluginName, Version: p.Version, State: p.State}
		report.Plugins = append(report.Plugins, info)
		if p.PluginKey == "" {
			continue // platform bundle, versioned with Kill Bill itself
		}
		label := p.PluginKey
		if pinned, ok := opts.Manifest.PinnedPlugin(p.PluginKey); ok {
			if p.Version != pinned.Version {
				add("plugin", Fail, "plugin %s version %s does not match the pin %s in components.yaml", label, p.Version, pinned.Version)
			} else {
				add("plugin", OK, "plugin %s version %s matches the pin (%s)", label, p.Version, p.State)
			}
		} else {
			add("plugin", Warn, "plugin %s version %s is not listed in components.yaml (%s)", label, p.Version, p.State)
		}
		if p.State != "RUNNING" {
			add("plugin", Warn, "plugin %s is %s", label, p.State)
		}
	}
	platform := 0
	for _, p := range report.Plugins {
		if p.Key == "" {
			platform++
		}
	}
	add("plugins", OK, "%d platform bundle(s), %d plugin(s) installed", platform, len(report.Plugins)-platform)
	return report, findings
}

func joinKeys(m map[string]bool) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}
