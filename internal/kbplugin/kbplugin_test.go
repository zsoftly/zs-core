package kbplugin

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zsoftly/zs-core/internal/components"
)

// testDDL has what plugin DDLs hold: a licence comment, a MySQL comment
// statement, a DROP before each CREATE, indexes, a line comment, a quoted
// default with a semicolon and a domain prelude.
const testPrelude = `/* licence */
CREATE DOMAIN datetime AS timestamp without time zone;

CREATE DOMAIN longtext AS text;
`

const testDDL = `/*
 * licence; with a semicolon
 */

/*! SET default_storage_engine=INNODB */;

drop table if exists demo_responses;
create table demo_responses (
  record_id serial
, kb_account_id char(36) not null
, note varchar(32) default 'a;b'
, created_date datetime not null
, primary key(record_id)
) /*! CHARACTER SET utf8 COLLATE utf8_bin */;
-- Not unique to handle retries
create index demo_responses_kb_account_id on demo_responses(kb_account_id);

CREATE TABLE demo_methods (
  record_id serial unique,
  additional_data longtext default null
);
CREATE UNIQUE INDEX demo_methods_record ON demo_methods(record_id);
`

func jarWith(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func pinOf(name string, jar []byte, tables ...string) components.Plugin {
	sum := sha256.Sum256(jar)
	return components.Plugin{Name: name, Version: "1.2.3", Artifact: "org.example:" + name + "-plugin", Sha256: hex.EncodeToString(sum[:]), Tables: tables}
}

func TestParseDDL(t *testing.T) {
	stmts, err := ParseDDL(testPrelude + testDDL)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, s := range stmts {
		kinds = append(kinds, s.Kind+":"+s.Object)
	}
	want := "create_domain:datetime,create_domain:longtext,drop_table:demo_responses,create_table:demo_responses,create_index:demo_responses,create_table:demo_methods,create_index:demo_methods"
	if strings.Join(kinds, ",") != want {
		t.Fatalf("statements = %v", kinds)
	}
	if got := strings.Join(Tables(stmts), ","); got != "demo_responses,demo_methods" {
		t.Fatalf("tables = %s", got)
	}
	if !strings.Contains(stmts[3].SQL, "'a;b'") {
		t.Fatalf("the quoted semicolon split the statement: %q", stmts[3].SQL)
	}
	for name, sql := range map[string]string{
		"insert":           "insert into demo values (1);",
		"function":         "create function f() returns int as $$ select 1; $$ language sql;",
		"unterminated":     "create table x (a int); /* open",
		"unterminated str": "create table x (a text default 'oops);",
	} {
		if _, err := ParseDDL(sql); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestExtractDDLAndCheckTables(t *testing.T) {
	jar := jarWith(t, map[string]string{"ddl.sql": testDDL, "ddl-postgresql.sql": testPrelude, "other.txt": "x"})
	pin := pinOf("demo", jar, "demo_responses", "demo_methods")
	d, err := ExtractDDL(jar, pin)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(d.Text, "-- demo 1.2.3 ddl-postgresql.sql\n") || !strings.Contains(d.Text, "-- demo 1.2.3 ddl.sql\n") || len(d.Digest) != 64 {
		t.Fatalf("ddl = %q %s", d.Text, d.Digest)
	}
	if err := CheckTables(d, pin); err != nil {
		t.Fatal(err)
	}
	other := pin
	other.Tables = []string{"demo_responses"}
	if err := CheckTables(d, other); err == nil || !strings.Contains(err.Error(), "components.yaml lists demo_responses") {
		t.Fatalf("other tables: %v", err)
	}
	other.Tables = nil
	if err := CheckTables(d, other); err == nil {
		t.Fatal("a pin without tables is accepted")
	}
	if _, err := ExtractDDL(jarWith(t, map[string]string{"x": "y"}), pin); err == nil {
		t.Fatal("a jar without ddl.sql is accepted")
	}
	if _, err := ExtractDDL([]byte("not a zip"), pin); err == nil {
		t.Fatal("a file that is not a zip is accepted")
	}
}

func TestExtractDDLRejectsOversizedEntries(t *testing.T) {
	const limit = 4 << 20
	for _, name := range []string{"ddl.sql", "ddl-postgresql.sql"} {
		t.Run(name, func(t *testing.T) {
			// A complete statement before the limit made a truncated entry
			// look valid even though the rest of the DDL was never checked.
			statement := "CREATE TABLE demo (id integer);\n"
			if name == "ddl-postgresql.sql" {
				statement = "CREATE DOMAIN demo_type AS integer;\n"
			}
			entry := statement + strings.Repeat(" ", limit-len(statement))
			files := map[string]string{"ddl.sql": "CREATE TABLE demo (id integer);"}
			files[name] = entry
			jar := jarWith(t, files)
			pin := pinOf("demo", jar, "demo")
			if _, err := ExtractDDL(jar, pin); err != nil {
				t.Fatalf("entry at the size limit: %v", err)
			}
			files[name] += " "
			jar = jarWith(t, files)
			pin = pinOf("demo", jar, "demo")
			d, err := ExtractDDL(jar, pin)
			if err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "larger than 4 MiB") {
				t.Fatalf("oversized entry: %v", err)
			}
			if d.Text != "" || len(d.Statements) != 0 {
				t.Fatal("oversized entry returned partial DDL")
			}
		})
	}
}

func TestSourcePrefersTheMirrorAndChecksDigests(t *testing.T) {
	jar := jarWith(t, map[string]string{"ddl.sql": testDDL})
	pin := pinOf("demo", jar, "demo_responses", "demo_methods")
	var hits atomic.Int32
	served := jar
	repo := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/maven2/org/example/demo-plugin/1.2.3/demo-plugin-1.2.3.jar" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(served)
	}))
	t.Cleanup(repo.Close)
	src := Source{Repository: repo.URL + "/maven2", HTTP: repo.Client()}
	got, from, err := src.Jar(context.Background(), pin)
	if err != nil || !bytes.Equal(got, jar) || !strings.HasPrefix(from, repo.URL) {
		t.Fatalf("repository = %v %s", err, from)
	}
	served = []byte("tampered")
	if _, _, err := src.Jar(context.Background(), pin); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("tampered jar: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName(pin)), jar, 0o600); err != nil {
		t.Fatal(err)
	}
	src.MirrorDir = dir
	before := hits.Load()
	if _, from, err := src.Jar(context.Background(), pin); err != nil || !strings.HasPrefix(from, "mirror") || hits.Load() != before {
		t.Fatalf("mirror = %v %s", err, from)
	}
	if err := os.WriteFile(filepath.Join(dir, FileName(pin)), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.Jar(context.Background(), pin); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("tampered mirror: %v", err)
	}
	if _, _, err := (Source{Repository: "http://repo.example.test"}).Jar(context.Background(), pin); err == nil {
		t.Fatal("an http repository is accepted")
	}
}

func TestSyncOpenAndTokens(t *testing.T) {
	jarA := jarWith(t, map[string]string{"ddl.sql": testDDL})
	jarB := []byte("plugin b jar")
	var hits atomic.Int32
	repo := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch {
		case strings.HasSuffix(r.URL.Path, "/a-plugin-1.2.3.jar"):
			_, _ = w.Write(jarA)
		case strings.HasSuffix(r.URL.Path, "/b-plugin-1.2.3.jar"):
			_, _ = w.Write(jarB)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(repo.Close)
	a, b := pinOf("a", jarA), pinOf("b", jarB)
	m := components.Manifest{CoreRelease: "1.0.0-test", KillBillPlugins: components.KillBillPlugins{
		Repository:             repo.URL + "/maven2",
		ApprovedPaymentPlugins: []components.Plugin{a, {Name: "none", Status: components.PluginStatusNoJavaArtifact}},
		Installable:            []components.Plugin{b},
	}}
	dir := filepath.Join(t.TempDir(), "mirror")
	secret := []byte(strings.Repeat("s", 32))
	if _, err := Sync(context.Background(), dir, nil, m, repo.Client()); err == nil {
		t.Fatal("a sync without the secret is accepted")
	}
	res, err := Sync(context.Background(), dir, secret, m, repo.Client())
	if err != nil || len(res) != 2 || !res[0].Downloaded || !res[1].Downloaded {
		t.Fatalf("sync = %+v %v", res, err)
	}
	mf, err := ReadManifest(dir)
	if err != nil || len(mf.Plugins) != 2 || mf.Plugins[0].Sha256 != a.Sha256 || mf.CoreRelease != "1.0.0-test" || mf.Verify(secret) != nil {
		t.Fatalf("manifest = %+v %v", mf, err)
	}
	if mf.Verify([]byte(strings.Repeat("x", 32))) == nil {
		t.Fatal("the manifest verifies under another secret")
	}
	before := hits.Load()
	if res, err := Sync(context.Background(), dir, secret, m, repo.Client()); err != nil || res[0].Downloaded || hits.Load() != before {
		t.Fatalf("second sync downloaded again: %+v %v", res, err)
	}
	// A pin that the repository does not match is never written.
	bad := m
	bad.KillBillPlugins.Installable = []components.Plugin{{Name: "b", Version: "1.2.3", Artifact: "org.example:b-plugin", Sha256: strings.Repeat("0", 64)}}
	if err := os.Remove(filepath.Join(dir, FileName(b))); err != nil {
		t.Fatal(err)
	}
	if _, err := Sync(context.Background(), dir, secret, bad, repo.Client()); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("sync of a mismatched jar: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, FileName(b))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a mismatched jar was kept: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".download-") {
			t.Fatalf("temporary file left behind: %s", e.Name())
		}
	}

	// The failed sync kept the signed manifest of the last good one.
	mirror := &Mirror{Dir: dir, Manifest: m, Secret: secret}
	if _, _, err := (&Mirror{Dir: dir, Manifest: m, Secret: []byte(strings.Repeat("x", 32))}).Open("a", "1.2.3"); !errors.Is(err, ErrManifestSignature) {
		t.Fatalf("open under another secret: %v", err)
	}
	f, info, err := mirror.Open("a", "1.2.3")
	if err != nil || info.Size() != int64(len(jarA)) {
		t.Fatalf("open = %v", err)
	}
	data, _ := io.ReadAll(f)
	f.Close()
	if !bytes.Equal(data, jarA) {
		t.Fatal("the mirror served other bytes")
	}
	for _, c := range [][2]string{{"a", "9.9.9"}, {"none", ""}, {"unknown", "1.2.3"}, {"b", "1.2.3"}} {
		if _, _, err := mirror.Open(c[0], c[1]); !errors.Is(err, ErrNotInMirror) {
			t.Errorf("open %v: %v", c, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, FileName(a)), []byte("tampered and longer than before"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mirror.Open("a", "1.2.3"); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("tampered jar served: %v", err)
	}

	// A jar the signed manifest does not list is never served, even when
	// components.yaml pins it and the file matches.
	if err := os.WriteFile(filepath.Join(dir, FileName(a)), jarA, 0o600); err != nil {
		t.Fatal(err)
	}
	unlisted := mf
	unlisted.Plugins = unlisted.Plugins[1:]
	unlisted.Signature, _ = unlisted.signature(secret)
	data, _ = json.Marshal(unlisted)
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mirror.Open("a", "1.2.3"); !errors.Is(err, ErrNotInMirror) {
		t.Fatalf("jar outside the manifest: %v", err)
	}
	// A manifest edited without the secret is refused.
	forged := mf
	forged.Signature = strings.Repeat("0", 64)
	data, _ = json.Marshal(forged)
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mirror.Open("a", "1.2.3"); !errors.Is(err, ErrManifestSignature) {
		t.Fatalf("forged manifest: %v", err)
	}
	tok := Token(secret, "a", "1.2.3", a.Sha256)
	for _, tc := range []struct {
		name                  string
		secret                []byte
		plugin, version, hash string
		token                 string
		want                  bool
	}{
		{"matching pin", secret, "a", "1.2.3", a.Sha256, tok, true},
		{"other version", secret, "a", "1.2.4", a.Sha256, tok, false},
		{"other plugin", secret, "b", "1.2.3", a.Sha256, tok, false},
		{"other digest", secret, "a", "1.2.3", b.Sha256, tok, false},
		{"missing digest", secret, "a", "1.2.3", "", tok, false},
		{"missing secret", nil, "a", "1.2.3", a.Sha256, tok, false},
		{"missing token", secret, "a", "1.2.3", a.Sha256, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CheckToken(tc.secret, tc.plugin, tc.version, tc.hash, tc.token); got != tc.want {
				t.Fatalf("token accepted = %t, want %t", got, tc.want)
			}
		})
	}
	u, err := MirrorURL("http://core:8080/", secret, a)
	if err != nil || u != "http://core:8080/v1/plugins/a/1.2.3.jar?sha256="+a.Sha256+"&token="+tok {
		t.Fatalf("mirror URL = %s %v", u, err)
	}
	if _, err := MirrorURL("ftp://core", secret, a); err == nil {
		t.Fatal("an ftp mirror URL is accepted")
	}
	if _, err := MirrorURL("http://core", nil, a); err == nil {
		t.Fatal("a mirror URL without a secret is accepted")
	}
	for _, digest := range []string{"", "not-a-digest", strings.Repeat("0", 63), strings.Repeat("A", 64)} {
		invalid := a
		invalid.Sha256 = digest
		if _, err := MirrorURL("http://core", secret, invalid); err == nil {
			t.Errorf("mirror URL accepted invalid digest %q", digest)
		}
	}
}

func TestMirrorURLRejectsQueryOrFragment(t *testing.T) {
	secret := []byte(strings.Repeat("s", 32))
	pin := pinOf("demo", []byte("pinned jar"))
	for _, tc := range []struct {
		name, suffix string
	}{
		{"query", "?source=repository"},
		{"empty query", "?"},
		{"fragment", "#download"},
		{"empty fragment", "#"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if uri, err := MirrorURL("http://core:8080/"+tc.suffix, secret, pin); err == nil || uri != "" {
				t.Fatal("mirror base with a query or fragment returned an authenticated URL")
			}
		})
	}
}

func TestMirrorOpenSnapshotsAndRechecksSameMetadata(t *testing.T) {
	jar := []byte("original pinned jar")
	pin := pinOf("demo", jar)
	repo := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(jar)
	}))
	t.Cleanup(repo.Close)
	m := components.Manifest{KillBillPlugins: components.KillBillPlugins{
		Repository: repo.URL, ApprovedPaymentPlugins: []components.Plugin{pin},
	}}
	dir := t.TempDir()
	secret := []byte(strings.Repeat("s", 32))
	if _, err := Sync(context.Background(), dir, secret, m, repo.Client()); err != nil {
		t.Fatal(err)
	}
	mirror := &Mirror{Dir: dir, Manifest: m, Secret: secret}
	snapshot, info, err := mirror.Open(pin.Name, pin.Version)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = snapshot.Close() })

	// Keep size and mtime unchanged to defeat metadata-only digest caches.
	tampered := bytes.Clone(jar)
	tampered[0] ^= 1
	path := filepath.Join(dir, FileName(pin))
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	changed, err := os.Stat(path)
	if err != nil || changed.Size() != info.Size() || !changed.ModTime().Equal(info.ModTime()) {
		t.Fatalf("replacement did not preserve size and mtime: %v", err)
	}
	data, err := io.ReadAll(snapshot)
	if err != nil || !bytes.Equal(data, jar) {
		t.Fatalf("opened snapshot changed after verification: %q, %v", data, err)
	}
	if _, err := snapshot.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	data, err = io.ReadAll(snapshot)
	if err != nil || !bytes.Equal(data, jar) {
		t.Fatalf("seeking the verified snapshot changed its bytes: %q, %v", data, err)
	}
	if reopened, _, err := mirror.Open(pin.Name, pin.Version); !errors.Is(err, ErrDigestMismatch) {
		if reopened != nil {
			_ = reopened.Close()
		}
		t.Fatalf("same-size, same-mtime tampering was not rejected: %v", err)
	}
}
