package helmchart

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/home-operations/flate/pkg/manifest"
	"github.com/home-operations/flate/pkg/store"
)

func writeRewrite(t *testing.T, rewrite, registryConfig string) *Rewrite {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "rewrite.yaml")
	if err := os.WriteFile(path, []byte(rewrite), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := ""
	if registryConfig != "" {
		cfg = filepath.Join(dir, "config.json")
		if err := os.WriteFile(cfg, []byte(registryConfig), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rw, err := LoadRewrite(path, cfg)
	if err != nil {
		t.Fatalf("LoadRewrite: %v", err)
	}
	return rw
}

func TestRewrite_Apply(t *testing.T) {
	rw := writeRewrite(t, `
http:
  https://charts.example.org: https://mirror.example/helm/example
oci:
  ghcr.io: oci://mirror.example/ghcr
`, "")
	for _, tc := range []struct {
		name, url, typ, provider, want string
	}{
		{name: "http exact", url: "https://charts.example.org", want: "https://mirror.example/helm/example"},
		{name: "http trailing slash", url: "https://charts.example.org/", want: "https://mirror.example/helm/example"},
		{name: "http other path stays", url: "https://charts.example.org/other", want: "https://charts.example.org/other"},
		{name: "http unmapped stays", url: "https://other.example", want: "https://other.example"},
		{name: "oci host", url: "oci://ghcr.io/org/charts", typ: manifest.RepoTypeOCI, want: "oci://mirror.example/ghcr/org/charts"},
		{name: "oci no path stays", url: "oci://ghcr.io", typ: manifest.RepoTypeOCI, want: "oci://ghcr.io"},
		{name: "oci cloud provider stays", url: "oci://ghcr.io/org/charts", typ: manifest.RepoTypeOCI, provider: "aws", want: "oci://ghcr.io/org/charts"},
		{name: "oci unmapped host stays", url: "oci://quay.io/org/charts", typ: manifest.RepoTypeOCI, want: "oci://quay.io/org/charts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &manifest.HelmRepository{Name: "r", Namespace: "ns"}
			r.URL, r.Type, r.Provider = tc.url, tc.typ, tc.provider
			r.SecretRef = &manifest.LocalObjectReference{Name: "upstream-creds"}
			got := rw.apply(r)
			if got.URL != tc.want {
				t.Errorf("URL = %q, want %q", got.URL, tc.want)
			}
			if rewritten := got != r; rewritten != (got.SecretRef == nil) {
				t.Errorf("rewritten = %v but SecretRef = %v: a rewritten repository drops its secretRef, any other keeps it", rewritten, got.SecretRef)
			}
			if r.URL != tc.url {
				t.Errorf("input mutated: URL = %q", r.URL)
			}
		})
	}
}

func TestLoadRewrite_Invalid(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"http target not a URL": "http:\n  https://a.example: mirror.example/helm\n",
		"oci target not oci":    "oci:\n  ghcr.io: https://mirror.example\n",
		"unknown key":           "https:\n  a: b\n",
	} {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-"))
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadRewrite(path, ""); err == nil {
			t.Errorf("%s: LoadRewrite accepted %q", name, body)
		}
	}
}

// The upstream is never contacted: index and chart come from the mirror,
// with the mirror host's --registry-config credential, not the upstream
// repository's secretRef.
func TestFetch_HTTPRewriteUsesMirrorAndRegistryCredential(t *testing.T) {
	chartBytes := buildChartTarGz(t, "app-template", "1.0.0")
	var upstreamHits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(upstream.Close)
	mux := http.NewServeMux()
	mux.HandleFunc("/helm/example/", func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "oidc" || p != "jwt" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch strings.TrimPrefix(r.URL.Path, "/helm/example/") {
		case "index.yaml":
			_, _ = w.Write([]byte(helmRepoIndex(chartDigest(chartBytes))))
		case "app-template-1.0.0.tgz":
			_, _ = w.Write(chartBytes)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	mirror := httptest.NewServer(mux)
	t.Cleanup(mirror.Close)

	host := strings.TrimPrefix(mirror.URL, "http://")
	rw := writeRewrite(t,
		fmt.Sprintf("http:\n  %s: %s/helm/example\n", upstream.URL, mirror.URL),
		fmt.Sprintf(`{"auths":{%q:{"auth":%q}}}`, host, base64.StdEncoding.EncodeToString([]byte("oidc:jwt"))))
	repo := httpRepo(upstream.URL)
	repo.SecretRef = &manifest.LocalObjectReference{Name: "absent"}
	f := newHTTPFetcher(t, repo)
	f.rewrite = rw

	art, err := f.Fetch(context.Background(), helmChart("repo", "app-template", "1.0.0"))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if want := mirror.URL + "/helm/example/app-template-1.0.0.tgz"; art.URL != want {
		t.Errorf("chart URL = %q, want %q", art.URL, want)
	}
	if upstreamHits != 0 {
		t.Errorf("upstream contacted %d times, want 0", upstreamHits)
	}
}

func TestFetch_OCIRewrite(t *testing.T) {
	rw := writeRewrite(t, "oci:\n  ghcr.io: oci://mirror.example/ghcr\n", "")
	repo := ociRepo("oci://ghcr.io/org/charts")
	repo.SecretRef = &manifest.LocalObjectReference{Name: "ghcr-creds"}
	stub := &stubOCI{art: &store.SourceArtifact{Kind: manifest.KindOCIRepository}}
	f := &Fetcher{repos: func(_, _ string) *manifest.HelmRepository { return repo }, oci: stub, rewrite: rw}

	if _, err := f.Fetch(context.Background(), helmChart("truecharts", "kromgo", "3.0.0")); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if stub.got.URL != "oci://mirror.example/ghcr/org/charts/kromgo" {
		t.Errorf("synthesized OCIRepository URL = %q", stub.got.URL)
	}
	if stub.got.SecretRef != nil {
		t.Errorf("SecretRef = %v, want nil (the mirror authenticates via --registry-config)", stub.got.SecretRef)
	}
}

// A redirect off the mirror host (net/http would forward Authorization to
// another port of the same hostname) never carries the mirror credential.
func TestFetch_RewriteCredentialStaysOnMirrorHost(t *testing.T) {
	chartBytes := buildChartTarGz(t, "app-template", "1.0.0")
	var leaked bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked = true
		}
		_, _ = w.Write([]byte(helmRepoIndex(chartDigest(chartBytes))))
	}))
	t.Cleanup(other.Close)
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(mirror.Close)

	host := strings.TrimPrefix(mirror.URL, "http://")
	rw := writeRewrite(t,
		fmt.Sprintf("http:\n  https://charts.example.org: %s/helm/example\n", mirror.URL),
		fmt.Sprintf(`{"auths":{%q:{"auth":%q}}}`, host, base64.StdEncoding.EncodeToString([]byte("oidc:jwt"))))
	repo := httpRepo("https://charts.example.org")
	repo.PassCredentials = true
	f := newHTTPFetcher(t, repo)
	f.rewrite = rw

	_, _ = f.Fetch(context.Background(), helmChart("repo", "app-template", "1.0.0"))
	if leaked {
		t.Error("mirror credential sent to a redirect target off the mirror host")
	}
}
