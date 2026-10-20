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
)

// Component is one pinned upstream component.
type Component struct {
	Version  string `yaml:"version"`
	Image    string `yaml:"image"`
	Digest   string `yaml:"digest"`
	Released string `yaml:"released"`
	Source   string `yaml:"source"`
}

// Plugin is a pinned Kill Bill plugin. Sha256 is the SHA-256 of the jar in
// the repository, in lowercase hex. Core checks it before it asks KPM to
// install the jar.
type Plugin struct {
	Name     string `yaml:"name"`
	Version  string `yaml:"version"`
	Artifact string `yaml:"artifact"`
	Sha256   string `yaml:"sha256"`
	Source   string `yaml:"source"`
	Status   string `yaml:"status"`
}

// KillBillPlugins are the plugins Core knows about. Repository is the Maven
// repository that KPM downloads plugin jars from.
type KillBillPlugins struct {
	Repository             string   `yaml:"repository"`
	ApprovedPaymentPlugins []Plugin `yaml:"approved_payment_plugins"`
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
	for _, list := range []struct {
		path    string
		plugins []Plugin
	}{{"killbill_plugins.approved_payment_plugins", m.KillBillPlugins.ApprovedPaymentPlugins}, {"killbill_plugins.installable", m.KillBillPlugins.Installable}} {
		for _, p := range list.plugins {
			switch {
			case p.Artifact == "" && p.Sha256 != "":
				problems = append(problems, fmt.Sprintf("%s %s has a sha256 but no artifact", list.path, p.Name))
			case p.Artifact != "" && !sha256Pattern.MatchString(p.Sha256):
				problems = append(problems, fmt.Sprintf("%s %s.sha256 %q is not 64 lowercase hex", list.path, p.Name, p.Sha256))
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

// Plugin statuses in components.yaml that mean the plugin is not expected on
// a running stack.
const (
	PluginStatusNoJavaArtifact     = "no_java_artifact"
	PluginStatusPinnedNotInstalled = "pinned_not_installed"
)

// Required reports whether a running stack must have the plugin installed and
// running: every pinned plugin except those marked no_java_artifact or
// pinned_not_installed.
func (p Plugin) Required() bool {
	return p.Status != PluginStatusNoJavaArtifact && p.Status != PluginStatusPinnedNotInstalled
}

// RequiredPlugins returns the approved payment plugins and installable
// plugins that a running stack must have.
func (m Manifest) RequiredPlugins() []Plugin {
	var required []Plugin
	for _, list := range [][]Plugin{m.KillBillPlugins.ApprovedPaymentPlugins, m.KillBillPlugins.Installable} {
		for _, p := range list {
			if p.Required() {
				required = append(required, p)
			}
		}
	}
	return required
}

// PinnedPlugin returns the pinned version of a Kill Bill plugin by name,
// searching the approved payment plugins and the installable plugins.
func (m Manifest) PinnedPlugin(name string) (Plugin, bool) {
	for _, list := range [][]Plugin{m.KillBillPlugins.ApprovedPaymentPlugins, m.KillBillPlugins.Installable} {
		for _, p := range list {
			if p.Name == name {
				return p, true
			}
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
