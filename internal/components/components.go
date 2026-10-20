// Package components reads components.yaml, the single place where the
// component versions of a Core release are pinned. See
// docs/decisions/0003-never-fork-upstream.md.
package components

import (
	"bytes"
	"errors"
	"fmt"
	"os"
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

// Plugin is a pinned Kill Bill plugin.
type Plugin struct {
	Name     string `yaml:"name"`
	Version  string `yaml:"version"`
	Artifact string `yaml:"artifact"`
	Source   string `yaml:"source"`
	Status   string `yaml:"status"`
}

// KillBillPlugins are the plugins Core knows about.
type KillBillPlugins struct {
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
	if len(problems) > 0 {
		sort.Strings(problems)
		return errors.New("invalid components file: " + strings.Join(problems, "; "))
	}
	return nil
}

// Component returns the named component.
func (m Manifest) Component(name string) (Component, bool) {
	c, ok := m.Components[name]
	return c, ok
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
