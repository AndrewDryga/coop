package networkstate

import (
	"context"
	"errors"
	"os"
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
}

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
	approval, err := s.approval(id)
	if err != nil {
		return ApprovalRecord{}, err
	}
	return ApprovalRecord{Approval: approval, Path: canonical, ID: id}, nil
}

// Forget removes the ONE approval a record names, reporting whether a record
// was actually there. It is deliberately not a sweep: an approval is filed under
// a keyed hash of the project path and stores no path, so this store can answer
// "is this project approved?" and never "which projects are approved?" — and no
// record written before today could answer it either.
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
		name := approvalRecord(record.ID)
		err := s.root.Remove(name)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := s.root.Lstat(name); !errors.Is(err, os.ErrNotExist) {
			return errors.New("this approval could not be proven removed")
		}
		removed = true
		// An unlink is durable only once its directory entry is — the same fsync
		// a publication needs, for the same reason.
		return s.confirmPublication()
	})
	if err != nil {
		return false, err
	}
	return removed, s.intactAuthority()
}
