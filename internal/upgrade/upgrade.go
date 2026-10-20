// Package upgrade compares the versions pinned in components.yaml with the
// upstream release feeds. It only reads: it never changes a pin, a file or a
// running stack.
package upgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zsoftly/zs-core/internal/components"
)

// Default feed locations.
const (
	DefaultGitHubAPI    = "https://api.github.com"
	DefaultDockerHubAPI = "https://hub.docker.com"
)

// Release is one upstream version.
type Release struct {
	Version   string    `json:"version"`
	Published time.Time `json:"published"`
}

// Result is the outcome for one component.
type Result struct {
	Component string    `json:"component"`
	Pinned    string    `json:"pinned"`
	Source    string    `json:"source"`
	Newer     []Release `json:"newer"`
	// Err is set when the feed could not be read.
	Err error `json:"-"`
}

// Options configure a check.
type Options struct {
	Manifest     components.Manifest
	Client       *http.Client
	GitHubAPI    string
	DockerHubAPI string
	// GitHubToken is optional and only raises the GitHub rate limit.
	GitHubToken string
}

type feed struct {
	component string
	describe  string
	fetch     func(ctx context.Context, o Options) ([]Release, error)
}

// Check queries the feeds for keycloak, killbill and kaui and returns one
// result per component, in that order. A feed failure is recorded in the
// result and does not stop the other checks.
func Check(ctx context.Context, o Options) []Result {
	if o.Client == nil {
		o.Client = &http.Client{Timeout: 20 * time.Second}
	}
	if o.GitHubAPI == "" {
		o.GitHubAPI = DefaultGitHubAPI
	}
	if o.DockerHubAPI == "" {
		o.DockerHubAPI = DefaultDockerHubAPI
	}
	feeds := []feed{
		{components.Keycloak, "github.com/keycloak/keycloak releases", func(ctx context.Context, o Options) ([]Release, error) {
			return githubReleases(ctx, o, "keycloak/keycloak", "")
		}},
		{components.KillBill, "github.com/killbill/killbill releases", func(ctx context.Context, o Options) ([]Release, error) {
			return githubReleases(ctx, o, "killbill/killbill", "killbill-")
		}},
		{components.KAUI, "hub.docker.com killbill/kaui tags", func(ctx context.Context, o Options) ([]Release, error) {
			return dockerHubTags(ctx, o, "killbill/kaui")
		}},
	}
	results := make([]Result, 0, len(feeds))
	for _, f := range feeds {
		pin, _ := o.Manifest.Component(f.component)
		r := Result{Component: f.component, Pinned: pin.Version, Source: f.describe}
		releases, err := f.fetch(ctx, o)
		if err != nil {
			r.Err = fmt.Errorf("%s: %w", f.describe, err)
		} else {
			r.Newer = newerThan(releases, pin.Version)
		}
		results = append(results, r)
	}
	return results
}

var versionPattern = regexp.MustCompile(`^\d+(\.\d+){1,3}$`)

// parseVersion splits a dotted numeric version. It returns false for anything
// else, including release candidates.
func parseVersion(v string) ([]int, bool) {
	if !versionPattern.MatchString(v) {
		return nil, false
	}
	parts := strings.Split(v, ".")
	out := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, false
		}
		out[i] = n
	}
	return out, true
}

func compare(a, b []int) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// newerThan returns the releases above pinned, oldest first, without
// duplicates.
func newerThan(releases []Release, pinned string) []Release {
	pin, ok := parseVersion(pinned)
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	var out []Release
	for _, r := range releases {
		v, ok := parseVersion(r.Version)
		if !ok || compare(v, pin) <= 0 || seen[r.Version] {
			continue
		}
		seen[r.Version] = true
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		a, _ := parseVersion(out[i].Version)
		b, _ := parseVersion(out[j].Version)
		return compare(a, b) < 0
	})
	return out
}

// Kind classifies the step from the pin to a newer version.
func Kind(pinned, newer string) string {
	p, ok1 := parseVersion(pinned)
	n, ok2 := parseVersion(newer)
	if !ok1 || !ok2 {
		return "unknown"
	}
	at := func(v []int, i int) int {
		if i < len(v) {
			return v[i]
		}
		return 0
	}
	switch {
	case at(n, 0) != at(p, 0):
		return "major"
	case at(n, 1) != at(p, 1):
		return "minor"
	default:
		return "patch"
	}
}

func get(ctx context.Context, o Options, rawURL, token string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "zs-core-upgrade-check")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := o.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func githubReleases(ctx context.Context, o Options, repo, tagPrefix string) ([]Release, error) {
	var payload []struct {
		TagName     string    `json:"tag_name"`
		PublishedAt time.Time `json:"published_at"`
		Draft       bool      `json:"draft"`
		Prerelease  bool      `json:"prerelease"`
	}
	u := strings.TrimRight(o.GitHubAPI, "/") + "/repos/" + repo + "/releases?per_page=100"
	if err := get(ctx, o, u, o.GitHubToken, &payload); err != nil {
		return nil, err
	}
	var out []Release
	for _, r := range payload {
		if r.Draft || r.Prerelease {
			continue
		}
		out = append(out, Release{Version: strings.TrimPrefix(r.TagName, tagPrefix), Published: r.PublishedAt})
	}
	return out, nil
}

func dockerHubTags(ctx context.Context, o Options, repo string) ([]Release, error) {
	var out []Release
	next := strings.TrimRight(o.DockerHubAPI, "/") + "/v2/repositories/" + repo + "/tags?page_size=100&ordering=last_updated"
	// Three pages of 100 cover far more than the tags that can be newer than a pin.
	for page := 0; page < 3 && next != ""; page++ {
		var payload struct {
			Next    string `json:"next"`
			Results []struct {
				Name        string    `json:"name"`
				LastUpdated time.Time `json:"last_updated"`
			} `json:"results"`
		}
		if err := get(ctx, o, next, "", &payload); err != nil {
			return nil, err
		}
		for _, t := range payload.Results {
			out = append(out, Release{Version: t.Name, Published: t.LastUpdated})
		}
		next = ""
		if payload.Next != "" {
			if _, err := url.Parse(payload.Next); err == nil {
				next = payload.Next
			}
		}
	}
	return out, nil
}
