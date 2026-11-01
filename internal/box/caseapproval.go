package box

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/ui"
)

// CaseExposureReview captures paths, not their contents: approving a path also
// accepts later content at that path. A new path always needs its own review.
type CaseExposureReview struct {
	Paths        []string
	Pending      []string
	repo, anchor string
}

type caseExposureApproval struct {
	Version int      `json:"version"`
	Anchor  string   `json:"anchor"`
	Paths   []string `json:"paths"`
}

func caseExposurePath(anchor string) (string, error) {
	root, err := serviceStateRoot("case-exposure-approvals")
	return filepath.Join(root, anchor+".json"), err
}

// UnapprovedCaseExposures is shared by agent and service launch admission.
// Broken or missing authority grants nothing. Policy files cannot be approved:
// their alternate spellings would allow writes to future hiding rules.
func UnapprovedCaseExposures(repo string, paths []string) []string {
	var approved caseExposureApproval
	anchor, err := serviceApprovalAnchor(repo, false)
	if err == nil {
		file, pathErr := caseExposurePath(anchor)
		if pathErr == nil {
			data, readErr := os.ReadFile(file)
			if readErr != nil || json.Unmarshal(data, &approved) != nil || approved.Version != 1 || approved.Anchor != anchor {
				approved.Paths = nil
			}
		}
	}
	var pending []string
	for _, path := range paths {
		if IsCoopignoreName(filepath.Base(path)) || !slices.Contains(approved.Paths, path) {
			pending = append(pending, path)
		}
	}
	return pending
}

// ReviewCaseExposure includes configured service binds because their cover
// budget and target lengths differ from the agent's workspace mount.
func ReviewCaseExposure(ctx context.Context, cfg *config.Config, repo string) (*CaseExposureReview, error) {
	_, err := ComputeMounts(repo, Workdir(cfg, repo))
	var uncovered *UncoveredSpellings
	var paths []string
	if errors.As(err, &uncovered) {
		paths = append(paths, uncovered.Paths...)
	} else if err != nil {
		return nil, err
	}
	p, err := project.Load(repo)
	if err != nil {
		return nil, err
	}
	if file := ComposeFileAt(repo, p.ComposeRel()); file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		_, _, exposed, err := serviceShadowPlan(repo, file, data)
		if err != nil {
			return nil, err
		}
		paths = append(paths, exposed...)
	}
	slices.Sort(paths)
	paths = slices.Compact(paths)
	for _, path := range paths {
		if IsCoopignoreName(filepath.Base(path)) {
			return nil, &UncoveredSpellings{Paths: []string{path}}
		}
	}
	review := &CaseExposureReview{Paths: paths, repo: repo}
	review.Pending = UnapprovedCaseExposures(repo, paths)
	if len(review.Pending) == 0 {
		return review, nil
	}
	// Existing identity needs no transition and must not wait for active boxes.
	if anchor, err := serviceApprovalAnchor(repo, false); err == nil {
		review.anchor = anchor
		return review, nil
	}
	ui.Note("Waiting for a safe launch window to record file access.")
	// The existing service identity provides the same copy-resistant binding
	// and private-state protection. Creation is serialized with box launches.
	unlock, err := forkspace.LockServiceLaunch(ctx, repo, true)
	if err != nil {
		return nil, err
	}
	defer unlock()
	review.anchor, err = serviceApprovalAnchor(repo, true)
	return review, err
}

func (r *CaseExposureReview) Commit(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(r.Pending) == 0 {
		return nil
	}
	anchor, err := serviceApprovalAnchor(r.repo, false)
	if err != nil {
		return err
	}
	if anchor != r.anchor {
		return errors.New("project identity changed — run 'coop approve' again")
	}
	file, err := caseExposurePath(anchor)
	if err != nil {
		return err
	}
	data, err := json.Marshal(caseExposureApproval{Version: 1, Anchor: anchor, Paths: r.Paths})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	return config.WriteFileAtomicMode(file, data, 0o600)
}
