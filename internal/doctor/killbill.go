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
			add("health", OK, "healthy at %s", redactURL(t.URL))
		}
	} else {
		add("health", Fail, "GET /1.0/healthcheck returned HTTP %d", status)
	}

	if t.AdminUser == "" || t.AdminPassword == "" {
		add("version", Skip, "version not checked (no admin credentials): set ZS_KILLBILL_ADMIN_USER and ZS_KILLBILL_ADMIN_PASSWORD; plugins not checked either (pin is %s)", pin.Version)
		return report, append(findings, checkPluginTables(ctx, opts, nil)...)
	}

	var nodes []struct {
		NodeName    string `json:"nodeName"`
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

	allInstalled := map[string]bool{}
	for i, node := range nodes {
		label := fmt.Sprintf("node %d", i+1)
		if node.NodeName != "" {
			label = fmt.Sprintf("node %q", node.NodeName)
		}
		nodeAdd := func(check string, status Status, format string, args ...any) {
			add(check, status, "%s (%s)", fmt.Sprintf(format, args...), label)
		}
		installed := map[string]bool{}
		platform := 0
		for _, p := range node.PluginsInfo {
			// Preserve the existing report inventory shape. Findings cover
			// every node; the summary list still describes the first node.
			if i == 0 {
				info := PluginInfo{Key: p.PluginKey, Name: p.PluginName, Version: p.Version, State: p.State}
				report.Plugins = append(report.Plugins, info)
			}
			if p.PluginKey == "" {
				platform++
				continue // platform bundle, versioned with Kill Bill itself
			}
			installed[p.PluginKey] = true
			allInstalled[p.PluginKey] = true
			pinned, ok := opts.Manifest.PinnedPlugin(p.PluginKey)
			if ok {
				if p.Version != pinned.Version {
					nodeAdd("plugin", Fail, "plugin %s version %s does not match the pin %s in components.yaml", p.PluginKey, p.Version, pinned.Version)
				} else {
					nodeAdd("plugin", OK, "plugin %s version %s matches the pin (%s)", p.PluginKey, p.Version, p.State)
				}
			} else {
				nodeAdd("plugin", Fail, "plugin %s version %s is not listed in components.yaml (%s)", p.PluginKey, p.Version, p.State)
			}
			if p.State != "RUNNING" {
				if ok && pinned.Required() {
					nodeAdd("plugin", Fail, "plugin %s is %s; components.yaml marks it %s, so it must be RUNNING", p.PluginKey, p.State, pinned.Status)
				} else {
					nodeAdd("plugin", Warn, "plugin %s is %s", p.PluginKey, p.State)
				}
			}
		}
		for _, pinned := range opts.Manifest.RequiredPlugins() {
			if !installed[pinned.Name] {
				nodeAdd("plugin", Fail, "plugin %s %s is not installed; components.yaml marks it %s", pinned.Name, pinned.Version, pinned.Status)
			}
		}
		nodeAdd("plugins", OK, "%d platform bundle(s), %d plugin(s) installed", platform, len(node.PluginsInfo)-platform)
	}
	return report, append(findings, checkPluginTables(ctx, opts, allInstalled)...)
}

func joinKeys(m map[string]bool) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}
