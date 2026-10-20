package upgrade

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zsoftly/zs-core/internal/components"
)

func manifest() components.Manifest {
	return components.Manifest{
		CoreRelease: "1.0.0-test",
		Components: map[string]components.Component{
			components.Keycloak: {Version: "26.8.1"},
			components.KillBill: {Version: "0.24.22"},
			components.KAUI:     {Version: "4.0.31"},
			components.Postgres: {Version: "18"},
		},
	}
}

func feedServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/keycloak/keycloak/releases", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[
			{"tag_name":"26.9.0","published_at":"2026-11-10T10:00:00Z"},
			{"tag_name":"26.8.2","published_at":"2026-10-27T10:00:00Z"},
			{"tag_name":"26.8.1","published_at":"2026-10-13T10:00:00Z"},
			{"tag_name":"26.7.5","published_at":"2026-09-01T10:00:00Z"},
			{"tag_name":"27.0.0-rc1","published_at":"2026-12-01T10:00:00Z","prerelease":true},
			{"tag_name":"26.9.1","published_at":"2026-11-20T10:00:00Z","draft":true}
		]`)
	})
	mux.HandleFunc("GET /repos/killbill/killbill/releases", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[
			{"tag_name":"killbill-0.24.23","published_at":"2026-10-12T10:00:00Z"},
			{"tag_name":"killbill-0.24.22","published_at":"2026-09-29T10:00:00Z"},
			{"tag_name":"killbill-0.25.0","published_at":"2026-10-20T10:00:00Z"}
		]`)
	})
	mux.HandleFunc("GET /v2/repositories/killbill/kaui/tags", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"next":null,"results":[
			{"name":"latest","last_updated":"2026-10-01T00:00:00Z"},
			{"name":"4.0.32","last_updated":"2026-10-01T00:00:00Z"},
			{"name":"4.0.31","last_updated":"2026-09-01T00:00:00Z"},
			{"name":"4.0.30-rc1","last_updated":"2026-08-01T00:00:00Z"}
		]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestCheckFindsNewerVersionsOldestFirst(t *testing.T) {
	srv := feedServer(t)
	results := Check(context.Background(), Options{Manifest: manifest(), Client: srv.Client(), GitHubAPI: srv.URL, DockerHubAPI: srv.URL})
	if len(results) != 3 {
		t.Fatalf("results = %+v", results)
	}
	want := map[string][]string{
		components.Keycloak: {"26.8.2", "26.9.0"},
		components.KillBill: {"0.24.23", "0.25.0"},
		components.KAUI:     {"4.0.32"},
	}
	for _, r := range results {
		if r.Err != nil {
			t.Fatalf("%s: %v", r.Component, r.Err)
		}
		var got []string
		for _, n := range r.Newer {
			got = append(got, n.Version)
		}
		if strings.Join(got, ",") != strings.Join(want[r.Component], ",") {
			t.Errorf("%s newer = %v, want %v", r.Component, got, want[r.Component])
		}
	}
	if d := results[0].Newer[0].Published; !d.Equal(time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("published = %v", d)
	}
}

func TestCheckNothingNewer(t *testing.T) {
	m := manifest()
	m.Components[components.Keycloak] = components.Component{Version: "26.9.0"}
	srv := feedServer(t)
	results := Check(context.Background(), Options{Manifest: m, Client: srv.Client(), GitHubAPI: srv.URL, DockerHubAPI: srv.URL})
	if len(results[0].Newer) != 0 || results[0].Err != nil {
		t.Fatalf("keycloak result = %+v", results[0])
	}
}

func TestCheckRecordsFeedFailuresAndContinues(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/keycloak/keycloak/releases", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusForbidden)
	})
	mux.HandleFunc("GET /repos/killbill/killbill/releases", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `not json`)
	})
	mux.HandleFunc("GET /v2/repositories/killbill/kaui/tags", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"results":[{"name":"4.0.40","last_updated":"2026-10-01T00:00:00Z"}]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	results := Check(context.Background(), Options{Manifest: manifest(), Client: srv.Client(), GitHubAPI: srv.URL, DockerHubAPI: srv.URL})
	if results[0].Err == nil || !strings.Contains(results[0].Err.Error(), "HTTP 403") {
		t.Errorf("keycloak err = %v", results[0].Err)
	}
	if results[1].Err == nil || !strings.Contains(results[1].Err.Error(), "decode") {
		t.Errorf("killbill err = %v", results[1].Err)
	}
	if results[2].Err != nil || len(results[2].Newer) != 1 {
		t.Errorf("kaui result = %+v", results[2])
	}

	srv.Close()
	results = Check(context.Background(), Options{Manifest: manifest(), Client: srv.Client(), GitHubAPI: srv.URL, DockerHubAPI: srv.URL})
	for _, r := range results {
		if r.Err == nil {
			t.Errorf("%s: a closed server must be an error", r.Component)
		}
	}
}

func TestGitHubTokenIsSentOnlyToGitHub(t *testing.T) {
	var githubAuth, hubAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/", func(w http.ResponseWriter, r *http.Request) {
		githubAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, `[]`)
	})
	mux.HandleFunc("GET /v2/", func(w http.ResponseWriter, r *http.Request) {
		hubAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, `{"results":[]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	Check(context.Background(), Options{Manifest: manifest(), Client: srv.Client(), GitHubAPI: srv.URL, DockerHubAPI: srv.URL, GitHubToken: "tok"})
	if githubAuth != "Bearer tok" || hubAuth != "" {
		t.Fatalf("github=%q hub=%q", githubAuth, hubAuth)
	}
}

func TestKind(t *testing.T) {
	cases := []struct{ pinned, newer, want string }{
		{"26.8.1", "26.8.2", "patch"},
		{"26.8.1", "26.9.0", "minor"},
		{"26.8.1", "27.0.0", "major"},
		{"0.24.22", "0.25.0", "minor"},
		{"4.0.31", "4.0.32", "patch"},
		{"x", "1.0.0", "unknown"},
	}
	for _, c := range cases {
		if got := Kind(c.pinned, c.newer); got != c.want {
			t.Errorf("Kind(%s, %s) = %s, want %s", c.pinned, c.newer, got, c.want)
		}
	}
}

func TestVersionOrderingIsNumeric(t *testing.T) {
	got := newerThan([]Release{{Version: "0.24.100"}, {Version: "0.24.9"}, {Version: "0.24.22"}, {Version: "0.24.23"}}, "0.24.22")
	if len(got) != 2 || got[0].Version != "0.24.23" || got[1].Version != "0.24.100" {
		t.Fatalf("got %+v", got)
	}
}
