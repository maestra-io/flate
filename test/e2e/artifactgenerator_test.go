package e2e

import (
	"fmt"
	"maps"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/home-operations/flate/internal/testutil"
)

// generatorLeaves are the pilot shape: leaf Kustomizations sourced from one
// ArtifactGenerator over the bootstrap GitRepository.
var generatorLeaves = []string{"alpha", "bravo", "charlie", "delta", "echo"}

const leafKustomization = `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: %NAME%
  namespace: flux-system
spec:
  interval: 10m
  path: ./apps/%NAME%
  prune: true
  sourceRef:
    kind: %KIND%
    name: %SOURCE%
`

// generatorFixture writes a bootstrap path holding an ArtifactGenerator that
// copies every leaf directory (minus its oci-source.yaml) and one
// Kustomization per leaf sourced from kind (GitRepository or
// ExternalArtifact), commits it, and returns the repository root.
func generatorFixture(t *testing.T, kind string, extra map[string]string) string {
	t.Helper()
	root := t.TempDir()
	repo := gitInit(t, root)
	source := map[string]string{"GitRepository": "flux-system", "ExternalArtifact": "platform"}[kind]
	var generator strings.Builder
	generator.WriteString(`apiVersion: source.extensions.fluxcd.io/v1beta1
kind: ArtifactGenerator
metadata:
  name: platform
  namespace: flux-system
spec:
  sources:
    - alias: repo
      kind: GitRepository
      name: flux-system
  artifacts:
    - name: platform
      copy:
`)
	files := map[string]string{}
	for _, leaf := range generatorLeaves {
		fmt.Fprintf(&generator, "        - from: \"@repo/apps/%s/**\"\n          to: \"@artifact/apps/%s/\"\n          exclude: [\"oci-source.yaml\"]\n", leaf, leaf)
		files["cluster/"+leaf+".yaml"] = strings.NewReplacer("%NAME%", leaf, "%KIND%", kind, "%SOURCE%", source).Replace(leafKustomization)
		files["apps/"+leaf+"/kustomization.yaml"] = "resources:\n  - cm.yaml\n  - nested\n"
		files["apps/"+leaf+"/cm.yaml"] = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: " + leaf + "\n  namespace: apps\ndata:\n  value: before\n"
		files["apps/"+leaf+"/nested/kustomization.yaml"] = "resources:\n  - cm.yaml\n"
		files["apps/"+leaf+"/nested/cm.yaml"] = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: " + leaf + "-nested\n  namespace: apps\n"
		files["apps/"+leaf+"/oci-source.yaml"] = "this file is excluded and never parsed: {"
	}
	files["cluster/generator.yaml"] = generator.String()
	maps.Copy(files, extra)
	for path, text := range files {
		testutil.WriteFile(t, root, path, text)
	}
	gitCommitAll(t, repo)
	return root
}

// TestE2E_ArtifactGeneratorRendersLikeGit: Kustomizations sourced from an
// in-tree ArtifactGenerator render byte-identically to the same
// Kustomizations sourced from the GitRepository it copies.
func TestE2E_ArtifactGeneratorRendersLikeGit(t *testing.T) {
	want := runCLIStdout(t, "build", "all", "--path", filepath.Join(generatorFixture(t, "GitRepository", nil), "cluster"))
	got := runCLIStdout(t, "build", "all", "--path", filepath.Join(generatorFixture(t, "ExternalArtifact", nil), "cluster"))
	if strings.Count(want, "kind: ConfigMap") != 2*len(generatorLeaves) {
		t.Fatalf("the Git render is incomplete:\n%s", want)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ExternalArtifact render differs from the Git render (-git +artifact):\n%s", diff)
	}
}

// TestE2E_ArtifactGeneratorFailures: a generated artifact that cannot be built
// fails its consumers and the run, and a Kustomization sourced from an
// ExternalArtifact that nothing generates fails instead of rendering nothing.
func TestE2E_ArtifactGeneratorFailures(t *testing.T) {
	cases := map[string]struct {
		extra map[string]string
		want  string
	}{
		"missing glob source": {
			extra: map[string]string{"cluster/generator.yaml": generatorSpec("@repo/apps/missing/**", "@artifact/")},
			want:  "no files match pattern 'apps/missing/**' in source 'repo'",
		},
		"missing file source": {
			extra: map[string]string{"cluster/generator.yaml": generatorSpec("@repo/apps/alpha/missing.yaml", "@artifact/")},
			want:  "source 'apps/alpha/missing.yaml' does not exist",
		},
		"no generator": {
			extra: map[string]string{"cluster/generator.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: unrelated\n  namespace: flux-system\n"},
			want:  "required by flux-system/alpha",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			root := generatorFixture(t, "ExternalArtifact", tc.extra)
			out, code := runCLIExpectErr(t, "build", "all", "--path", filepath.Join(root, "cluster"))
			if code == 0 {
				t.Fatalf("exit 0, want a failure naming %q:\n%s", tc.want, out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("output lacks %q:\n%s", tc.want, out)
			}
		})
	}
}

// TestE2E_ArtifactGeneratorOutsideTreeIsVisibleSkip: a generator whose source
// is not in the rendered tree skips its consumers with a warning.
func TestE2E_ArtifactGeneratorOutsideTreeIsVisibleSkip(t *testing.T) {
	spec := strings.Replace(generatorSpec("@repo/apps/alpha/**", "@artifact/apps/alpha/"),
		"kind: GitRepository\n      name: flux-system", "kind: OCIRepository\n      name: elsewhere", 1)
	root := generatorFixture(t, "ExternalArtifact", map[string]string{"cluster/generator.yaml": spec})
	if out := runCLIStdout(t, "build", "all", "--path", filepath.Join(root, "cluster")); strings.Contains(out, "kind: ConfigMap") {
		t.Errorf("a skipped consumer rendered:\n%s", out)
	}
	if out := runCLI(t, "test", "--path", filepath.Join(root, "cluster")); !strings.Contains(out, "OCIRepository/flux-system/elsewhere is not in the rendered tree") {
		t.Errorf("flate test does not report the skip:\n%s", out)
	}
}

// generatorSpec is the platform ArtifactGenerator with one copy operation.
func generatorSpec(from, to string) string {
	return `apiVersion: source.extensions.fluxcd.io/v1beta1
kind: ArtifactGenerator
metadata:
  name: platform
  namespace: flux-system
spec:
  sources:
    - alias: repo
      kind: GitRepository
      name: flux-system
  artifacts:
    - name: platform
      copy:
        - from: "` + from + `"
          to: "` + to + `"
`
}

// TestE2E_ArtifactGeneratorChartRef: a HelmRelease chartRef to a generated
// ExternalArtifact renders the chart the generator copies, laid out as
// source-watcher's bundle shape (the chart directory under the artifact
// root); a chart at the artifact root is rejected, as helm-controller does.
func TestE2E_ArtifactGeneratorChartRef(t *testing.T) {
	chartRelease := `apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: app
  namespace: apps
spec:
  interval: 10m
  chartRef:
    kind: ExternalArtifact
    name: platform
    namespace: flux-system
`
	extra := map[string]string{
		"apps/alpha/kustomization.yaml":       "resources:\n  - hr.yaml\n",
		"apps/alpha/hr.yaml":                  chartRelease,
		"charts/app/Chart.yaml":               "apiVersion: v2\nname: app\nversion: 1.0.0\n",
		"charts/app/templates/configmap.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: from-chart\ndata:\n  value: {{ .Values.value }}\n",
		"charts/app/values.yaml":              "value: chart-default\n",
	}
	for _, leaf := range generatorLeaves {
		extra["cluster/"+leaf+".yaml"] = strings.NewReplacer("%NAME%", leaf, "%KIND%", "GitRepository", "%SOURCE%", "flux-system").Replace(leafKustomization)
	}
	t.Run("bundle", func(t *testing.T) {
		extra := maps.Clone(extra)
		extra["cluster/generator.yaml"] = generatorSpec("@repo/charts/app/", "@artifact/")
		root := generatorFixture(t, "ExternalArtifact", extra)
		out := runCLIStdout(t, "build", "all", "--path", filepath.Join(root, "cluster"))
		if !strings.Contains(out, "name: from-chart") || !strings.Contains(out, "value: chart-default") {
			t.Fatalf("the chart from the ExternalArtifact did not render:\n%s", out)
		}
	})
	t.Run("chart at artifact root", func(t *testing.T) {
		extra := maps.Clone(extra)
		extra["cluster/generator.yaml"] = generatorSpec("@repo/charts/app/**", "@artifact/")
		root := generatorFixture(t, "ExternalArtifact", extra)
		out, code := runCLIExpectErr(t, "build", "all", "--path", filepath.Join(root, "cluster"))
		if code == 0 || !strings.Contains(out, "chart yaml not in base directory") {
			t.Fatalf("exit %d, want the root Chart.yaml rejected:\n%s", code, out)
		}
	})
}

// TestE2E_ChangedOnlyArtifactGeneratorInputs: a change to a file the
// generator copies, or to the generator, re-renders its consumers; a change
// outside every copy operation re-renders none of them.
func TestE2E_ChangedOnlyArtifactGeneratorInputs(t *testing.T) {
	other := map[string]string{
		"cluster/other.yaml":       strings.NewReplacer("%NAME%", "other", "%KIND%", "GitRepository", "%SOURCE%", "flux-system", "./apps/other", "./other").Replace(leafKustomization),
		"other/kustomization.yaml": "resources:\n  - cm.yaml\n",
		"other/cm.yaml":            "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: other\n  namespace: apps\ndata:\n  value: before\n",
	}
	cases := map[string]struct {
		file, old, new string
		consumers      bool
	}{
		"copied file":   {file: "apps/bravo/cm.yaml", old: "value: before", new: "value: after", consumers: true},
		"generator":     {file: "cluster/generator.yaml", old: "name: platform\n      copy:", new: "name: platform\n      revision: \"@repo\"\n      copy:", consumers: true},
		"uncopied file": {file: "other/cm.yaml", old: "value: before", new: "value: after"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			root := generatorFixture(t, "ExternalArtifact", other)
			mutateFile(t, filepath.Join(root, tc.file), tc.old, tc.new)
			out := runCLIStdout(t, "build", "all", "--path", filepath.Join(root, "cluster"), "--base", "HEAD")
			for _, leaf := range generatorLeaves {
				if got := strings.Contains(out, "name: "+leaf+"\n"); got != tc.consumers {
					t.Errorf("consumer %s rendered=%v, want %v:\n%s", leaf, got, tc.consumers, out)
				}
			}
			if name == "copied file" && !strings.Contains(out, "value: after") {
				t.Errorf("the edited copied file is missing from the render:\n%s", out)
			}
		})
	}
}
