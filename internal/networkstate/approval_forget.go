package networkstate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/AndrewDryga/coop/internal/fsidentity"
)

// ApprovalRecord is one remembered approval and the id it is filed under. It is
// what `coop net forget` shows before it removes anything.
type ApprovalRecord struct {
	// Approval is nil when nothing is remembered for this path.
	Approval *Approval
	// Path is the project path as the id was derived from it, and ID is that
	// derivation.
	Path string
	ID   string
	// old are the files an older coop filed for this project (old_records.go), which Forget removes.
	old []string
}

// OldFiles are the records an older coop filed for this project that Forget also removes.
func (r ApprovalRecord) OldFiles() []string { return slices.Clone(r.old) }

// ApprovalAt reads what is remembered for a project path WITHOUT requiring the
// directory to exist — the one case Approval cannot serve, and exactly the case
// a deleted checkout leaves behind.
//
// It is for REMOVING an approval, never for authorizing a launch: it proves
// nothing about the directory standing at that path now, which is the whole
// point of projectIdentity's stat and Approval.checkDirectory.
//
// The id derivation is LEXICAL. canonicalPath resolves the ancestors that still
// exist and appends the rest as written, so an ordinary directory yields the
// same id whether it is there or not. A path whose LEAF was a symlink does not:
// the approval was filed under the directory it pointed at, so once the link is
// gone this derivation finds nothing — and finding nothing is the right answer,
// because the alternative is removing some other project's approval.
func (s *Store) ApprovalAt(project string) (ApprovalRecord, error) {
	if err := s.intactAuthority(); err != nil {
		return ApprovalRecord{}, err
	}
	canonical, err := canonicalPath(project)
	if err != nil {
		return ApprovalRecord{}, err
	}
	id := s.projectKey(canonical)
	record := ApprovalRecord{Path: canonical, ID: id}
	found, err := s.oldRecordsFor(canonical)
	if err != nil {
		return ApprovalRecord{}, err
	}
	if found != nil {
		record.old = append(found.Files, found.anchors...)
	}
	// An approval under this project's own id that an older coop wrote cannot be read, and is
	// among the files Forget removes.
	if record.Approval, err = s.approval(id); err != nil && !slices.Contains(record.old, approvalRecord(id)) {
		return ApprovalRecord{}, err
	}
	return record, nil
}

// Forget removes the ONE approval a record names, reporting whether a record
// was actually there. It is deliberately not a sweep: an approval is filed under
// a hash of the project path and stores no path, so this store can answer
// "is this project approved?" and never "which projects are approved?".
//
// There is no review digest here, unlike Approve: forgetting fails safe, since a
// launch with no approval asks for one. What it must never do is report success
// for a record it could not locate, so removal is proven, not assumed.
func (s *Store) Forget(ctx context.Context, record ApprovalRecord) (bool, error) {
	if !lowerHex(record.ID, 64) {
		return false, errors.New("forgetting a network approval needs the record it was read from")
	}
	if err := s.intactAuthority(); err != nil {
		return false, err
	}
	removed := false
	err := s.lockRecord(ctx, "approval", record.ID, func() error {
		// ApprovalAt is a review snapshot, not the object to clean up after this lock is acquired.
		// A concurrent reapproval can replace its anchor while the operator is confirming. Re-read
		// under the record lock so deleting the current grant cannot orphan the current anchor.
		name := approvalRecord(record.ID)
		current, err := s.approval(record.ID)
		if err != nil && !slices.Contains(record.old, name) {
			return err
		}
		if err != nil {
			current = nil
		}
		// The barrier goes down FIRST. Removing the grant is what
		// makes this project's mode fall back to coop's built-in default, so the
		// order is the guarantee: after this line no ordinary launch of this
		// project starts until a human approves again.
		if err := s.recordWithdrawal(record.ID); err != nil {
			return err
		}
		// Retire the anchor while its approval record still names it. If the process dies here,
		// withdrawal keeps launches closed and the retained record lets the next retry finish
		// cleanup. Removing the record first would turn this into an uncollectable private link.
		if err := s.retireProjectAnchor(record.Path, current); err != nil {
			return fmt.Errorf("retire project approval identity: %w", err)
		}
		if err := s.retireOldMarker(record.Path); err != nil {
			return fmt.Errorf("retire project approval identity: %w", err)
		}
		// What an older coop filed for this project goes after the barrier, which replaced any old
		// withdrawal under this project's own id: that one stays.
		for _, old := range record.old {
			if old == withdrawalRecord(record.ID) {
				continue
			}
			if err := s.removeRecord(old); err != nil {
				return err
			}
			removed = true
		}
		err = s.root.Remove(name)
		if errors.Is(err, os.ErrNotExist) {
			return s.checkRootPath()
		}
		if err != nil {
			return err
		}
		if _, err := s.root.Lstat(name); !errors.Is(err, os.ErrNotExist) {
			return errors.New("this approval could not be proven removed")
		}
		if err := s.checkRootPath(); err != nil {
			return err
		}
		removed = true
		return nil
	})
	if err != nil {
		return false, err
	}
	if err := s.intactAuthority(); err != nil {
		return removed, err
	}
	if !removed {
		return false, nil // nothing changed that a running session has to hear about
	}
	// After the withdrawal is in place, as in Approve: a running editor session admits again and
	// finds this project withdrawn.
	return true, s.announceApprovalChange(ctx)
}

// retireOldMarker removes the checkout's marker and private anchor when an older coop's approval
// made them, while the anchor file still exists to prove the pair. Left behind, the marker would
// name a checkout the next approval cannot re-enroll from.
func (s *Store) retireOldMarker(project string) error {
	keyed := s.keyedProjectID(project)
	if keyed == "" {
		return nil
	}
	body, err := fsidentity.ReadMarker(project, ProjectApprovalMarker)
	if err != nil {
		return nil
	}
	id, anchor, err := parseAnyProjectAnchorBody(body)
	if err != nil || id != keyed {
		return nil
	}
	if _, err := s.root.Lstat(projectAnchorName(id, anchor)); err != nil {
		return nil
	}
	return fsidentity.Retire(s.projectAnchorBinding(project, id, anchor))
}
