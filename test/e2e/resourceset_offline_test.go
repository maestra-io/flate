package e2e

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/home-operations/flate/internal/testutil"
)

// TestE2E_ResourceSetOfflineInputsRendersNothing: a ResourceSet fed only by a
// pull-request provider renders zero objects, as the cluster does with no open
// pull request, and `flate test` names it; a Static provider still renders.
func TestE2E_ResourceSetOfflineInputsRendersNothing(t *testing.T) {
	root := t.TempDir()
	repo := gitInit(t, root)
	files := map[string]string{
		"cluster/apps.yaml": `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: apps
  namespace: flux-system
spec:
  interval: 10m
  path: ./apps
  prune: true
  sourceRef:
    kind: GitRepository
    name: flux-system
`,
		"apps/kustomization.yaml": "resources:\n  - providers.yaml\n  - sets.yaml\n",
		"leaf/kustomization.yaml": "resources:\n  - cm.yaml\n",
		"leaf/cm.yaml":            "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: leaf\n  namespace: apps\n",
		"apps/providers.yaml": `apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSetInputProvider
metadata:
  name: pulls
  namespace: apps
  labels:
    envs: pulls
spec:
  type: GitHubPullRequest
  url: https://github.com/example/app
---
apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSetInputProvider
metadata:
  name: fixed
  namespace: apps
spec:
  type: Static
  defaultValues:
    id: fixed
`,
		"apps/sets.yaml": `apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSet
metadata:
  name: pr-envs
  namespace: apps
spec:
  inputsFrom:
    - apiVersion: fluxcd.controlplane.io/v1
      kind: ResourceSetInputProvider
      selector:
        matchLabels:
          envs: pulls
  resourcesTemplate: |
    <<- $id := inputs.id | toString ->>
    apiVersion: kustomize.toolkit.fluxcd.io/v1
    kind: Kustomization
    metadata:
      name: pr-<< $id >>
      namespace: flux-system
    spec:
      interval: 10m
      path: ./leaf
      prune: true
      sourceRef:
        kind: GitRepository
        name: flux-system
---
apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSet
metadata:
  name: static-envs
  namespace: apps
spec:
  inputsFrom:
    - apiVersion: fluxcd.controlplane.io/v1
      kind: ResourceSetInputProvider
      name: fixed
  resourcesTemplate: |
    apiVersion: kustomize.toolkit.fluxcd.io/v1
    kind: Kustomization
    metadata:
      name: static-<< inputs.id >>
      namespace: flux-system
    spec:
      interval: 10m
      path: ./leaf
      prune: true
      sourceRef:
        kind: GitRepository
        name: flux-system
`,
	}
	for path, text := range files {
		testutil.WriteFile(t, root, path, text)
	}
	gitCommitAll(t, repo)
	report := runCLI(t, "test", "--path", filepath.Join(root, "cluster"))
	if !strings.Contains(report, "flux-system/static-fixed") {
		t.Errorf("the Static provider's ResourceSet did not render its Kustomization:\n%s", report)
	}
	if strings.Contains(report, "flux-system/pr-") {
		t.Errorf("the pull-request ResourceSet rendered a Kustomization:\n%s", report)
	}
	want := "ResourceSet apps/pr-envs: inputs of GitHubPullRequest providers are not available offline, rendered 0 objects"
	if !strings.Contains(report, want) {
		t.Errorf("flate test lacks %q:\n%s", want, report)
	}
	if strings.Contains(report, "static-envs: inputs") {
		t.Errorf("the Static ResourceSet was reported:\n%s", report)
	}
}
