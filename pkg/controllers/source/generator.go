package source

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/home-operations/flate/pkg/depwait"
	"github.com/home-operations/flate/pkg/manifest"
	"github.com/home-operations/flate/pkg/source/generator"
	"github.com/home-operations/flate/pkg/store"
)

// reconcileGenerator builds every ExternalArtifact of a source-watcher
// ArtifactGenerator from its sources and publishes each one's status and
// artifact under the ExternalArtifact id; no ExternalArtifact object is
// emitted, as none exists in the rendered tree. A source still absent at the
// structural fixpoint lies outside the rendered tree: its artifacts are
// skipped with a warning, so their consumers are skipped visibly. Every other
// failure fails the artifacts, and their consumers with them.
func (c *Controller) reconcileGenerator(ctx context.Context, raw *manifest.RawObject, level manifest.DrainLevel) error {
	g, err := manifest.ParseArtifactGenerator(raw)
	if err != nil {
		c.settleArtifacts(manifest.ProducerTargets(raw), err)
		return err
	}
	targets := make([]manifest.ProducerTarget, len(g.Artifacts))
	for i, a := range g.Artifacts {
		targets[i] = manifest.ProducerTarget{NamedResource: a.ID}
		c.SetPendingUnlessReady(a.ID, "generating")
	}
	deps := make([]manifest.DependencyRef, len(g.Sources))
	for i, s := range g.Sources {
		deps[i] = manifest.DependencyRef{NamedResource: s.Ref}
		if level >= manifest.DrainCascade && c.outsideTree(s.Ref) {
			err := fmt.Errorf("%w: source %s is not in the rendered tree", manifest.ErrSourceSkipped, s.Ref)
			c.settleArtifacts(targets, err)
			return err
		}
	}
	if err := c.Require(ctx, g.ID, deps, level); err != nil {
		if _, blocked := errors.AsType[*depwait.ErrBlocked](err); !blocked {
			c.settleArtifacts(targets, err)
		}
		return err
	}
	sources := make(map[string]fs.FS, len(g.Sources))
	for _, s := range g.Sources {
		if err := c.SourceSkipped(s.Ref, "source"); err != nil {
			c.settleArtifacts(targets, err)
			return err
		}
		sa, ok := c.Store.GetArtifact(s.Ref).(*store.SourceArtifact)
		if !ok {
			err := fmt.Errorf("%w: source %s has no artifact", manifest.ErrObjectNotFound, s.Ref)
			c.settleArtifacts(targets, err)
			return err
		}
		if sources[s.Alias], err = generator.SourceFS(sa); err != nil {
			err = fmt.Errorf("%w: source %s: %w", manifest.ErrInput, s.Ref, err)
			c.settleArtifacts(targets, err)
			return err
		}
	}
	var errs []error
	for _, a := range g.Artifacts {
		var t fs.FS
		var digest string
		c.Tasks.YieldSlot(func() { t, digest, err = generator.Build(ctx, a, sources) })
		if err != nil {
			err = fmt.Errorf("ExternalArtifact %s: %w", a.ID.NamespacedName(), err)
			c.settleArtifacts([]manifest.ProducerTarget{{NamedResource: a.ID}}, err)
			errs = append(errs, err)
			continue
		}
		c.Store.SetArtifact(a.ID, &store.SourceArtifact{
			Kind: manifest.KindExternalArtifact, FS: t,
			Key:      "artifactgenerator-" + digest,
			Revision: "latest@" + digest, Digest: digest,
		})
		c.Store.UpdateStatus(a.ID, store.StatusReady, "")
	}
	return errors.Join(errs...)
}

// outsideTree reports a source the store has never seen.
func (c *Controller) outsideTree(id manifest.NamedResource) bool {
	_, hasStatus := c.Store.GetStatus(id)
	return c.Store.GetObject(id) == nil && c.Store.GetArtifact(id) == nil && !hasStatus
}

// settleArtifacts records err as the terminal status of every target: a skip
// as a warned Ready skip, anything else as a failure.
func (c *Controller) settleArtifacts(targets []manifest.ProducerTarget, err error) {
	for _, t := range targets {
		if !errors.Is(err, manifest.ErrSourceSkipped) {
			c.Store.UpdateFailure(t.NamedResource, err)
			continue
		}
		msg := manifest.TrimSentinelPrefix(err.Error())
		c.Store.UpdateStatus(t.NamedResource, store.StatusReady, store.SkippedPrefix+" "+msg)
		c.Store.AddWarning(manifest.Warning{
			Resource: t.NamedResource, Category: manifest.WarnUnrenderedArtifact,
			Message: "ExternalArtifact not generated offline: " + msg + "; its consumers are skipped",
		})
	}
}
