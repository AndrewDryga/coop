package networkstate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync/atomic"

	"github.com/AndrewDryga/coop/internal/egress"
)

var ErrApprovalChanged = errors.New("network approval changed after review; review the current request again")

// ApprovalReview is an owner-only, one-use capability. It never crosses a worker
// or session request. Before/After return copies, not mutable approval inputs.
type ApprovalReview struct {
	store             *Store
	project, resolved string
	identity          os.FileInfo
	before, after     *Approval
	bundles           []egress.Bundle
	used              atomic.Bool
}

func (s *Store) ReviewApproval(project string, mode egress.Mode, requests []egress.Rule, bundles []egress.Bundle) (*ApprovalReview, error) {
	if err := s.intactAuthority(); err != nil {
		return nil, err
	}
	resolved, err := canonicalPath(project)
	if err != nil {
		return nil, err
	}
	identity, err := os.Stat(resolved)
	if err != nil || !identity.IsDir() {
		return nil, errors.New("network approval requires an existing project directory")
	}
	id, err := s.projectID(resolved)
	if err != nil {
		return nil, err
	}
	if _, err := egress.ParseMode(string(mode)); err != nil {
		return nil, err
	}
	rules, err := egress.NormalizeRules(requests)
	if err != nil {
		return nil, err
	}
	if mode != egress.Filtered && len(rules) != 0 {
		return nil, errors.New("remove project rules before approving open/none posture")
	}
	selected, err := egress.SelectedBundles(bundles)
	if err != nil {
		return nil, err
	}
	features, err := featureApprovals(rules, selected)
	if err != nil {
		return nil, err
	}
	after := &Approval{Version: 1, ProjectID: id, Posture: mode, Envelope: rules, Features: features}
	data, err := json.Marshal(after)
	if err != nil || len(data) > maxPrivateRecordBytes {
		return nil, errors.New("network approval exceeds byte limit")
	}
	before, err := s.approval(id)
	if err != nil {
		return nil, err
	}
	return &ApprovalReview{store: s, project: project, resolved: resolved, identity: identity,
		before: before, after: after, bundles: selected}, nil
}

func copyApproval(value *Approval) *Approval {
	if value == nil {
		return nil
	}
	data, _ := json.Marshal(value)
	var copy Approval
	_ = json.Unmarshal(data, &copy)
	return &copy
}

func (r *ApprovalReview) Before() *Approval { return copyApproval(r.before) }
func (r *ApprovalReview) After() *Approval  { return copyApproval(r.after) }

// Commit never rereads project configuration or accepts replacement rules.
// A failed or cancelled attempt consumes the review; retries need a fresh view.
func (r *ApprovalReview) Commit(ctx context.Context) error {
	if r == nil || r.store == nil || r.after == nil || !r.used.CompareAndSwap(false, true) {
		return errors.New("network approval review was already consumed or is invalid")
	}
	return r.store.lockRecord(ctx, "approval", r.after.ProjectID, func() error {
		if err := r.store.intactAuthority(); err != nil {
			return err
		}
		resolved, err := canonicalPath(r.project)
		if err != nil || resolved != r.resolved {
			return errors.New("network approval project changed after review")
		}
		current, err := os.Stat(resolved)
		if err != nil || !os.SameFile(r.identity, current) {
			return errors.New("network approval project changed after review")
		}
		before, err := r.store.approval(r.after.ProjectID)
		if err != nil {
			return fmt.Errorf("read approval after confirmation: %w", err)
		}
		if !equalJSON(before, r.before) {
			return ErrApprovalChanged
		}
		if err := r.store.CheckBundles(r.bundles); err != nil {
			return fmt.Errorf("publish reviewed bundles: %w", err)
		}
		data, err := json.Marshal(r.after)
		if err != nil {
			return err
		}
		if err := r.store.publish("approval-"+r.after.ProjectID+".json", data, true); err != nil {
			return fmt.Errorf("publish reviewed approval: %w", err)
		}
		return r.store.intactAuthority()
	})
}
