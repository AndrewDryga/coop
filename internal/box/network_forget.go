package box

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/AndrewDryga/coop/internal/networkstate"
)

// ProjectNetworkForget is the host-only capability behind `coop net forget`:
// the approval one project has remembered, and the single removal that consumes
// it. A nil Approval means nothing is remembered for that path — which is an
// answer, not a failure.
type ProjectNetworkForget struct {
	store  *networkstate.Store
	record networkstate.ApprovalRecord
	gone   bool
	used   bool
}

// ReviewProjectNetworkForget reads what a project remembered so an operator can
// see it before it goes. The directory is NOT required to exist: a deleted
// checkout is precisely the record nothing else can reach, so this reads through
// the store's lexical derivation instead of the project loader every other
// network verb uses.
//
// It opens the store with OpenExisting: forgetting must never be how an owner
// key is born, and a project that is gone has no mounts left to check exposure
// against — removing an approval cannot widen anything.
func ReviewProjectNetworkForget(project string) (_ *ProjectNetworkForget, err error) {
	if !filepath.IsAbs(project) {
		return nil, errors.New("forgetting a network approval needs the project's absolute path")
	}
	project = filepath.Clean(project)
	info, statErr := os.Stat(project)
	gone := statErr != nil || !info.IsDir()
	root, err := NetworkStatePath()
	if err != nil {
		return nil, err
	}
	store, err := networkstate.OpenExisting(root, nil)
	// A host that never approved anything has no records to remove, and reading
	// that fact must not create the store that would then describe it.
	if errors.Is(err, fs.ErrNotExist) {
		return &ProjectNetworkForget{record: networkstate.ApprovalRecord{Path: project}, gone: gone}, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, store.Close())
		}
	}()
	record, err := store.ApprovalAt(project)
	if err != nil {
		return nil, err
	}
	return &ProjectNetworkForget{store: store, record: record, gone: gone}, nil
}

// Project is the path the record is keyed by, canonical where the filesystem
// could still resolve it.
func (f *ProjectNetworkForget) Project() string { return f.record.Path }

// Approval is what would be removed, nil when nothing is remembered here.
func (f *ProjectNetworkForget) Approval() *networkstate.Approval { return f.record.Approval }

// Gone reports that the project directory itself is no longer there, so the
// remembered approval is all that is left to remove.
func (f *ProjectNetworkForget) Gone() bool { return f.gone }

// Commit removes the approval. A record that could not be located is an error,
// never a success: a forget that removed nothing must not read as one that did.
func (f *ProjectNetworkForget) Commit(ctx context.Context) error {
	if f == nil || f.store == nil || f.used || f.record.Approval == nil {
		return errors.New("there is no approval to forget for this project")
	}
	f.used = true
	removed, err := f.store.Forget(ctx, f.record)
	if err != nil {
		return err
	}
	if !removed {
		return errors.New("the approval for " + f.record.Path + " was already gone — nothing was removed")
	}
	return nil
}

func (f *ProjectNetworkForget) Close() error {
	if f == nil || f.store == nil {
		return nil
	}
	store := f.store
	f.store = nil
	return store.Close()
}
