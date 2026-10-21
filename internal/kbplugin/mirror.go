package kbplugin

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zsoftly/zs-core/internal/components"
)

// ManifestFile is the name of the mirror's manifest in the mirror directory.
const ManifestFile = "manifest.json"

// FileName is the file of a plugin jar in the mirror: <name>-<version>.jar.
func FileName(p components.Plugin) string { return p.Name + "-" + p.Version + ".jar" }

// MirrorManifest lists what a mirror holds. sync writes it after it has
// checked every jar, and signs it: Signature is the HMAC-SHA256, in hex, of
// the manifest without its signature under the deployment's
// ZS_PLUGIN_MIRROR_SECRET. The mirror serves only a jar its signed manifest
// lists with the pinned digest.
type MirrorManifest struct {
	CoreRelease string        `json:"core_release"`
	Repository  string        `json:"repository"`
	SyncedAt    time.Time     `json:"synced_at"`
	Plugins     []MirrorEntry `json:"plugins"`
	Signature   string        `json:"signature,omitempty"`
}

// ErrManifestSignature means the mirror manifest is missing, unreadable or
// not signed with the mirror secret.
var ErrManifestSignature = errors.New("the mirror manifest is not signed with ZS_PLUGIN_MIRROR_SECRET")

// signature is the HMAC of the manifest without its signature.
func (m MirrorManifest) signature(secret []byte) (string, error) {
	m.Signature = ""
	data, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("zs-core.plugin-mirror.manifest.v1:"))
	mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// Verify checks the manifest's signature.
func (m MirrorManifest) Verify(secret []byte) error {
	if len(secret) == 0 || m.Signature == "" {
		return ErrManifestSignature
	}
	want, err := m.signature(secret)
	if err != nil || !hmac.Equal([]byte(want), []byte(m.Signature)) {
		return ErrManifestSignature
	}
	return nil
}

// entry returns the manifest's entry of a plugin version.
func (m MirrorManifest) entry(name, version string) (MirrorEntry, bool) {
	for _, e := range m.Plugins {
		if e.Name == name && e.Version == version {
			return e, true
		}
	}
	return MirrorEntry{}, false
}

// MirrorEntry is one jar of the mirror.
type MirrorEntry struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Artifact string `json:"artifact"`
	File     string `json:"file"`
	Sha256   string `json:"sha256"`
	Size     int64  `json:"size"`
	Source   string `json:"source"`
}

// SyncResult is one plugin of a sync.
type SyncResult struct {
	Entry      MirrorEntry
	Downloaded bool
}

// Sync makes the mirror directory hold every pinned jar of m, checked
// against its sha256, and writes the manifest. A jar already in the mirror
// with the right digest is kept. A jar is downloaded to a temporary file and
// renamed into place only after its digest matches.
func Sync(ctx context.Context, dir string, secret []byte, m components.Manifest, client *http.Client) ([]SyncResult, error) {
	if dir == "" {
		return nil, errors.New("ZS_PLUGIN_MIRROR_DIR is not set")
	}
	if len(secret) == 0 {
		return nil, errors.New("ZS_PLUGIN_MIRROR_SECRET is not set: sync signs the mirror manifest with it")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create the mirror directory: %w", err)
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}
	var out []SyncResult
	manifest := MirrorManifest{CoreRelease: m.CoreRelease, Repository: m.KillBillPlugins.Repository, Plugins: []MirrorEntry{}}
	for _, pin := range m.Artifacts() {
		uri, err := ArtifactURL(m.KillBillPlugins.Repository, pin)
		if err != nil {
			return out, err
		}
		path := filepath.Join(dir, FileName(pin))
		entry := MirrorEntry{Name: pin.Name, Version: pin.Version, Artifact: pin.Artifact, File: FileName(pin), Sha256: pin.Sha256, Source: uri}
		downloaded := false
		size, err := fileDigestMatches(path, pin.Sha256)
		if err != nil {
			if size, err = fetchTo(ctx, client, uri, path, pin); err != nil {
				return out, err
			}
			downloaded = true
		}
		entry.Size = size
		manifest.Plugins = append(manifest.Plugins, entry)
		out = append(out, SyncResult{Entry: entry, Downloaded: downloaded})
	}
	manifest.SyncedAt = time.Now().UTC().Truncate(time.Second)
	sig, err := manifest.signature(secret)
	if err != nil {
		return out, err
	}
	manifest.Signature = sig
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return out, err
	}
	if err := writeAtomic(filepath.Join(dir, ManifestFile), append(data, '\n')); err != nil {
		return out, fmt.Errorf("write the mirror manifest: %w", err)
	}
	return out, nil
}

// fileDigestMatches returns the size of the file when its SHA-256 is want.
func fileDigestMatches(path, want string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, MaxJarBytes+1))
	if err != nil {
		return 0, err
	}
	if n > MaxJarBytes || !hmac.Equal([]byte(hex.EncodeToString(h.Sum(nil))), []byte(want)) {
		return 0, ErrDigestMismatch
	}
	return n, nil
}

func fetchTo(ctx context.Context, client *http.Client, uri, path string, pin components.Plugin) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("download %s: %w", uri, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("download %s: HTTP %d", uri, resp.StatusCode)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".download-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, MaxJarBytes+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, fmt.Errorf("download %s: %w", uri, err)
	}
	if n > MaxJarBytes {
		return 0, fmt.Errorf("download %s: larger than %d bytes", uri, MaxJarBytes)
	}
	if !hmac.Equal([]byte(hex.EncodeToString(h.Sum(nil))), []byte(pin.Sha256)) {
		return 0, fmt.Errorf("plugin %s %s from %s: %w", pin.Name, pin.Version, uri, ErrDigestMismatch)
	}
	if err := os.Chmod(tmp.Name(), 0o640); err != nil {
		return 0, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return 0, err
	}
	return n, nil
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".manifest-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o640); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// ReadManifest reads the mirror's manifest.
func ReadManifest(dir string) (MirrorManifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	if err != nil {
		return MirrorManifest{}, err
	}
	var m MirrorManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return MirrorManifest{}, fmt.Errorf("the mirror manifest is not valid JSON: %w", err)
	}
	return m, nil
}

// Status is the state of one pinned jar in the mirror.
type Status struct {
	Plugin components.Plugin
	// Present is false when the mirror lacks the jar.
	Present bool
	// Match is true when the jar's SHA-256 equals the pin.
	Match bool
}

// Check reports, for every pinned jar, whether the mirror holds it with the
// pinned digest. It hashes every jar.
func Check(dir string, m components.Manifest) []Status {
	var out []Status
	for _, pin := range m.Artifacts() {
		st := Status{Plugin: pin}
		_, err := fileDigestMatches(filepath.Join(dir, FileName(pin)), pin.Sha256)
		switch {
		case err == nil:
			st.Present, st.Match = true, true
		case errors.Is(err, ErrDigestMismatch):
			st.Present = true
		}
		out = append(out, st)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Plugin.Name < out[j].Plugin.Name })
	return out
}

// Token is the token of one jar in a mirror URL: the HMAC-SHA256, in hex,
// of the plugin, version and digest under ZS_PLUGIN_MIRROR_SECRET.
// KPM sends no headers, so the token travels in the query. It opens one
// jar only and never reveals the secret.
func Token(secret []byte, name, version, digest string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("zs-core.plugin-mirror.v2:"))
	mac.Write([]byte(name))
	mac.Write([]byte{0})
	mac.Write([]byte(version))
	mac.Write([]byte{0})
	mac.Write([]byte(digest))
	return hex.EncodeToString(mac.Sum(nil))
}

// JarPath is the path of a jar on Core's mirror route.
func JarPath(p components.Plugin) string {
	return "/v1/plugins/" + url.PathEscape(p.Name) + "/" + url.PathEscape(p.Version) + ".jar"
}

// MirrorURL is the address KPM downloads a jar from on Core's mirror: the
// mirror base URL, the jar path, its expected digest and a token bound to it.
func MirrorURL(base string, secret []byte, p components.Plugin) (string, error) {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(base, "#") {
		return "", errors.New("ZS_PLUGIN_MIRROR_URL is not an http or https URL")
	}
	if len(secret) == 0 {
		return "", errors.New("ZS_PLUGIN_MIRROR_SECRET is not set")
	}
	if digest, err := hex.DecodeString(p.Sha256); err != nil || len(digest) != sha256.Size || hex.EncodeToString(digest) != p.Sha256 {
		return "", errors.New("the plugin has no valid sha256 pin")
	}
	q := url.Values{"sha256": {p.Sha256}, "token": {Token(secret, p.Name, p.Version, p.Sha256)}}
	return trimSlash(base) + JarPath(p) + "?" + q.Encode(), nil
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// Mirror serves the jars of a mirror directory. A jar is served only when
// components.yaml pins its plugin and version, the mirror manifest signed
// with Secret lists it with the pinned digest, and the file's SHA-256
// matches. Every Open verifies a fresh snapshot, and the caller serves
// those exact bytes even if the file changes afterwards.
type Mirror struct {
	Dir      string
	Manifest components.Manifest
	Secret   []byte
}

// ErrNotInMirror means the mirror does not serve the requested jar.
var ErrNotInMirror = errors.New("the jar is not in the mirror")

// Open returns a snapshot of a pinned plugin jar, checked against its pin.
// A plugin or version that is not pinned, or a jar the mirror lacks, is
// ErrNotInMirror. A jar whose digest differs is ErrDigestMismatch.
func (m *Mirror) Open(name, version string) (io.ReadSeekCloser, os.FileInfo, error) {
	pin, ok := m.Manifest.PinnedPlugin(name)
	if !ok || !pin.HasArtifact() || pin.Version != version {
		return nil, nil, ErrNotInMirror
	}
	signed, err := ReadManifest(m.Dir)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrManifestSignature, err)
	}
	if err := signed.Verify(m.Secret); err != nil {
		return nil, nil, err
	}
	entry, ok := signed.entry(name, version)
	if !ok || entry.File != FileName(pin) || !hmac.Equal([]byte(entry.Sha256), []byte(pin.Sha256)) {
		return nil, nil, ErrNotInMirror
	}
	path := filepath.Join(m.Dir, FileName(pin))
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, ErrNotInMirror
	}
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if info.Size() > MaxJarBytes {
		return nil, nil, fmt.Errorf("mirror jar %s: %w", FileName(pin), ErrDigestMismatch)
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxJarBytes+1))
	if err != nil {
		return nil, nil, err
	}
	if len(data) > MaxJarBytes || int64(len(data)) != info.Size() {
		return nil, nil, fmt.Errorf("mirror jar %s: %w", FileName(pin), ErrDigestMismatch)
	}
	if err := checkDigest(data, pin); err != nil {
		return nil, nil, err
	}
	return mirrorSnapshot{Reader: bytes.NewReader(data)}, info, nil
}

type mirrorSnapshot struct{ *bytes.Reader }

func (mirrorSnapshot) Close() error { return nil }

// CheckToken reports whether token opens the named version with this digest.
func CheckToken(secret []byte, name, version, digest, token string) bool {
	if len(secret) == 0 || digest == "" || token == "" {
		return false
	}
	return hmac.Equal([]byte(Token(secret, name, version, digest)), []byte(token))
}
