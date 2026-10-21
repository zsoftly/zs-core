// Package components reads components.yaml, the single place where the
// component versions of a Core release are pinned. See
// docs/decisions/0003-never-fork-upstream.md.
package components

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Component names used in components.yaml.
const (
	Keycloak = "keycloak"
	KillBill = "killbill"
	KAUI     = "kaui"
	Postgres = "postgres"
	// Gotenberg is the optional PDF renderer of invoice PDFs.
	Gotenberg = "gotenberg"
)

// Component is one pinned upstream component. Licence is the SPDX
// identifier of its licence. Optional marks a component a deployment may
// leave out, such as the PDF renderer.
type Component struct {
	Version  string `yaml:"version"`
	Image    string `yaml:"image"`
	Digest   string `yaml:"digest"`
	Released string `yaml:"released"`
	Source   string `yaml:"source"`
	Licence  string `yaml:"licence"`
	Optional bool   `yaml:"optional"`
}

// Plugin is a pinned Kill Bill plugin. Sha256 is the SHA-256 of the jar in
// the repository, in lowercase hex. Core checks it before it asks KPM to
// install the jar. Tables are the tables the plugin's ddl.sql creates in the
// Kill Bill database: zs-core plugin ddl apply refuses a DDL that creates
// other tables, and doctor checks they exist.
type Plugin struct {
	Name     string   `yaml:"name"`
	Version  string   `yaml:"version"`
	Artifact string   `yaml:"artifact"`
	Sha256   string   `yaml:"sha256"`
	Tables   []string `yaml:"tables"`
	Source   string   `yaml:"source"`
	Status   string   `yaml:"status"`
}

// KillBillPlugins are the plugins Core knows about. Repository supplies Maven
// jars to mirror sync and DDL commands. KPM installs from the verified mirror.
type KillBillPlugins struct {
	Repository             string   `yaml:"repository"`
	ApprovedPaymentPlugins []Plugin `yaml:"approved_payment_plugins"`
	ApprovedInvoicePlugins []Plugin `yaml:"approved_invoice_plugins"`
	Installable            []Plugin `yaml:"installable"`
	PlatformBundlesInImage []string `yaml:"platform_bundles_in_image"`
	PluginFramework        string   `yaml:"plugin_framework"`
}

// Manifest is the parsed components.yaml.
type Manifest struct {
	CoreRelease     string               `yaml:"core_release"`
	Components      map[string]Component `yaml:"components"`
	KillBillPlugins KillBillPlugins      `yaml:"killbill_plugins"`
}

// Load reads and validates the file at path.
func Load(path string) (Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("read components file: %w", err)
	}
	return Parse(data)
}

// Parse parses and validates the contents of a components.yaml file. Unknown
// keys are an error, so a typo cannot silently drop a pin.
func Parse(data []byte) (Manifest, error) {
	var m Manifest
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("parse components file: %w", err)
	}
	if err := m.validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

func (m Manifest) validate() error {
	var problems []string
	if m.CoreRelease == "" {
		problems = append(problems, "core_release is missing")
	}
	for _, name := range []string{Keycloak, KillBill, KAUI, Postgres} {
		c, ok := m.Components[name]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("components.%s is missing", name))
		case c.Version == "":
			problems = append(problems, fmt.Sprintf("components.%s.version is missing", name))
		case c.Image == "":
			problems = append(problems, fmt.Sprintf("components.%s.image is missing", name))
		}
	}
	names := make([]string, 0, len(m.Components))
	for name := range m.Components {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		switch digest := m.Components[name].Digest; {
		case digest == "":
			problems = append(problems, fmt.Sprintf("components.%s.digest is missing", name))
		case !digestPattern.MatchString(digest):
			problems = append(problems, fmt.Sprintf("components.%s.digest %q is not sha256:<64 lowercase hex>", name, digest))
		}
	}
	seenPlugins := map[string]string{}
	for _, list := range []struct {
		path    string
		plugins []Plugin
	}{
		{"killbill_plugins.approved_payment_plugins", m.KillBillPlugins.ApprovedPaymentPlugins},
		{"killbill_plugins.approved_invoice_plugins", m.KillBillPlugins.ApprovedInvoicePlugins},
		{"killbill_plugins.installable", m.KillBillPlugins.Installable},
	} {
		for _, p := range list.plugins {
			switch p.Status {
			case PluginStatusRequired, PluginStatusVerifiedInstall, PluginStatusNoJavaArtifact, PluginStatusPinnedNotInstalled:
			case "":
				problems = append(problems, fmt.Sprintf("%s %s.status is missing", list.path, p.Name))
			default:
				problems = append(problems, fmt.Sprintf("%s %s.status %q is not a known plugin status", list.path, p.Name, p.Status))
			}
			switch {
			case p.Artifact == "" && p.Sha256 != "":
				problems = append(problems, fmt.Sprintf("%s %s has a sha256 but no artifact", list.path, p.Name))
			case p.Artifact != "" && !sha256Pattern.MatchString(p.Sha256):
				problems = append(problems, fmt.Sprintf("%s %s.sha256 %q is not 64 lowercase hex", list.path, p.Name, p.Sha256))
			}
			if !pluginName.MatchString(p.Name) {
				problems = append(problems, fmt.Sprintf("%s %q is not a plugin name of lowercase letters, digits and dashes", list.path, p.Name))
			}
			if prev, ok := seenPlugins[p.Name]; ok {
				problems = append(problems, fmt.Sprintf("%s %s is also listed in %s", list.path, p.Name, prev))
			}
			seenPlugins[p.Name] = list.path
			for _, t := range p.Tables {
				if !tableName.MatchString(t) {
					problems = append(problems, fmt.Sprintf("%s %s table %q is not a lowercase table name", list.path, p.Name, t))
				}
			}
			if len(p.Tables) > 0 && p.Artifact == "" {
				problems = append(problems, fmt.Sprintf("%s %s has tables but no artifact", list.path, p.Name))
			}
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return errors.New("invalid components file: " + strings.Join(problems, "; "))
	}
	return nil
}

// digestPattern is the form of an image digest pin, and sha256Pattern the
// form of a plugin jar digest.
var (
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	pluginName    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	tableName     = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)
)

// PinnedReference returns the image reference pinned by digest, such as
// postgres@sha256:..., which is what the reference stack pulls.
func (c Component) PinnedReference() string {
	return ImageRepository(c.Image) + "@" + c.Digest
}

// Component returns the named component.
func (m Manifest) Component(name string) (Component, bool) {
	c, ok := m.Components[name]
	return c, ok
}

// Plugin statuses in components.yaml. PluginStatusRequired marks a plugin a
// running stack must have installed and running. The other three statuses are
// informational: a plugin is installed when a brand manifest names it.
const (
	PluginStatusRequired           = "required"
	PluginStatusVerifiedInstall    = "verified_install"
	PluginStatusNoJavaArtifact     = "no_java_artifact"
	PluginStatusPinnedNotInstalled = "pinned_not_installed"
)

// Required reports whether a running stack must have the plugin installed and
// running: only a plugin with status required. Brand apply installs the
// plugins a brand names, so a stack without brands needs none.
func (p Plugin) Required() bool {
	return p.Status == PluginStatusRequired
}

// RequiredPlugins returns the approved payment and invoice plugins and the
// installable plugins that a running stack must have.
func (m Manifest) RequiredPlugins() []Plugin {
	var required []Plugin
	for _, p := range m.Plugins() {
		if p.Required() {
			required = append(required, p)
		}
	}
	return required
}

// Plugins returns every pinned plugin: the approved payment plugins, the
// approved invoice plugins and the installable plugins, in that order.
func (m Manifest) Plugins() []Plugin {
	k := m.KillBillPlugins
	out := make([]Plugin, 0, len(k.ApprovedPaymentPlugins)+len(k.ApprovedInvoicePlugins)+len(k.Installable))
	out = append(out, k.ApprovedPaymentPlugins...)
	out = append(out, k.ApprovedInvoicePlugins...)
	return append(out, k.Installable...)
}

// Artifacts returns the pinned plugins that have a Java artifact, the ones
// KPM can install and the mirror holds.
func (m Manifest) Artifacts() []Plugin {
	var out []Plugin
	for _, p := range m.Plugins() {
		if p.HasArtifact() {
			out = append(out, p)
		}
	}
	return out
}

// HasArtifact reports whether the plugin has a Java artifact with a version
// and a digest.
func (p Plugin) HasArtifact() bool {
	return p.Artifact != "" && p.Version != "" && p.Sha256 != "" && p.Status != PluginStatusNoJavaArtifact
}

// PinnedPlugin returns the pinned version of a Kill Bill plugin by name,
// searching every pinned plugin.
func (m Manifest) PinnedPlugin(name string) (Plugin, bool) {
	for _, p := range m.Plugins() {
		if p.Name == name {
			return p, true
		}
	}
	return Plugin{}, false
}

// ImageTag returns the part of an image reference after the last colon that
// follows the last slash, or "" when the reference has no tag.
func ImageTag(image string) string {
	slash := strings.LastIndex(image, "/")
	colon := strings.LastIndex(image, ":")
	if colon <= slash {
		return ""
	}
	return image[colon+1:]
}

// ImageRepository returns an image reference without its tag or digest.
func ImageRepository(image string) string {
	if at := strings.Index(image, "@"); at >= 0 {
		image = image[:at]
	}
	if tag := ImageTag(image); tag != "" {
		image = image[:len(image)-len(tag)-1]
	}
	return image
}
