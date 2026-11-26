package e2e

import (
	"strings"
	"testing"

	"github.com/home-operations/flate/internal/testutil"
)

// writeResourceSetDependsOnCluster: Kustomization apps dependsOn a
// Kustomization only a ResourceSet emits, and substitutes ${REGISTRY} from a
// ConfigMap another Kustomization renders (fluxcd omicron microfrontends,
// 25.11.2026: frontend-entry-static-builder from ResourceSet kargo-services,
// CONTAINER_REGISTRY from cluster-vars).
func writeResourceSetDependsOnCluster(t *testing.T, dir, dependsOn string) {
	t.Helper()
	files := map[string]string{
		"cluster/flux-system.yaml": "apiVersion: kustomize.toolkit.fluxcd.io/v1\nkind: Kustomization\nmetadata: {name: flux-system, namespace: flux-system}\nspec:\n  path: ./flux\n",
		"flux/kustomization.yaml":  "resources: [vars.yaml, gen.yaml, apps.yaml]\n",
		"flux/vars.yaml":           "apiVersion: kustomize.toolkit.fluxcd.io/v1\nkind: Kustomization\nmetadata: {name: vars, namespace: flux-system}\nspec:\n  path: ./vars\n",
		"vars/kustomization.yaml":  "resources: [cluster-vars.yaml]\n",
		"vars/cluster-vars.yaml":   "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: cluster-vars, namespace: flux-system}\ndata: {REGISTRY: registry.example.com}\n",
		"flux/gen.yaml": `apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSet
metadata: {name: gen, namespace: flux-system}
spec:
  inputs:
    - name: generated
  resourcesTemplate: |
    apiVersion: kustomize.toolkit.fluxcd.io/v1
    kind: Kustomization
    metadata:
      name: << inputs.name >>
      namespace: flux-system
    spec:
      path: ./leaf
`,
		"leaf/kustomization.yaml": "resources: [cm.yaml]\n",
		"leaf/cm.yaml":            "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: leaf, namespace: default}\n",
		"flux/apps.yaml":          "apiVersion: kustomize.toolkit.fluxcd.io/v1\nkind: Kustomization\nmetadata: {name: apps, namespace: flux-system}\nspec:\n  path: ./apps\n  dependsOn: [{name: " + dependsOn + "}]\n  postBuild:\n    substituteFrom: [{kind: ConfigMap, name: cluster-vars}]\n",
		"apps/kustomization.yaml": "resources: [cm.yaml]\n",
		"apps/cm.yaml":            "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: app, namespace: default}\ndata: {registry: \"${REGISTRY}\"}\n",
	}
	for path, body := range files {
		testutil.WriteFile(t, dir, path, body)
	}
}

// TestE2E_ChangedOnlyDependsOnResourceSetChild: an edit under apps/ alone must
// render apps in changed-only mode with ${REGISTRY} substituted; its dependsOn
// target exists only once the ResourceSet renders, so the ResourceSet MUST
// join the keep set or apps fails "generated not found".
func TestE2E_ChangedOnlyDependsOnResourceSetChild(t *testing.T) {
	for _, mode := range []string{"full", "changed"} {
		t.Run(mode, func(t *testing.T) {
			root, original := t.TempDir(), t.TempDir()
			writeResourceSetDependsOnCluster(t, root, "generated")
			writeResourceSetDependsOnCluster(t, original, "generated")
			testutil.WriteFile(t, root, "apps/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: app, namespace: default}\ndata: {registry: \"${REGISTRY}\", edited: \"yes\"}\n")
			args := []string{"build", "all", "--path", root, "--cache-dir", t.TempDir(), "--concurrency", "2", "--no-progress", "--strict-substitutions"}
			if mode == "changed" {
				args = append(args, "--path-orig", original)
			}
			stdout, stderr, code := runCLIBuffers(args...)
			if code != 0 || !strings.Contains(stdout, "registry: registry.example.com") || strings.Contains(stdout, "${REGISTRY}") {
				t.Fatalf("exit=%d stdout=%s stderr=%s", code, stdout, stderr)
			}
		})
	}
}

// TestE2E_ChangedOnlyDanglingDependsOnFails: a dependsOn no tree object or
// ResourceSet produces still fails the changed-only render.
func TestE2E_ChangedOnlyDanglingDependsOnFails(t *testing.T) {
	root, original := t.TempDir(), t.TempDir()
	writeResourceSetDependsOnCluster(t, root, "generatd")
	writeResourceSetDependsOnCluster(t, original, "generated")
	_, stderr, code := runCLIBuffers("build", "all", "--path", root, "--path-orig", original, "--cache-dir", t.TempDir(), "--concurrency", "2", "--no-progress")
	if code == 0 || !strings.Contains(stderr, "flux-system/generatd  not found") {
		t.Fatalf("dangling dependsOn: exit=%d stderr=%s", code, stderr)
	}
}
