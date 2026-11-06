package helmchart

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"oras.land/oras-go/v2/registry/remote/credentials"
	"sigs.k8s.io/yaml"

	"github.com/home-operations/flate/pkg/manifest"
)

// Rewrite redirects HelmRepository chart fetches to a mirror, as an admission
// policy that rewrites spec.url (a Kyverno mutation) does in the cluster. It
// applies at fetch time only: rendered HelmRepository objects, synthetic
// HelmChart names and change detection keep the spec.url from the inputs.
// A rewritten repository drops its spec.secretRef and authenticates with the
// --registry-config credential of the mirror host (credHelpers included), so
// short-lived tokens can come from a credential helper.
type Rewrite struct {
	// HTTP maps the spec.url of a non-OCI HelmRepository, trailing slash
	// trimmed, to the index URL fetched instead.
	HTTP map[string]string `json:"http"`
	// OCI maps the registry host of a generic-provider OCI HelmRepository to
	// the oci:// base its repository path is appended to.
	OCI map[string]string `json:"oci"`

	creds credentials.Store
	hosts map[string]bool // hosts of the HTTP targets, the only ones given credentials
}

// LoadRewrite reads a Rewrite (YAML or JSON) from path; an empty path is no
// rewrite. registryConfig is the --registry-config path (empty = docker's
// default config).
func LoadRewrite(path, registryConfig string) (*Rewrite, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path) //nolint:gosec // operator-supplied CLI flag, like --substitute-from
	if err != nil {
		return nil, fmt.Errorf("%w: helm repository rewrite: %w", manifest.ErrInput, err)
	}
	rw := &Rewrite{hosts: map[string]bool{}}
	if err := yaml.UnmarshalStrict(data, rw); err != nil {
		return nil, fmt.Errorf("%w: helm repository rewrite %s: %w", manifest.ErrInput, path, err)
	}
	for from, to := range rw.HTTP {
		u, err := url.Parse(to)
		if err != nil || u.Scheme != "https" && u.Scheme != "http" || u.Host == "" {
			return nil, fmt.Errorf("%w: helm repository rewrite %s: %q -> %q: target is not an http(s) URL", manifest.ErrInput, path, from, to)
		}
		rw.hosts[u.Host] = true
	}
	for host, to := range rw.OCI {
		if !strings.HasPrefix(to, "oci://") {
			return nil, fmt.Errorf("%w: helm repository rewrite %s: %q -> %q: target is not an oci:// base", manifest.ErrInput, path, host, to)
		}
	}
	if registryConfig != "" {
		rw.creds, err = credentials.NewStore(registryConfig, credentials.StoreOptions{})
	} else {
		rw.creds, err = credentials.NewStoreFromDocker(credentials.StoreOptions{})
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: helm repository rewrite: load credentials: %w", manifest.ErrInput, err)
	}
	return rw, nil
}

// apply returns r redirected to its mirror, or r itself when no entry matches.
// Matching mirrors the cluster policy: HTTP by the whole URL, OCI by the
// registry host of a generic-provider repository with a repository path.
func (rw *Rewrite) apply(r *manifest.HelmRepository) *manifest.HelmRepository {
	if rw == nil {
		return r
	}
	var to string
	if isOCIHelmRepo(r) {
		host, path, _ := strings.Cut(strings.TrimPrefix(strings.Trim(r.URL, "/"), "oci://"), "/")
		if base, ok := rw.OCI[host]; ok && path != "" && (r.Provider == "" || r.Provider == "generic") {
			to = strings.TrimSuffix(base, "/") + "/" + path
		}
	} else {
		to = rw.HTTP[strings.TrimRight(r.URL, "/")]
	}
	if to == "" {
		return r
	}
	cp := *r
	cp.URL = to
	cp.SecretRef = nil
	cp.PassCredentials = false // the mirror credential never leaves the mirror host
	return &cp
}

// credentials returns the basic-auth pair for an HTTP repository without a
// secretRef: the --registry-config credential of its host when that host is
// a rewrite target, else none (anonymous, as Flux fetches it).
func (rw *Rewrite) credentials(ctx context.Context, repoURL string) (string, string, error) {
	if rw == nil || rw.creds == nil {
		return "", "", nil
	}
	u, err := url.Parse(repoURL)
	if err != nil || !rw.hosts[u.Host] {
		return "", "", nil
	}
	cred, err := rw.creds.Get(ctx, u.Host)
	if err != nil {
		return "", "", fmt.Errorf("helm repository rewrite: credentials for %s: %w", u.Host, err)
	}
	return cred.Username, cred.Password, nil
}
