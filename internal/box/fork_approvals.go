package box

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/fsidentity"
	"github.com/AndrewDryga/coop/internal/networkstate"
)

// SnapshotForkApprovals is a host-only creation step, before the new checkout is published or
// mounted. It copies grants, never project requests, and is never called when reopening a fork.
func SnapshotForkApprovals(ctx context.Context, cfg *config.Config, parent, fork string) (err error) {
	parent, err = canonicalProjectDir(parent)
	if err != nil {
		return err
	}
	fork, err = canonicalProjectDir(fork)
	if err != nil {
		return err
	}
	if SameFolder(parent, fork) {
		return errors.New("approval snapshot requires a new fork directory")
	}
	exposed, err := networkExposureRoots(cfg, RunSpec{Repo: fork, PolicyRepo: parent})
	if err != nil {
		return err
	}
	root, err := NetworkStatePath()
	if err != nil {
		return err
	}
	store, err := networkstate.OpenExisting(root, exposed)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if store != nil {
		defer store.Close()
	}
	serviceRoot, err := serviceApprovalRoot()
	if err != nil {
		return err
	}
	if err := networkstate.CheckPathExposure(serviceRoot, exposed); err != nil {
		return err
	}
	// An approval may re-enroll a parent marker only under the exclusive side. Reading its
	// existing grants takes the shared side, alongside boxes already using that same identity.
	unlock, err := forkspace.LockServiceLaunch(ctx, parent, false)
	if err != nil {
		return err
	}
	defer unlock()
	undo, err := snapshotForkServiceApprovals(parent, fork, serviceRoot)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, undo())
		}
	}()
	if store == nil {
		return nil
	}
	if err := forkspace.ExcludeIfRepository(fork, "/"+networkstate.ProjectApprovalMarker); err != nil {
		return err
	}
	return store.SnapshotForkApprovals(ctx, parent, fork, ComposeProject(fork)+"-filtered")
}

func snapshotForkServiceApprovals(parent, fork, root string) (undo func() error, err error) {
	undo = func() error { return nil }
	if _, err := os.Lstat(filepath.Join(fork, serviceApprovalMarker)); !errors.Is(err, os.ErrNotExist) {
		return undo, errors.Join(errors.New("new fork already has a service approval marker"), err)
	}
	anchor, err := serviceApprovalAnchor(parent, false)
	if errors.Is(err, os.ErrNotExist) {
		return undo, nil
	}
	if err != nil {
		return undo, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return undo, err
	}
	var approvals []ServiceApproval
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, entry.Name()))
		if err != nil {
			return undo, err
		}
		var approval ServiceApproval
		if json.Unmarshal(raw, &approval) != nil || approval.Version != serviceApprovalVersion || approval.Anchor != anchor {
			continue
		}
		if entry.Name() != serviceApprovalKey(anchor, approval.File, approval.Digest)+".json" ||
			!filepath.IsLocal(approval.File) || len(approval.Images) == 0 || !validServiceImagePins(approval.Images) {
			return undo, errors.New("parent service approval is invalid")
		}
		approvals = append(approvals, approval)
	}
	if len(approvals) == 0 {
		return undo, nil
	}
	forkAnchor, err := serviceApprovalAnchor(fork, true)
	if err != nil {
		return undo, err
	}
	var written []string
	undo = func() error {
		var errs []error
		for _, path := range written {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
		}
		private, err := os.OpenRoot(root)
		if err != nil {
			return errors.Join(append(errs, err)...)
		}
		defer private.Close()
		return errors.Join(append(errs, fsidentity.Retire(serviceAnchorBinding(fork, forkAnchor, private)))...)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, undo())
		}
	}()
	for _, approval := range approvals {
		// Paths are repository-relative. Image IDs, volume objects and the original Compose
		// digest stay exactly as reviewed; normal startup revalidates them against this fork.
		approval.Anchor, approval.Workspace = forkAnchor, fork
		key := serviceApprovalKey(forkAnchor, approval.File, approval.Digest)
		written = append(written, filepath.Join(root, key+".json"))
		if err := writeServiceApproval(root, key, approval); err != nil {
			return undo, fmt.Errorf("snapshot fork service approval: %w", err)
		}
	}
	return undo, nil
}
