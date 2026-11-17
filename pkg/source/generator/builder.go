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
internal/builder (builder.go, merge.go, extract.go) from os.Root onto
io/fs sources and an in-memory tree.Builder staging area.
*/

// Package generator builds the ExternalArtifacts of a source-watcher
// ArtifactGenerator offline, with source-watcher v2.3.1's copy semantics.
package generator

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	flux "github.com/fluxcd/pkg/sourceignore"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	"github.com/home-operations/flate/pkg/manifest"
	"github.com/home-operations/flate/pkg/tree"
)

// maxUntarSize is fluxcd/pkg/tar's DefaultMaxUntarSize, the Extract limit.
const maxUntarSize = 100 << 20

// Build applies art's copy operations, in order, from sources (by alias) and
// returns the artifact tree and its content digest. Like source-watcher's
// archive, the tree omits Flux's default source-ignore matches.
func Build(ctx context.Context, art manifest.GeneratedArtifact, sources map[string]fs.FS) (*tree.Tree, string, error) {
	stage := tree.NewBuilder(nil)
	for _, op := range art.Copy {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		if err := applyCopyOperation(ctx, op, sources, stage); err != nil {
			return nil, "", fmt.Errorf("%w: failed to apply copy operation from '%s' to '%s': %w", manifest.ErrInput, op.From, op.To, err)
		}
	}
	if err := dropIgnored(stage); err != nil {
		return nil, "", err
	}
	t, err := stage.Build()
	if err != nil {
		return nil, "", err
	}
	digest, err := contentDigest(t)
	return t, digest, err
}

// dropIgnored applies storage.SourceIgnoreFilter(nil, nil), which
// source-watcher's archive step uses.
func dropIgnored(stage *tree.Builder) error {
	matcher := flux.NewDefaultMatcher(nil, nil)
	var ignored []string
	err := stage.WalkRaw(".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || name == "." {
			return err
		}
		if matcher.Match(strings.Split(name, "/"), d.IsDir()) && !d.IsDir() {
			ignored = append(ignored, name)
		}
		return nil
	})
	for _, name := range ignored {
		err = errors.Join(err, stage.RemoveAll(name))
	}
	return err
}

// contentDigest hashes every file's path, permissions and bytes in walk order.
func contentDigest(t *tree.Tree) (string, error) {
	h := sha256.New()
	err := t.WalkRaw(".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := t.ReadFile(name)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(h, "%s\x00%o\x00%d\x00", name, d.Type()|fileMode(d).Perm(), len(data))
		_, _ = h.Write(data)
		return nil
	})
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), err
}

func fileMode(d fs.DirEntry) fs.FileMode {
	info, err := d.Info()
	if err != nil {
		return 0
	}
	return info.Mode()
}

// applyCopyOperation implements cp-like semantics: a non-glob source copies
// the file or directory it names; a glob copies each non-excluded match.
func applyCopyOperation(ctx context.Context, op manifest.CopyOperation, sources map[string]fs.FS, stage *tree.Builder) error {
	srcAlias, srcPattern, err := parseCopySource(op.From)
	if err != nil {
		return fmt.Errorf("invalid copy source '%s': %w", op.From, err)
	}
	if err := validateGlobPattern(srcPattern); err != nil {
		return fmt.Errorf("invalid copy source '%s': %w", op.From, err)
	}
	destRelPath, err := parseCopyDestinationRelative(op.To)
	if err != nil {
		return fmt.Errorf("invalid copy destination '%s': %w", op.To, err)
	}
	src, exists := sources[srcAlias]
	if !exists {
		return fmt.Errorf("source alias '%s' not found", srcAlias)
	}
	for _, pattern := range op.Exclude {
		if err := validateGlobPattern(pattern); err != nil {
			return fmt.Errorf("invalid exclude pattern '%s': %w", pattern, err)
		}
		if _, err := doublestar.Match(pattern, "."); err != nil {
			return fmt.Errorf("invalid exclude pattern '%s'", pattern)
		}
	}
	if !containsGlobChars(srcPattern) {
		return applySingleSourceCopy(ctx, op, src, srcPattern, stage, destRelPath)
	}
	var matches []string
	if op.Strategy == manifest.CopyStrategyExtract {
		matches, err = doublestar.Glob(src, srcPattern)
	} else {
		matches, err = fs.Glob(src, srcPattern)
	}
	if err != nil {
		return fmt.Errorf("invalid glob pattern '%s': %w", srcPattern, err)
	}
	if len(matches) == 0 {
		if op.Optional {
			return nil
		}
		return fmt.Errorf("no files match pattern '%s' in source '%s'", srcPattern, srcAlias)
	}
	excludeBasePath := manifest.CopySelectionRoot(srcPattern)
	filtered := slices.DeleteFunc(matches, func(match string) bool {
		return match == "." || match == ".." || shouldExcludePath(match, excludeBasePath, op.Exclude)
	})
	if len(filtered) == 0 {
		if op.Optional {
			return nil
		}
		return fmt.Errorf("all files matching pattern '%s' in source '%s' were excluded", srcPattern, srcAlias)
	}
	for _, match := range filtered {
		if err := ctx.Err(); err != nil {
			return err
		}
		if op.Strategy == manifest.CopyStrategyExtract {
			if !isTarball(match) {
				continue
			}
			if err := extractTarball(src, match, stage, destRelPath); err != nil {
				return fmt.Errorf("failed to extract tarball '%s' to '%s': %w", match, destRelPath, err)
			}
			continue
		}
		destFile := calculateGlobDestination(srcPattern, match, destRelPath)
		if err := copyFile(ctx, op, copyPaths{src: src, srcPath: match, stage: stage, destPath: destFile}, excludeBasePath); err != nil {
			return fmt.Errorf("failed to copy file '%s' to '%s': %w", match, destFile, err)
		}
	}
	return nil
}

// copyPaths names one copy's source and staged destination.
type copyPaths struct {
	src      fs.FS
	srcPath  string
	stage    *tree.Builder
	destPath string
}

// applySingleSourceCopy copies a file to dest (or into dest when dest ends in
// '/' or is a staged directory) and a directory to dest/<dirname>.
func applySingleSourceCopy(ctx context.Context, op manifest.CopyOperation, src fs.FS, srcPath string, stage *tree.Builder, destPath string) error {
	srcPath = path.Clean(srcPath)
	srcInfo, err := statSource(src, srcPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if op.Optional {
				return nil
			}
			return fmt.Errorf("source '%s' does not exist", srcPath)
		}
		return fmt.Errorf("failed to stat source '%s': %w", srcPath, err)
	}
	p := copyPaths{src: src, srcPath: srcPath, stage: stage}
	if srcInfo.IsDir() {
		if op.Strategy == manifest.CopyStrategyExtract {
			return fmt.Errorf("extract strategy is not supported for directories, got '%s'", srcPath)
		}
		p.destPath = path.Join(destPath, path.Base(srcPath))
		return copyFile(ctx, op, p, srcPath)
	}
	if shouldExcludePath(srcPath, ".", op.Exclude) {
		return nil
	}
	if op.Strategy == manifest.CopyStrategyExtract {
		if !isTarball(srcPath) {
			return fmt.Errorf("extract strategy requires tarball file (.tar.gz or .tgz), got '%s'", srcPath)
		}
		return extractTarball(src, srcPath, stage, destPath)
	}
	p.destPath = destPath
	if strings.HasSuffix(op.To, "/") {
		p.destPath = path.Join(destPath, path.Base(srcPath))
	} else if info, err := stage.Stat(destPath); err == nil && info.IsDir() {
		p.destPath = path.Join(destPath, path.Base(srcPath))
	}
	return copyFile(ctx, op, p, ".")
}

// statSource stats name; a name fs.FS cannot address ("/x", "../x") fails
// as os.Root does for a path escaping the source.
func statSource(src fs.FS, name string) (fs.FileInfo, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrInvalid}
	}
	return fs.Stat(src, name)
}

func containsGlobChars(p string) bool {
	return strings.ContainsAny(p, "*?[]")
}

// maxGlobAlternationCommas bounds glob alternation expansion: doublestar
// re-matches once per alternative, so cost is the product of group widths.
const maxGlobAlternationCommas = 20

// validateGlobPattern bounds a pattern's alternation expansion. Only commas
// inside a '{}' group count; a backslash escapes the next byte. MUST run
// before any matcher, including doublestar.Match.
func validateGlobPattern(pattern string) error {
	var commas, depth int
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '\\':
			i++
		case '{':
			depth++
		case '}':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth > 0 {
				commas++
			}
		}
	}
	if commas > maxGlobAlternationCommas {
		return fmt.Errorf("pattern has %d alternatives in '{}' groups, at most %d are allowed",
			commas, maxGlobAlternationCommas)
	}
	return nil
}

// calculateGlobDestination strips the directory prefix of a 'dir/**' pattern
// from its matches and keeps the full match path for every other pattern.
func calculateGlobDestination(pattern, match, destPath string) string {
	if dirPrefix, ok := strings.CutSuffix(pattern, "/**"); ok {
		if relativeMatch, ok := strings.CutPrefix(match, dirPrefix+"/"); ok {
			return path.Join(destPath, relativeMatch)
		}
	}
	return path.Join(destPath, match)
}

func parseCopySource(from string) (alias, pattern string, err error) {
	rest, ok := strings.CutPrefix(from, "@")
	if !ok {
		return "", "", errors.New("source must start with '@'")
	}
	alias, pattern, ok = strings.Cut(rest, "/")
	if !ok {
		return "", "", errors.New("source format must be '@alias/pattern'")
	}
	return alias, pattern, nil
}

func parseCopyDestinationRelative(to string) (string, error) {
	destPath, ok := strings.CutPrefix(to, "@artifact/")
	if !ok {
		return "", errors.New("destination must start with '@artifact/'")
	}
	if destPath == "" {
		return ".", nil
	}
	if !filepath.IsLocal(destPath) {
		return "", errors.New("destination path must stay within the artifact root")
	}
	if slices.Contains(strings.Split(filepath.ToSlash(destPath), "/"), "..") {
		return "", errors.New("destination path must not contain '..'")
	}
	return path.Clean(destPath), nil
}

// copyFile copies a file or, recursively, a directory; Merge merges a YAML
// file into an existing staged destination.
func copyFile(ctx context.Context, op manifest.CopyOperation, p copyPaths, excludeBasePath string) error {
	info, err := statSource(p.src, p.srcPath)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return copyDir(ctx, p, op.Exclude, excludeBasePath)
	}
	if _, err := p.stage.Stat(p.destPath); err == nil && op.Strategy == manifest.CopyStrategyMerge {
		return mergeFile(p)
	}
	return copyRegularFile(p, info.Mode())
}

func copyRegularFile(p copyPaths, mode fs.FileMode) error {
	data, err := fs.ReadFile(p.src, p.srcPath)
	if err != nil {
		return err
	}
	if dir := path.Dir(p.destPath); dir != "." {
		if err := p.stage.AddDir(dir, 0o755); err != nil {
			return fmt.Errorf("failed to create destination directory: %w", err)
		}
	}
	return p.stage.AddFile(p.destPath, data, mode.Perm())
}

// mergeFile deep-merges the source YAML into the staged destination, maps
// recursively and every other value (arrays included) replaced, as Helm
// merges values files.
func mergeFile(p copyPaths) error {
	srcData, err := loadYAML(p.src, p.srcPath)
	if err != nil {
		return err
	}
	destData, err := loadYAML(p.stage, p.destPath)
	if err != nil {
		return err
	}
	merged, err := yaml.Marshal(mergeMap(destData, srcData))
	if err != nil {
		return fmt.Errorf("failed to merged YAML: %w", err)
	}
	return p.stage.AddFile(p.destPath, merged, 0o644)
}

// loadYAML merges every document of a multi-document YAML file into one map.
func loadYAML(fsys fs.FS, name string) (map[string]any, error) {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	for {
		raw, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("error reading YAML document: %w", err)
		}
		doc := map[string]any{}
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("cannot unmarshal YAML document: %w", err)
		}
		out = mergeMap(out, doc)
	}
}

func mergeMap(base, overlay map[string]any) map[string]any {
	out := maps.Clone(base)
	if out == nil {
		out = map[string]any{}
	}
	for k, v := range overlay {
		if v, ok := v.(map[string]any); ok {
			if bv, ok := out[k].(map[string]any); ok {
				out[k] = mergeMap(bv, v)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// copyDir copies p.srcPath's tree to p.destPath, skipping excluded entries.
// Files inside a directory are always overwritten, never merged.
func copyDir(ctx context.Context, p copyPaths, excludes []string, excludeBasePath string) error {
	return fs.WalkDir(p.src, p.srcPath, func(name string, d fs.DirEntry, err error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(name, p.srcPath), "/")
		if p.srcPath == "." {
			rel = name
		}
		if name == p.srcPath {
			return addDir(p.stage, p.destPath)
		}
		if shouldExcludePath(name, excludeBasePath, excludes) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		dest := path.Join(p.destPath, rel)
		if d.IsDir() {
			return addDir(p.stage, dest)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return copyRegularFile(copyPaths{src: p.src, srcPath: name, stage: p.stage, destPath: dest}, info.Mode())
	})
}

func addDir(stage *tree.Builder, name string) error {
	if name == "." {
		return nil
	}
	return stage.AddDir(name, 0o755)
}

// shouldExcludePath matches excludes against both the source-root-relative
// path and the path relative to the operation's selected source root.
func shouldExcludePath(filePath, basePath string, excludes []string) bool {
	if shouldExclude(filePath, excludes) {
		return true
	}
	relPath, ok := relativeToBase(basePath, filePath)
	if !ok || relPath == "." || relPath == filePath {
		return false
	}
	return shouldExclude(relPath, excludes)
}

func relativeToBase(basePath, filePath string) (string, bool) {
	basePath, filePath = path.Clean(basePath), path.Clean(filePath)
	if basePath == "." {
		return filePath, true
	}
	if filePath == basePath {
		return ".", true
	}
	if rel, ok := strings.CutPrefix(filePath, basePath+"/"); ok {
		return rel, true
	}
	return "", false
}

// shouldExclude matches each pattern against the path and, for a pattern
// without '/', against the file name at any depth.
func shouldExclude(filePath string, excludes []string) bool {
	fileName := path.Base(filePath)
	for _, pattern := range excludes {
		// Patterns are validated before any copy runs.
		if doublestar.MatchUnvalidated(pattern, filePath) ||
			(!strings.Contains(pattern, "/") && doublestar.MatchUnvalidated(pattern, fileName)) {
			return true
		}
	}
	return false
}

// isTarball reports a .tar.gz or .tgz name, case-insensitively.
func isTarball(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz")
}

// extractTarball unpacks the gzip tarball at srcPath under destPath.
func extractTarball(src fs.FS, srcPath string, stage *tree.Builder, destPath string) error {
	f, err := src.Open(srcPath)
	if err != nil {
		return fmt.Errorf("failed to open tarball %q: %w", srcPath, err)
	}
	defer func() { _ = f.Close() }()
	if err := addDir(stage, destPath); err != nil {
		return fmt.Errorf("failed to create destination directory %q: %w", destPath, err)
	}
	if err := untar(f, stage, destPath); err != nil {
		return fmt.Errorf("failed to extract tarball %q to %q: %w", srcPath, destPath, err)
	}
	return nil
}

// untar applies fluxcd/pkg/tar Untar's rules: at most maxUntarSize bytes,
// entry names that stay inside the destination, regular files and
// directories only.
func untar(r io.Reader, stage *tree.Builder, destPath string) error {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("requires gzip-compressed body: %w", err)
	}
	tr := tar.NewReader(zr)
	var processed int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return zr.Close()
		}
		if err != nil {
			return fmt.Errorf("tar error: %w", err)
		}
		processed += h.Size
		if processed > maxUntarSize {
			return fmt.Errorf("tar %q is bigger than max archive size of %d bytes", h.Name, maxUntarSize)
		}
		if h.Name == "" || strings.Contains(h.Name, `\`) || strings.HasPrefix(h.Name, "/") || strings.Contains(h.Name, "../") {
			return fmt.Errorf("tar contained invalid name error %q", h.Name)
		}
		name := path.Join(destPath, h.Name) //nolint:gosec // G305: h.Name is rejected above when it could leave destPath.
		switch mode := h.FileInfo().Mode(); {
		case mode.IsRegular():
			data, err := io.ReadAll(io.LimitReader(tr, h.Size))
			if err != nil {
				return fmt.Errorf("error copying buffer: %w", err)
			}
			if err := addDir(stage, path.Dir(name)); err != nil {
				return err
			}
			if err := stage.AddFile(name, data, mode.Perm()); err != nil {
				return err
			}
		case mode.IsDir():
			if err := addDir(stage, name); err != nil {
				return err
			}
		case mode&fs.ModeSymlink != 0:
			return fmt.Errorf("tar file entry %s is a symlink, which is not allowed in this context", h.Name)
		default:
			return fmt.Errorf("tar file entry %s contained unsupported file type %v", h.Name, mode)
		}
	}
}
