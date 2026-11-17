package generator

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"

	"github.com/home-operations/flate/pkg/source/sourceignore"
	"github.com/home-operations/flate/pkg/store"
)

// SourceFS returns the files source-watcher would see for a source artifact:
// its acquired filesystem, or the regular files and directories of its local
// tree. A local tree with neither revision nor digest is the working tree,
// which MUST be filtered like a source-controller archive (.sourceignore plus
// Flux defaults), exactly as Kustomization renders of it are.
func SourceFS(sa *store.SourceArtifact) (fs.FS, error) {
	if sa.FS != nil {
		return sa.FS, nil
	}
	if sa.LocalPath == "" {
		return nil, errors.New("source artifact has no content")
	}
	local := localFS{FS: os.DirFS(sa.LocalPath)}
	if sa.Digest == "" && sa.Revision == "" {
		m, err := sourceignore.New(sa.LocalPath, nil, true)
		if err != nil {
			return nil, err
		}
		local.ignore = m
	}
	return local, nil
}

// localFS hides what a Flux source archive omits: symlinks, other
// non-regular files, and ignored paths.
type localFS struct {
	fs.FS
	ignore *sourceignore.Matcher
}

func (l localFS) hidden(name string, mode fs.FileMode) bool {
	if !mode.IsRegular() && !mode.IsDir() {
		return true
	}
	return l.ignore != nil && name != "." && l.ignore.Match(filepath.FromSlash(name), mode.IsDir())
}

// visible reports whether name and every parent directory are visible.
func (l localFS) visible(name string) error {
	for p := name; p != "."; p = path.Dir(p) {
		info, err := fs.Lstat(l.FS, p)
		if err != nil {
			return err
		}
		if l.hidden(p, info.Mode()) {
			return &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
		}
	}
	return nil
}

// Open implements fs.FS.
func (l localFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	if err := l.visible(name); err != nil {
		return nil, err
	}
	return l.FS.Open(name)
}

// Stat implements fs.StatFS.
func (l localFS) Stat(name string) (fs.FileInfo, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrInvalid}
	}
	if err := l.visible(name); err != nil {
		return nil, err
	}
	return fs.Stat(l.FS, name)
}

// ReadDir implements fs.ReadDirFS.
func (l localFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrInvalid}
	}
	if err := l.visible(name); err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(l.FS, name)
	return slices.DeleteFunc(entries, func(e fs.DirEntry) bool {
		return l.hidden(path.Join(name, e.Name()), e.Type())
	}), err
}
