// Package kbplugin handles the artifacts of the pinned Kill Bill plugins:
// it downloads a plugin jar from the Core mirror or the Maven repository and
// checks its SHA-256 against components.yaml, extracts the plugin's DDL,
// applies that DDL to the Kill Bill database once, and keeps the plugin
// mirror Core serves to KPM. See docs/modules/plugins.md.
//
// Kill Bill does not create a plugin's own tables, and Core's service never
// touches the Kill Bill database. Applying a plugin's DDL is a deployment
// step: zs-core plugin ddl apply, with ZS_KILLBILL_DATABASE_URL.
package kbplugin

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zsoftly/zs-core/internal/components"
)

// MaxJarBytes caps the size of a plugin jar Core reads.
const MaxJarBytes = 256 << 20

// The DDL files inside a plugin jar. ddl-postgresql.sql, when present, holds
// the Postgres prelude, the domains ddl.sql relies on, and goes first.
const (
	ddlFile         = "ddl.sql"
	ddlPostgresFile = "ddl-postgresql.sql"
)

// ErrDigestMismatch means a jar does not match the sha256 of components.yaml.
var ErrDigestMismatch = errors.New("the plugin jar does not match the sha256 in components.yaml")

// Source is where Core reads plugin jars from: the mirror directory when it
// holds the jar, otherwise the Maven repository of components.yaml.
type Source struct {
	MirrorDir  string
	Repository string
	HTTP       *http.Client
}

// Jar returns the pinned jar of a plugin and where it came from, after
// checking its SHA-256 against the pin.
func (s Source) Jar(ctx context.Context, pin components.Plugin) ([]byte, string, error) {
	if !pin.HasArtifact() {
		return nil, "", fmt.Errorf("plugin %s has no Java artifact pinned in components.yaml", pin.Name)
	}
	if s.MirrorDir != "" {
		path := filepath.Join(s.MirrorDir, FileName(pin))
		if data, err := readLimited(path); err == nil {
			if err := checkDigest(data, pin); err != nil {
				return nil, "", fmt.Errorf("mirror %s: %w", path, err)
			}
			return data, "mirror " + path, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, "", fmt.Errorf("read the mirror: %w", err)
		}
	}
	uri, err := ArtifactURL(s.Repository, pin)
	if err != nil {
		return nil, "", err
	}
	data, err := download(ctx, s.HTTP, uri)
	if err != nil {
		return nil, "", err
	}
	if err := checkDigest(data, pin); err != nil {
		return nil, "", err
	}
	return data, uri, nil
}

func readLimited(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxJarBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxJarBytes {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, MaxJarBytes)
	}
	return data, nil
}

func download(ctx context.Context, client *http.Client, uri string) ([]byte, error) {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", uri, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: HTTP %d", uri, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxJarBytes+1))
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", uri, err)
	}
	if len(data) > MaxJarBytes {
		return nil, fmt.Errorf("download %s: larger than %d bytes", uri, MaxJarBytes)
	}
	return data, nil
}

func checkDigest(data []byte, pin components.Plugin) error {
	sum := sha256.Sum256(data)
	if !hmac.Equal([]byte(hex.EncodeToString(sum[:])), []byte(pin.Sha256)) {
		return fmt.Errorf("plugin %s %s: %w", pin.Name, pin.Version, ErrDigestMismatch)
	}
	return nil
}

// ArtifactURL is the address of a plugin jar in a Maven repository: the
// artifact group:name and the version of components.yaml under the
// repository base URL. The repository must be https.
func ArtifactURL(repository string, p components.Plugin) (string, error) {
	group, name, ok := strings.Cut(p.Artifact, ":")
	if !ok || group == "" || name == "" || strings.ContainsAny(p.Artifact+p.Version, "/\\ ?#") || p.Version == "" {
		return "", fmt.Errorf("plugin %s: artifact %q is not group:name", p.Name, p.Artifact)
	}
	if repository == "" {
		return "", errors.New("killbill_plugins.repository is not set in components.yaml")
	}
	if err := CheckRepository(repository); err != nil {
		return "", err
	}
	return strings.TrimRight(repository, "/") + "/" + strings.ReplaceAll(group, ".", "/") + "/" + name + "/" + p.Version + "/" + name + "-" + p.Version + ".jar", nil
}

// CheckRepository refuses a plugin repository that is not an https URL, so
// a plugin jar is never fetched in clear.
func CheckRepository(repository string) error {
	u, err := url.Parse(repository)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return errors.New("killbill_plugins.repository is not an https URL")
	}
	return nil
}

// DDL is the schema a plugin needs in the Kill Bill database: the Postgres
// prelude and ddl.sql of its jar. Text is both, as zs-core plugin ddl show
// prints them, and Digest is the SHA-256 of Text in hex.
type DDL struct {
	Plugin     string
	Version    string
	Prelude    string
	Main       string
	Text       string
	Digest     string
	Statements []Statement
}

// ExtractDDL reads the DDL of a plugin jar and parses it.
func ExtractDDL(jar []byte, pin components.Plugin) (DDL, error) {
	zr, err := zip.NewReader(bytes.NewReader(jar), int64(len(jar)))
	if err != nil {
		return DDL{}, fmt.Errorf("plugin %s: the jar is not a zip archive: %w", pin.Name, err)
	}
	read := func(name string) (string, bool, error) {
		f, err := zr.Open(name)
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		if err != nil {
			return "", false, err
		}
		defer f.Close()
		data, err := io.ReadAll(io.LimitReader(f, (4<<20)+1))
		if err == nil && len(data) > 4<<20 {
			return "", true, fmt.Errorf("%s is larger than 4 MiB", name)
		}
		return string(data), true, err
	}
	main, ok, err := read(ddlFile)
	if err != nil {
		return DDL{}, fmt.Errorf("plugin %s: read %s: %w", pin.Name, ddlFile, err)
	}
	if !ok {
		return DDL{}, fmt.Errorf("plugin %s %s has no %s", pin.Name, pin.Version, ddlFile)
	}
	prelude, _, err := read(ddlPostgresFile)
	if err != nil {
		return DDL{}, fmt.Errorf("plugin %s: read %s: %w", pin.Name, ddlPostgresFile, err)
	}
	var text strings.Builder
	if prelude != "" {
		fmt.Fprintf(&text, "-- %s %s %s\n%s\n", pin.Name, pin.Version, ddlPostgresFile, strings.TrimRight(prelude, "\n"))
	}
	fmt.Fprintf(&text, "-- %s %s %s\n%s\n", pin.Name, pin.Version, ddlFile, strings.TrimRight(main, "\n"))
	stmts, err := ParseDDL(text.String())
	if err != nil {
		return DDL{}, fmt.Errorf("plugin %s %s: %w", pin.Name, pin.Version, err)
	}
	sum := sha256.Sum256([]byte(text.String()))
	return DDL{Plugin: pin.Name, Version: pin.Version, Prelude: prelude, Main: main, Text: text.String(),
		Digest: hex.EncodeToString(sum[:]), Statements: stmts}, nil
}

// CheckTables refuses a DDL whose tables differ from the tables
// components.yaml lists for the plugin.
func CheckTables(d DDL, pin components.Plugin) error {
	got := Tables(d.Statements)
	if len(pin.Tables) == 0 {
		return fmt.Errorf("components.yaml lists no tables for plugin %s; its DDL creates %s", pin.Name, strings.Join(got, ", "))
	}
	want := map[string]bool{}
	for _, t := range pin.Tables {
		want[t] = true
	}
	same := len(got) == len(want)
	for _, t := range got {
		same = same && want[t]
	}
	if !same {
		return fmt.Errorf("plugin %s %s creates the tables %s, but components.yaml lists %s", pin.Name, pin.Version,
			strings.Join(got, ", "), strings.Join(pin.Tables, ", "))
	}
	return nil
}
