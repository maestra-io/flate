package manifest

import (
	"cmp"
	"fmt"
	"path"
	"slices"
	"strings"
)

// ArtifactGeneratorGroup is source-watcher's API group.
const ArtifactGeneratorGroup = "source.extensions.fluxcd.io"

// Copy strategies of a source-watcher copy operation.
const (
	CopyStrategyOverwrite = "Overwrite"
	CopyStrategyMerge     = "Merge"
	CopyStrategyExtract   = "Extract"
)

// ArtifactGenerator is the offline view of a source-watcher ArtifactGenerator:
// the sources it reads and the ExternalArtifacts it composes from them.
type ArtifactGenerator struct {
	ID        NamedResource
	Sources   []GeneratorSource
	Artifacts []GeneratedArtifact
}

// GeneratorSource binds a copy-operation alias to a Flux source.
type GeneratorSource struct {
	Alias string
	Ref   NamedResource
}

// GeneratedArtifact is one ExternalArtifact and the copy operations that build it.
type GeneratedArtifact struct {
	ID   NamedResource
	Copy []CopyOperation
}

// CopyOperation is one spec.artifacts[].copy entry.
type CopyOperation struct {
	From     string
	To       string
	Exclude  []string
	Strategy string
	Optional bool
}

// SourceSelection splits From into its source alias and selection root: the
// literal path before the first glob segment, "." when there is none. A
// malformed From yields an empty alias.
func (op CopyOperation) SourceSelection() (alias, root string) {
	alias, pattern, ok := strings.Cut(strings.TrimPrefix(op.From, "@"), "/")
	if !ok || !strings.HasPrefix(op.From, "@") {
		return "", ""
	}
	return alias, CopySelectionRoot(pattern)
}

// CopySelectionRoot returns the literal prefix of a copy pattern before its
// first glob segment ("*?[]"), or "." when the first segment is a glob.
func CopySelectionRoot(pattern string) string {
	var parts []string
	for part := range strings.SplitSeq(path.Clean(pattern), "/") {
		if strings.ContainsAny(part, "*?[]") {
			break
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return "."
	}
	return path.Join(parts...)
}

// IsArtifactGenerator reports whether obj is a source-watcher ArtifactGenerator
// (kind and API group), which flate keeps as a RawObject.
func IsArtifactGenerator(obj BaseManifest) bool {
	raw, ok := obj.(*RawObject)
	return ok && raw.Kind == KindArtifactGenerator && strings.HasPrefix(raw.APIVersion, ArtifactGeneratorGroup+"/")
}

// ParseArtifactGenerator reads raw's spec. spec.pathPattern is rejected: the
// artifacts it would generate depend on a directory scan flate does not model.
// Each artifact lands in spec.artifacts[].namespace, defaulting to raw's.
func ParseArtifactGenerator(raw *RawObject) (*ArtifactGenerator, error) {
	g := &ArtifactGenerator{ID: raw.Named()}
	fail := func(format string, args ...any) (*ArtifactGenerator, error) {
		return nil, fmt.Errorf("%w: ArtifactGenerator %s: %s", ErrInput, g.ID.NamespacedName(), fmt.Sprintf(format, args...))
	}
	spec := specView{raw.Spec}
	if spec.str("pathPattern") != "" {
		return fail("spec.pathPattern is not supported offline")
	}
	for i, e := range spec.entries("sources") {
		s := GeneratorSource{Alias: e.str("alias"), Ref: NamedResource{
			Kind: e.str("kind"), Namespace: cmp.Or(e.str("namespace"), raw.Namespace), Name: e.str("name")}}
		switch {
		case s.Alias == "" || s.Ref.Name == "":
			return fail("spec.sources[%d] needs alias and name", i)
		case !slices.Contains([]string{KindGitRepository, KindOCIRepository, KindBucket, KindHelmChart, KindExternalArtifact}, s.Ref.Kind):
			return fail("spec.sources[%d] has unsupported kind %q", i, s.Ref.Kind)
		case slices.ContainsFunc(g.Sources, func(o GeneratorSource) bool { return o.Alias == s.Alias }):
			return fail("duplicate source alias %q", s.Alias)
		}
		g.Sources = append(g.Sources, s)
	}
	for i, e := range spec.entries("artifacts") {
		a := GeneratedArtifact{ID: NamedResource{Kind: KindExternalArtifact,
			Namespace: cmp.Or(e.str("namespace"), raw.Namespace), Name: e.str("name")}}
		if a.ID.Name == "" {
			return fail("spec.artifacts[%d] needs a name", i)
		}
		for j, c := range e.entries("copy") {
			op := CopyOperation{From: c.str("from"), To: c.str("to"), Strategy: c.str("strategy"), Optional: c.boolean("optional")}
			for _, x := range c.list("exclude") {
				s, ok := x.(string)
				if !ok {
					return fail("spec.artifacts[%d].copy[%d].exclude must list strings", i, j)
				}
				op.Exclude = append(op.Exclude, s)
			}
			if !slices.Contains([]string{"", CopyStrategyOverwrite, CopyStrategyMerge, CopyStrategyExtract}, op.Strategy) {
				return fail("spec.artifacts[%d].copy[%d] has unsupported strategy %q", i, j, op.Strategy)
			}
			a.Copy = append(a.Copy, op)
		}
		if len(a.Copy) == 0 {
			return fail("spec.artifacts[%d] has no copy operations", i)
		}
		g.Artifacts = append(g.Artifacts, a)
	}
	return g, nil
}

// entries reads the list of maps at key.
func (s specView) entries(key string) []specView {
	var out []specView
	for _, e := range s.list(key) {
		m, _ := e.(map[string]any)
		out = append(out, specView{m})
	}
	return out
}

// list reads the list at key (nil if absent or not a list).
func (s specView) list(key string) []any {
	l, _ := s.m[key].([]any)
	return l
}

// boolean reads the bool at key (false if absent or not a bool).
func (s specView) boolean(key string) bool {
	b, _ := s.m[key].(bool)
	return b
}
