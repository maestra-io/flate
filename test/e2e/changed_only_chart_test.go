package e2e

import (
	"maps"
	"path/filepath"
	"strings"
	"testing"

	"github.com/home-operations/flate/internal/testutil"
)

const (
	chartKustomization = `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: %NAME%
  namespace: flux-system
spec:
  interval: 10m
  path: ./%NAME%
  prune: true
  sourceRef:
    kind: GitRepository
    name: flux-system
`
	chartHelmRelease = `apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: app
  namespace: apps
spec:
  interval: 10m
  chart:
    spec:
      chart: %CHART%
      sourceRef:
        kind: GitRepository
        name: %SOURCE%
        namespace: flux-system
`
)

// chartFixture is a git repository with a HelmRelease reading a chart from source,
// and an unrelated ConfigMap in another Kustomization.
func chartFixture(t *testing.T, chart, source string, extra map[string]string) string {
	t.Helper()
	root := t.TempDir()
	repo := gitInit(t, root)
	files := map[string]string{
		"cluster/apps.yaml":            strings.ReplaceAll(chartKustomization, "%NAME%", "apps"),
		"cluster/other.yaml":           strings.ReplaceAll(chartKustomization, "%NAME%", "other"),
		"apps/kustomization.yaml":      "resources:\n  - hr.yaml\n",
		"apps/hr.yaml":                 strings.NewReplacer("%CHART%", chart, "%SOURCE%", source).Replace(chartHelmRelease),
		"other/kustomization.yaml":     "resources:\n  - cm.yaml\n",
		"other/cm.yaml":                "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: other\n  namespace: apps\ndata:\n  value: before\n",
		"charts/app/Chart.yaml":        "apiVersion: v2\nname: app\nversion: 1.0.0\n",
		"charts/app/templates/cm.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: from-chart\ndata:\n  value: before\n",
	}
	maps.Copy(files, extra)
	for path, text := range files {
		testutil.WriteFile(t, root, path, text)
	}
	gitCommitAll(t, repo)
	return root
}

// TestE2E_ChangedOnlyLocalChart: an edited template of a chart read from the
// repository itself reaches the changed-only render, which is what a validator reads.
func TestE2E_ChangedOnlyLocalChart(t *testing.T) {
	root := chartFixture(t, "./charts/app", "flux-system", nil)
	mutateFile(t, filepath.Join(root, "charts/app/templates/cm.yaml"), "value: before", "value: after")

	out := runCLIStdout(t, "build", "all", "--path", filepath.Join(root, "cluster"), "--base", "HEAD")
	if !strings.Contains(out, "value: after") {
		t.Fatalf("the edited chart template is missing from the render:\n%s", out)
	}
	if strings.Contains(out, "name: other") {
		t.Errorf("an unrelated Kustomization joined the render:\n%s", out)
	}
}

// TestE2E_ChangedOnlyRemoteChartIsNotLocal: a chart path of another repository names
// nothing in the change set. An unrelated change must not select the HelmRelease: with
// `chart: ./` that used to fetch the remote, and fail a commit that never touched it.
func TestE2E_ChangedOnlyRemoteChartIsNotLocal(t *testing.T) {
	remote := map[string]string{"cluster/remote.yaml": `apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: remote
  namespace: flux-system
spec:
  interval: 10m
  url: https://fixture.invalid/remote.git
  ref:
    branch: main
`}
	for name, chart := range map[string]string{"root": "./", "same_path_as_local": "./charts/app"} {
		t.Run(name, func(t *testing.T) {
			root := chartFixture(t, chart, "remote", remote)
			changed := "other/cm.yaml"
			if name == "same_path_as_local" {
				changed = "charts/app/templates/cm.yaml"
			}
			mutateFile(t, filepath.Join(root, changed), "value: before", "value: after")

			stdout, stderr, code := runCLIBuffers("build", "all", "--path", filepath.Join(root, "cluster"), "--base", "HEAD")
			if code != 0 {
				t.Fatalf("an unrelated change must not read the remote: exit %d\n%s", code, stderr)
			}
			if strings.Contains(stdout, "kind: HelmRelease") {
				t.Errorf("the remote chart's HelmRelease joined the render:\n%s", stdout)
			}
		})
	}
}
