/*
Copyright 2025 The Flux authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

Modified for flate: ported from github.com/fluxcd/source-watcher v2.3.1
internal/builder/*_test.go
*/

package generator

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/home-operations/flate/internal/assert"
	"github.com/home-operations/flate/pkg/manifest"
	"github.com/home-operations/flate/pkg/tree"
)

// buildCase is one Build run. want lists every produced path: files map to
// their content, directories carry a trailing '/' and map to "".
type buildCase struct {
	name    string
	sources map[string]fs.FS
	copy    []manifest.CopyOperation
	want    map[string]string
	// yaml compares file contents as YAML documents, not bytes.
	yaml    bool
	wantErr string
}

func runBuildCases(t *testing.T, cases []buildCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			got, digest, err := Build(t.Context(), manifest.GeneratedArtifact{Copy: tc.copy}, tc.sources)
			elapsed := time.Since(start)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
				}
				if got != nil {
					t.Errorf("tree = %v, want nil on error", paths(t, got))
				}
				// Rejected before any matcher runs, not after.
				if elapsed >= 2*time.Second {
					t.Errorf("rejection took %v, want < 2s", elapsed)
				}
				return
			}
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if !strings.HasPrefix(digest, "sha256:") {
				t.Errorf("digest = %q, want a sha256 digest", digest)
			}
			gotPaths, want := paths(t, got), tc.want
			if tc.yaml {
				gotPaths, want = canonYAML(t, gotPaths), canonYAML(t, want)
			}
			assert.Diff(t, gotPaths, want)
		})
	}
}

// paths reads every entry of fsys as described on buildCase.want.
func paths(t *testing.T, fsys fs.FS) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || name == "." {
			return err
		}
		if d.IsDir() {
			out[name+"/"] = ""
			return nil
		}
		data, err := fs.ReadFile(fsys, name)
		out[name] = string(data)
		return err
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return out
}

// canonYAML re-encodes every file content so equal YAML compares equal.
func canonYAML(t *testing.T, files map[string]string) map[string]string {
	t.Helper()
	out := make(map[string]string, len(files))
	for name, content := range files {
		if strings.HasSuffix(name, "/") {
			out[name] = content
			continue
		}
		var v any
		if err := yaml.Unmarshal([]byte(content), &v); err != nil {
			t.Fatalf("%s is not YAML: %v", name, err)
		}
		b, err := yaml.Marshal(v)
		if err != nil {
			t.Fatalf("marshal %s: %v", name, err)
		}
		out[name] = string(b)
	}
	return out
}

// src builds a source from name, content pairs.
func src(kv ...string) fstest.MapFS {
	m := fstest.MapFS{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i]] = &fstest.MapFile{Data: []byte(kv[i+1]), Mode: 0o644}
	}
	return m
}

// tgz builds a gzip tarball from name, content pairs; a name ending in '/'
// is a directory entry.
func tgz(t *testing.T, kv ...string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for i := 0; i < len(kv); i += 2 {
		h := &tar.Header{Name: kv[i], Mode: 0o644, Size: int64(len(kv[i+1]))}
		if strings.HasSuffix(kv[i], "/") {
			h.Typeflag, h.Mode = tar.TypeDir, 0o755
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(kv[i+1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestBuild_Copy(t *testing.T) {
	runBuildCases(t, []buildCase{
		{
			name:    "build artifact with single file copy",
			sources: map[string]fs.FS{"source": src("config.yaml", "apiVersion: v1\nkind: ConfigMap")},
			copy:    []manifest.CopyOperation{{From: "@source/config.yaml", To: "@artifact/"}},
			want:    map[string]string{"config.yaml": "apiVersion: v1\nkind: ConfigMap"},
		},
		{
			name: "build artifact with multiple copy operations",
			sources: map[string]fs.FS{
				"git": src("app.yaml", "apiVersion: apps/v1"),
				"oci": src("config.json", `{"version": "1.0"}`),
			},
			copy: []manifest.CopyOperation{
				{From: "@git/app.yaml", To: "@artifact/manifests/"},
				{From: "@oci/config.json", To: "@artifact/config/"},
			},
			want: map[string]string{
				"manifests/":         "",
				"manifests/app.yaml": "apiVersion: apps/v1",
				"config/":            "",
				"config/config.json": `{"version": "1.0"}`,
			},
		},
		{
			name: "build artifact with glob patterns",
			sources: map[string]fs.FS{"source": src(
				"deployment.yaml", "apiVersion: v1",
				"service.yaml", "apiVersion: v1",
				"configmap.yaml", "apiVersion: v1",
			)},
			copy: []manifest.CopyOperation{{From: "@source/*.yaml", To: "@artifact/manifests/"}},
			want: map[string]string{
				"manifests/":                "",
				"manifests/deployment.yaml": "apiVersion: v1",
				"manifests/service.yaml":    "apiVersion: v1",
				"manifests/configmap.yaml":  "apiVersion: v1",
			},
		},
		{
			name:    "build artifact with root directory copy",
			sources: map[string]fs.FS{"source": src("root.yaml", "root: content", "subdir/nested.yaml", "nested: content")},
			copy:    []manifest.CopyOperation{{From: "@source/", To: "@artifact/"}},
			want: map[string]string{
				"root.yaml":          "root: content",
				"subdir/":            "",
				"subdir/nested.yaml": "nested: content",
			},
		},
		{
			name:    "build artifact with recursive directory copy",
			sources: map[string]fs.FS{"source": src("root.yaml", "root: content", "subdir/nested.yaml", "nested: content")},
			copy:    []manifest.CopyOperation{{From: "@source/**", To: "@artifact/"}},
			want: map[string]string{
				"root.yaml":          "root: content",
				"subdir/":            "",
				"subdir/nested.yaml": "nested: content",
			},
		},
		{
			name: "copy operations overwrite entire subdirectories",
			sources: map[string]fs.FS{
				"source1": src("config/app.yaml", "source1: app", "config/database.yaml", "source1: db"),
				"source2": src("config/app.yaml", "source2: app", "config/network.yaml", "source2: net"),
			},
			copy: []manifest.CopyOperation{
				{From: "@source1/**", To: "@artifact/"},
				{From: "@source2/**", To: "@artifact/"},
			},
			want: map[string]string{
				"config/":              "",
				"config/app.yaml":      "source2: app",
				"config/database.yaml": "source1: db",
				"config/network.yaml":  "source2: net",
			},
		},
		{
			name:    "directory copy without trailing slash creates subdirectory",
			sources: map[string]fs.FS{"source": src("config/app.yaml", "app: config", "config/db.yaml", "db: config")},
			copy:    []manifest.CopyOperation{{From: "@source/config", To: "@artifact/dest"}},
			want: map[string]string{
				"dest/":                "",
				"dest/config/":         "",
				"dest/config/app.yaml": "app: config",
				"dest/config/db.yaml":  "db: config",
			},
		},
		{
			name:    "directory copy with trailing slash creates subdirectory",
			sources: map[string]fs.FS{"source": src("config/app.yaml", "app: config", "config/db.yaml", "db: config")},
			copy:    []manifest.CopyOperation{{From: "@source/config/", To: "@artifact/dest/"}},
			want: map[string]string{
				"dest/":                "",
				"dest/config/":         "",
				"dest/config/app.yaml": "app: config",
				"dest/config/db.yaml":  "db: config",
			},
		},
		{
			name:    "file copy with trailing slash uses source filename",
			sources: map[string]fs.FS{"source": src("config.yaml", "a: test")},
			copy:    []manifest.CopyOperation{{From: "@source/config.yaml", To: "@artifact/dest/"}},
			want:    map[string]string{"dest/": "", "dest/config.yaml": "a: test"},
		},
		{
			name:    "recursive glob pattern strips directory prefix like cp",
			sources: map[string]fs.FS{"source": src("config/app.yaml", "app: config", "config/subdir/db.yaml", "db: config")},
			copy:    []manifest.CopyOperation{{From: "@source/config/**", To: "@artifact/dest/"}},
			want: map[string]string{
				"dest/":               "",
				"dest/app.yaml":       "app: config",
				"dest/subdir/":        "",
				"dest/subdir/db.yaml": "db: config",
			},
		},
		{
			name: "file-to-file copy should not treat destination as directory",
			sources: map[string]fs.FS{
				"chart": src("Chart.yaml", "name: podinfo", "values.yaml", "name: podinfo"),
				"repo":  src("charts/podinfo/values-prod.yaml", "env: production"),
			},
			copy: []manifest.CopyOperation{
				{From: "@chart/**", To: "@artifact/"},
				{From: "@repo/charts/podinfo/values-prod.yaml", To: "@artifact/podinfo/values.yaml"},
			},
			want: map[string]string{
				"Chart.yaml":          "name: podinfo",
				"values.yaml":         "name: podinfo",
				"podinfo/":            "",
				"podinfo/values.yaml": "env: production",
			},
		},
		{
			name:    "file copy should detect when destination is a directory",
			sources: map[string]fs.FS{"source": src("file1.txt", "content1", "subdir/file2.txt", "content2")},
			copy: []manifest.CopyOperation{
				{From: "@source/subdir/", To: "@artifact/"},
				{From: "@source/file1.txt", To: "@artifact/subdir"},
			},
			want: map[string]string{
				"subdir/":          "",
				"subdir/file1.txt": "content1",
				"subdir/file2.txt": "content2",
			},
		},
	})
}

func TestBuild_Errors(t *testing.T) {
	runBuildCases(t, []buildCase{
		{
			name:    "error when source file does not exist",
			sources: map[string]fs.FS{"source": src()},
			copy:    []manifest.CopyOperation{{From: "@source/test.yaml", To: "@artifact/"}},
			wantErr: "does not exist",
		},
		{
			name:    "error when glob pattern matches no files",
			sources: map[string]fs.FS{"source": src()},
			copy:    []manifest.CopyOperation{{From: "@source/*.yaml", To: "@artifact/"}},
			wantErr: "no files match pattern",
		},
		{
			name:    "error when invalid glob pattern is used",
			sources: map[string]fs.FS{"source": src()},
			copy:    []manifest.CopyOperation{{From: "@source/[", To: "@artifact/"}},
			wantErr: "syntax error in pattern",
		},
		{
			name:    "error when source alias not found",
			sources: map[string]fs.FS{"source": src()},
			copy:    []manifest.CopyOperation{{From: "@missing/file.yaml", To: "@artifact/"}},
			wantErr: "source alias 'missing' not found",
		},
		{
			name:    "optional skips when source file does not exist",
			sources: map[string]fs.FS{"source": src()},
			copy:    []manifest.CopyOperation{{From: "@source/test.yaml", To: "@artifact/", Optional: true}},
			want:    map[string]string{},
		},
		{
			name:    "optional skips when glob pattern matches no files",
			sources: map[string]fs.FS{"source": src()},
			copy:    []manifest.CopyOperation{{From: "@source/*.yaml", To: "@artifact/", Optional: true}},
			want:    map[string]string{},
		},
		{
			name:    "optional skips when all files are excluded",
			sources: map[string]fs.FS{"source": src("test.md", "test file", "other.md", "other file")},
			copy: []manifest.CopyOperation{
				{From: "@source/*.md", To: "@artifact/", Exclude: []string{"*.md"}, Optional: true},
			},
			want: map[string]string{},
		},
	})
}

func TestBuild_CopySources(t *testing.T) {
	cases := []struct{ name, from, wantErr string }{
		{name: "missing alias prefix", from: "source/config.yaml", wantErr: "source must start with '@'"},
		{name: "alias without a pattern", from: "@source", wantErr: "source format must be '@alias/pattern'"},
		{name: "unknown alias", from: "@missing/config.yaml", wantErr: "source alias 'missing' not found"},
	}
	var bc []buildCase
	for _, c := range cases {
		bc = append(bc, buildCase{
			name:    c.name,
			sources: map[string]fs.FS{"source": src("config.yaml", "apiVersion: v1")},
			copy:    []manifest.CopyOperation{{From: c.from, To: "@artifact/"}},
			wantErr: c.wantErr,
		})
	}
	runBuildCases(t, bc)
}

// TestBuild_GlobAlternationLimit checks that patterns too expensive to match
// are rejected before reaching a matcher: each rejected pattern takes seconds
// or more to match, so runBuildCases' elapsed bound catches late validation.
func TestBuild_GlobAlternationLimit(t *testing.T) {
	manyGroups := strings.Repeat("{a,a}", 30) + "z"
	wideGroups := strings.Repeat("{a,a,a,a,a,a,a,a}", 10) + "z"
	stallsPreValidation := "{.,.}" + strings.Repeat("{,}", 29) + "z"
	atLimit := strings.Repeat("{a,a}", 20) + "zzz*"
	// Long enough for the alternation recursion to bite.
	longName := strings.Repeat("a", 40)

	cases := []struct {
		name, from, strategy string
		exclude              []string
		wantErr              string
	}{
		{name: "many narrow groups in from", from: "@source/" + manyGroups + "*", wantErr: "at most 20 are allowed"},
		{name: "many narrow groups in exclude", from: "@source/**", exclude: []string{manyGroups}, wantErr: "at most 20 are allowed"},
		{name: "many narrow groups in a later exclude entry", from: "@source/**", exclude: []string{"*.md", manyGroups}, wantErr: "at most 20 are allowed"},
		{name: "few wide groups in exclude", from: "@source/**", exclude: []string{wideGroups}, wantErr: "at most 20 are allowed"},
		{name: "few wide groups in from", from: "@source/" + wideGroups + "*", wantErr: "at most 20 are allowed"},
		{name: "pattern that stalls the exclude syntax check", from: "@source/**", exclude: []string{stallsPreValidation}, wantErr: "at most 20 are allowed"},
		{
			// Extract takes the doublestar.Glob branch, not fs.Glob.
			name: "many narrow groups in from with Extract strategy", from: "@source/" + manyGroups + "*.tgz",
			strategy: manifest.CopyStrategyExtract, wantErr: "at most 20 are allowed",
		},
		{name: "at the limit is accepted", from: "@source/**", exclude: []string{atLimit}},
		{name: "escaped braces are literals and do not count", from: "@source/**", exclude: []string{strings.Repeat(`\{a\}`, 12) + "/**"}},
		{name: "realistic extension alternation is accepted", from: "@source/**", exclude: []string{"**/*.{yaml,yml,json,toml}"}},
	}
	var bc []buildCase
	for _, c := range cases {
		b := buildCase{
			name:    c.name,
			sources: map[string]fs.FS{"source": src(longName, "content")},
			copy:    []manifest.CopyOperation{{From: c.from, To: "@artifact/", Exclude: c.exclude, Strategy: c.strategy}},
			wantErr: c.wantErr,
		}
		if c.wantErr == "" {
			b.want = map[string]string{longName: "content"}
		}
		bc = append(bc, b)
	}
	runBuildCases(t, bc)
}

func TestBuild_CopyDestinations(t *testing.T) {
	cases := []struct {
		name, to string
		want     map[string]string
		wantErr  string
	}{
		{name: "artifact root", to: "@artifact/", want: map[string]string{"config.yaml": "apiVersion: v1"}},
		{
			name: "subdirectory with trailing slash", to: "@artifact/sub/dir/",
			want: map[string]string{"sub/": "", "sub/dir/": "", "sub/dir/config.yaml": "apiVersion: v1"},
		},
		{
			name: "subdirectory without trailing slash renames the file", to: "@artifact/sub/dir",
			want: map[string]string{"sub/": "", "sub/dir": "apiVersion: v1"},
		},
		{
			name: "current directory component", to: "@artifact/./sub/",
			want: map[string]string{"sub/": "", "sub/config.yaml": "apiVersion: v1"},
		},
		{
			name: "dots within a directory name", to: "@artifact/sub..dir/",
			want: map[string]string{"sub..dir/": "", "sub..dir/config.yaml": "apiVersion: v1"},
		},
		{name: "parent directory", to: "@artifact/..", wantErr: "destination path must stay within the artifact root"},
		{name: "parent traversal", to: "@artifact/../../escape", wantErr: "destination path must stay within the artifact root"},
		{name: "nested parent component", to: "@artifact/sub/../escape", wantErr: "destination path must not contain '..'"},
		{name: "absolute path", to: "@artifact//tmp/escape", wantErr: "destination path must stay within the artifact root"},
		{name: "unknown destination alias", to: "@source/sub", wantErr: "destination must start with '@artifact/'"},
	}
	var bc []buildCase
	for _, c := range cases {
		bc = append(bc, buildCase{
			name:    c.name,
			sources: map[string]fs.FS{"source": src("config.yaml", "apiVersion: v1")},
			copy:    []manifest.CopyOperation{{From: "@source/config.yaml", To: c.to}},
			want:    c.want,
			wantErr: c.wantErr,
		})
	}
	runBuildCases(t, bc)
}

func TestBuild_Excludes(t *testing.T) {
	infra := func(extra ...string) fstest.MapFS {
		return src(append([]string{
			"infrastructure/controllers/descheduler/release.yaml", "descheduler release",
			"infrastructure/controllers/sealed-secrets/release.yaml", "sealed-secrets release",
			"infrastructure/configs/cluster-issuer.yaml", "cluster issuer",
		}, extra...)...)
	}
	globRootWant := map[string]string{
		"controllers/":                            "",
		"controllers/sealed-secrets/":             "",
		"controllers/sealed-secrets/release.yaml": "sealed-secrets release",
		"configs/":                    "",
		"configs/cluster-issuer.yaml": "cluster issuer",
	}
	nonGlobWant := map[string]string{
		"infrastructure/":                                        "",
		"infrastructure/controllers/":                            "",
		"infrastructure/controllers/sealed-secrets/":             "",
		"infrastructure/controllers/sealed-secrets/release.yaml": "sealed-secrets release",
		"infrastructure/configs/":                                "",
		"infrastructure/configs/cluster-issuer.yaml":             "cluster issuer",
	}
	runBuildCases(t, []buildCase{
		{
			name: "exclude single file extension",
			sources: map[string]fs.FS{"source": src(
				"app.yaml", "yaml content",
				"config.json", "json content",
				"README.md", "markdown content",
				"notes.md", "more markdown",
			)},
			copy: []manifest.CopyOperation{{From: "@source/**", To: "@artifact/", Exclude: []string{"*.md"}}},
			want: map[string]string{"app.yaml": "yaml content", "config.json": "json content"},
		},
		{
			name: "exclude directory recursively",
			sources: map[string]fs.FS{"source": src(
				"app.yaml", "main app config",
				"testdata/test.yaml", "test config",
				"testdata/fixture.json", "test fixture",
				"configs/prod.yaml", "prod config",
			)},
			copy: []manifest.CopyOperation{{From: "@source/**", To: "@artifact/", Exclude: []string{"**/testdata/**"}}},
			want: map[string]string{"app.yaml": "main app config", "configs/": "", "configs/prod.yaml": "prod config"},
		},
		{
			name: "multiple exclude patterns",
			sources: map[string]fs.FS{"source": src(
				"app.yaml", "app config",
				"README.md", "documentation",
				"temp.tmp", "temporary file",
				".hidden", "hidden file",
				"docs/guide.md", "user guide",
				"tests/test.go", "test file",
			)},
			copy: []manifest.CopyOperation{{
				From: "@source/**", To: "@artifact/",
				Exclude: []string{"*.md", "*.tmp", "**/.*", "**/tests/**"},
			}},
			// docs/ stays, emptied of guide.md.
			want: map[string]string{"app.yaml": "app config", "docs/": ""},
		},
		{
			name: "exclude with glob patterns",
			sources: map[string]fs.FS{"source": src(
				"app.yaml", "app config",
				"config.yaml", "config",
				"secret.yaml", "secret",
				"other.json", "other config",
			)},
			copy: []manifest.CopyOperation{{From: "@source/*.yaml", To: "@artifact/configs/", Exclude: []string{"secret.yaml"}}},
			want: map[string]string{"configs/": "", "configs/app.yaml": "app config", "configs/config.yaml": "config"},
		},
		{
			name:    "exclude single file copy",
			sources: map[string]fs.FS{"source": src("secret.yaml", "secret content")},
			copy:    []manifest.CopyOperation{{From: "@source/secret.yaml", To: "@artifact/config.yaml", Exclude: []string{"secret.yaml"}}},
			want:    map[string]string{},
		},
		{
			// https://github.com/fluxcd/source-watcher/issues/306
			name:    "exclude nested directory by full path from source root",
			sources: map[string]fs.FS{"source": infra("infrastructure/controllers/descheduler/kustomization.yaml", "descheduler kustomization")},
			copy: []manifest.CopyOperation{{
				From: "@source/infrastructure/**", To: "@artifact/",
				Exclude: []string{"infrastructure/controllers/descheduler/**"},
			}},
			want: globRootWant,
		},
		{
			name:    "exclude nested directory from non-glob directory copy by source root path",
			sources: map[string]fs.FS{"source": infra()},
			copy: []manifest.CopyOperation{{
				From: "@source/infrastructure", To: "@artifact/",
				Exclude: []string{"infrastructure/controllers/descheduler/**"},
			}},
			want: nonGlobWant,
		},
		{
			name:    "exclude nested directory from glob directory copy by selected root path",
			sources: map[string]fs.FS{"source": infra()},
			copy: []manifest.CopyOperation{{
				From: "@source/infrastructure/**", To: "@artifact/",
				Exclude: []string{"controllers/descheduler/**"},
			}},
			want: globRootWant,
		},
		{
			name: "exclude does not use recursive walk root as anchor",
			sources: map[string]fs.FS{"source": src(
				"infrastructure/descheduler/release.yaml", "top-level descheduler",
				"infrastructure/controllers/descheduler/release.yaml", "nested descheduler",
				"infrastructure/controllers/sealed-secrets/release.yaml", "sealed-secrets release",
			)},
			copy: []manifest.CopyOperation{{From: "@source/infrastructure/**", To: "@artifact/", Exclude: []string{"descheduler/**"}}},
			want: map[string]string{
				"controllers/":                            "",
				"controllers/descheduler/":                "",
				"controllers/descheduler/release.yaml":    "nested descheduler",
				"controllers/sealed-secrets/":             "",
				"controllers/sealed-secrets/release.yaml": "sealed-secrets release",
			},
		},
		{
			name:    "exclude nested directory from non-glob directory copy by subtree path",
			sources: map[string]fs.FS{"source": infra()},
			copy: []manifest.CopyOperation{{
				From: "@source/infrastructure", To: "@artifact/",
				Exclude: []string{"controllers/descheduler/**"},
			}},
			want: nonGlobWant,
		},
		{
			name: "exclude single nested file by full path from source root",
			sources: map[string]fs.FS{"source": src(
				"infrastructure/configs/app.yaml", "app config",
				"infrastructure/configs/secret.yaml", "secret config",
			)},
			copy: []manifest.CopyOperation{{
				From: "@source/infrastructure/**", To: "@artifact/",
				Exclude: []string{"infrastructure/configs/secret.yaml"},
			}},
			want: map[string]string{"configs/": "", "configs/app.yaml": "app config"},
		},
		{
			name:    "all files excluded - error",
			sources: map[string]fs.FS{"source": src("test.md", "test file", "other.md", "other file")},
			copy:    []manifest.CopyOperation{{From: "@source/*.md", To: "@artifact/", Exclude: []string{"*.md"}}},
			wantErr: "all files matching pattern",
		},
		{
			name:    "invalid exclude - error",
			sources: map[string]fs.FS{"source": src()},
			copy:    []manifest.CopyOperation{{From: "@source/*.md", To: "@artifact/", Exclude: []string{"[*.md"}}},
			wantErr: "invalid exclude pattern",
		},
	})
}

func TestBuild_MergeStrategy(t *testing.T) {
	two := func(a, b fstest.MapFS) map[string]fs.FS { return map[string]fs.FS{"source1": a, "source2": b} }
	runBuildCases(t, []buildCase{
		{
			name: "merge YAML files successfully",
			sources: two(
				src("config.yaml", `
name: app
ports: [80, 443]
labels:
  env: dev
  keep: me
version: 1.0.0
replicas: 3
`),
				src("config.yaml", `
replicas: 5 # This should overwrite the replicas
ports: [8080] # This should overwrite the ports array
labels:
  env: prod # This should overwrite the env label
env: production # This should add a new top-level field
`)),
			copy: []manifest.CopyOperation{
				{From: "@source1/config.yaml", To: "@artifact/config.yaml", Strategy: manifest.CopyStrategyOverwrite},
				{From: "@source2/config.yaml", To: "@artifact/config.yaml", Strategy: manifest.CopyStrategyMerge},
			},
			yaml: true,
			want: map[string]string{"config.yaml": `
name: app
ports: [8080]
labels:
  env: prod
  keep: me
version: 1.0.0
replicas: 5
env: production
`},
		},
		{
			name:    "merge JSON with YAML successfully",
			sources: two(src("config.yaml", "a: b"), src("config.json", `{"c": {"d": "e"}, "a": "override"}`)),
			copy: []manifest.CopyOperation{
				{From: "@source1/config.yaml", To: "@artifact/config.yaml"},
				// YAML is a superset of JSON.
				{From: "@source2/config.json", To: "@artifact/config.yaml", Strategy: manifest.CopyStrategyMerge},
			},
			yaml: true,
			want: map[string]string{"config.yaml": "a: override\nc:\n  d: e\n"},
		},
		{
			name: "merge YAML in dirs successfully",
			sources: two(
				src("config1.yaml", "\nenv: dev\nregion: us-west-1\n", "config2.yaml", "\nversion: 1.0.0\nimage: my-app:latest\n"),
				src("config1.yaml", "env: prod", "config2.yaml", "replicas: 5"),
			),
			copy: []manifest.CopyOperation{
				{From: "@source1/**", To: "@artifact/", Strategy: manifest.CopyStrategyOverwrite},
				{From: "@source2/**", To: "@artifact/", Strategy: manifest.CopyStrategyMerge},
			},
			yaml: true,
			want: map[string]string{
				"config1.yaml": "env: prod\nregion: us-west-1\n",
				"config2.yaml": "image: my-app:latest\nreplicas: 5\nversion: 1.0.0\n",
			},
		},
		{
			name: "merge YAML in dirs with exclude",
			sources: two(
				src("config.yaml", "\nenv: dev\nregion: us-west-1\n", "secret.yaml", "token: base"),
				src("config.yaml", "env: prod", "secret.yaml", "token: leak"),
			),
			copy: []manifest.CopyOperation{
				{From: "@source1/**", To: "@artifact/", Strategy: manifest.CopyStrategyOverwrite},
				{From: "@source2/**", To: "@artifact/", Strategy: manifest.CopyStrategyMerge, Exclude: []string{"secret.yaml"}},
			},
			yaml: true,
			want: map[string]string{
				"config.yaml": "env: prod\nregion: us-west-1\n",
				"secret.yaml": "token: base",
			},
		},
		{
			name:    "merge with non existing destination file",
			sources: map[string]fs.FS{"source": src("config.yaml", "a: b")},
			copy: []manifest.CopyOperation{
				{From: "@source/config.yaml", To: "@artifact/config.yaml", Strategy: manifest.CopyStrategyMerge},
			},
			yaml: true,
			want: map[string]string{"config.yaml": "a: b"},
		},
		{
			name:    "fails with destination not YAML",
			sources: two(src("config.env", "A=B"), src("config.yaml", "a: b")),
			copy: []manifest.CopyOperation{
				{From: "@source1/config.env", To: "@artifact/config.env"},
				{From: "@source2/config.yaml", To: "@artifact/config.env", Strategy: manifest.CopyStrategyMerge},
			},
			wantErr: "cannot unmarshal YAML document",
		},
		{
			name:    "fails with source not YAML",
			sources: two(src("config.yaml", "a: b"), src("config.env", "A=B")),
			copy: []manifest.CopyOperation{
				{From: "@source1/config.yaml", To: "@artifact/config.yaml"},
				{From: "@source2/config.env", To: "@artifact/config.yaml", Strategy: manifest.CopyStrategyMerge},
			},
			wantErr: "cannot unmarshal YAML document",
		},
	})
}

const (
	devConfig = `
name: app
ports: [8080]
labels:
  env: dev
  keep: me
version: 1.0.1
replicas: 2
env: development
`
	prodConfig = `
name: app
ports: [8081]
labels:
  env: prod
  keep: me
version: 1.0.0
replicas: 5
env: production
`
	intConfig = `
name: app
ports: [8082]
labels:
  env: int
  keep: me
version: 1.0.0
replicas: 3
env: int
`
)

func TestBuild_ExtractStrategy(t *testing.T) {
	appTgz := tgz(t, "config.yaml", devConfig, "prod/", "", "prod/config.yaml", prodConfig)
	intTgz := tgz(t, "int/", "", "int/config.yaml", intConfig)
	appWant := map[string]string{"config.yaml": devConfig, "prod/": "", "prod/config.yaml": prodConfig}
	bothWant := map[string]string{
		"config.yaml": devConfig, "prod/": "", "prod/config.yaml": prodConfig,
		"int/": "", "int/config.yaml": intConfig,
	}
	extract := func(from, to string) []manifest.CopyOperation {
		return []manifest.CopyOperation{{From: from, To: to, Strategy: manifest.CopyStrategyExtract}}
	}
	firstConfig := "\nname: first-app\nversion: 1.0.0\nenv: first\n"
	secondConfig := "\nname: second-app\nversion: 2.0.0\nenv: second\n"
	runBuildCases(t, []buildCase{
		{
			name:    "Extract single referenced archive successfully",
			sources: map[string]fs.FS{"source": src("manifests.tgz", appTgz)},
			copy:    extract("@source/manifests.tgz", "@artifact/manifests"),
			want: map[string]string{
				"manifests/":                 "",
				"manifests/config.yaml":      devConfig,
				"manifests/prod/":            "",
				"manifests/prod/config.yaml": prodConfig,
			},
		},
		{
			name:    "Extract multiple referenced archives using same source pattern",
			sources: map[string]fs.FS{"source": src("manifests1.tgz", appTgz, "manifests2.tgz", intTgz)},
			copy:    extract("@source/*.tg*", "@artifact/"),
			want:    bothWant,
		},
		{
			name:    "Extract multiple referenced archives using recursive glob pattent matching",
			sources: map[string]fs.FS{"source": src("prod/manifests.tgz", appTgz, "int/manifests.tgz", intTgz)},
			copy:    extract("@source/**", "@artifact/"),
			want:    bothWant,
		},
		{
			name:    "Extract with invalid tarball fails gracefully",
			sources: map[string]fs.FS{"source": src("invalid.tgz", "this is not a tarball")},
			copy:    extract("@source/invalid.tgz", "@artifact/"),
			wantErr: "failed to extract tarball",
		},
		{
			name:    "Extract to subdirectory",
			sources: map[string]fs.FS{"source": src("manifests.tgz", appTgz)},
			copy:    extract("@source/manifests.tgz", "@artifact/extracted/configs/"),
			want: map[string]string{
				"extracted/":                         "",
				"extracted/configs/":                 "",
				"extracted/configs/config.yaml":      devConfig,
				"extracted/configs/prod/":            "",
				"extracted/configs/prod/config.yaml": prodConfig,
			},
		},
		{
			name:    "Extract with .tar.gz extension",
			sources: map[string]fs.FS{"source": src("manifests.tar.gz", appTgz)},
			copy:    extract("@source/manifests.tar.gz", "@artifact/"),
			want:    appWant,
		},
		{
			name:    "Extract with exclude patterns",
			sources: map[string]fs.FS{"source": src("app.tgz", appTgz, "int.tgz", intTgz)},
			copy: []manifest.CopyOperation{{
				From: "@source/*.tgz", To: "@artifact/",
				Strategy: manifest.CopyStrategyExtract, Exclude: []string{"int.tgz"},
			}},
			want: appWant,
		},
		{
			name:    "Extract pattern matches no tarballs",
			sources: map[string]fs.FS{"source": src("config.yaml", "key: value")},
			copy:    extract("@source/*.tgz", "@artifact/"),
			wantErr: "no files match pattern",
		},
		{
			name: "Extract overwrites existing files from previous extract",
			sources: map[string]fs.FS{"source": src(
				"first.tgz", tgz(t, "config.yaml", firstConfig),
				"second.tgz", tgz(t, "config.yaml", secondConfig),
			)},
			copy: append(extract("@source/first.tgz", "@artifact/"), extract("@source/second.tgz", "@artifact/")...),
			want: map[string]string{"config.yaml": secondConfig},
		},
		{
			name:    "Extract skips non-tarball files in glob pattern",
			sources: map[string]fs.FS{"source": src("readme.txt", "This is a readme", "archive.tgz", appTgz)},
			copy:    extract("@source/*", "@artifact/"),
			want:    appWant,
		},
		{
			name:    "Extract strategy fails for directory source",
			sources: map[string]fs.FS{"source": src("manifests/config.yaml", "key: value")},
			copy:    extract("@source/manifests", "@artifact/"),
			wantErr: "extract strategy is not supported for directories",
		},
		{
			name:    "Extract single file requires tarball extension",
			sources: map[string]fs.FS{"source": src("config.yaml", "key: value")},
			copy:    extract("@source/config.yaml", "@artifact/"),
			wantErr: "extract strategy requires tarball file",
		},
		{
			// Upstream TestBuild_ExtractStrategyRejectsDestinationTraversal.
			name:    "Extract strategy rejects destination traversal",
			sources: map[string]fs.FS{"source": src("manifests.tgz", appTgz)},
			copy:    extract("@source/manifests.tgz", "@artifact/../../escape"),
			wantErr: "destination path must stay within the artifact root",
		},
	})
}

// TestRelativeToBase_Paths covers the guard rejecting paths that escape
// basePath, which Build never produces: every walked path is under the copy
// source or the literal prefix of its glob.
func TestRelativeToBase_Paths(t *testing.T) {
	cases := []struct {
		name, basePath, filePath, want string
		wantOK                         bool
	}{
		{name: "empty base returns the path unchanged", basePath: "", filePath: "a/b", want: "a/b", wantOK: true},
		{name: "current directory base returns the path unchanged", basePath: ".", filePath: "a/b", want: "a/b", wantOK: true},
		{name: "path under base is made relative", basePath: "a", filePath: "a/b/c", want: "b/c", wantOK: true},
		{name: "trailing slash on base is cleaned", basePath: "a/", filePath: "a/b", want: "b", wantOK: true},
		{name: "path equal to base", basePath: "a/b", filePath: "a/b", want: ".", wantOK: true},
		{name: "leading dots in a name are not treated as traversal", basePath: "a", filePath: "a/..b", want: "..b", wantOK: true},
		{name: "sibling of base escapes", basePath: "a/b", filePath: "a/c"},
		{name: "parent of base escapes", basePath: "a/b", filePath: "a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := relativeToBase(tc.basePath, tc.filePath)
			assert.Equal(t, ok, tc.wantOK)
			assert.Equal(t, got, tc.want)
		})
	}
}

// TestExtractTarball_ConfinesDestination reaches extractTarball's own guard,
// which Build's destination parsing otherwise rejects first.
func TestExtractTarball_ConfinesDestination(t *testing.T) {
	stage := tree.NewBuilder(nil)
	err := extractTarball(src("manifests.tgz", "invalid"), "manifests.tgz", stage, "../escape")
	if err == nil || !strings.Contains(err.Error(), "failed to create destination directory") {
		t.Fatalf("error = %v, want it to contain %q", err, "failed to create destination directory")
	}
	got, err := stage.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	assert.Diff(t, paths(t, got), map[string]string{})
}
