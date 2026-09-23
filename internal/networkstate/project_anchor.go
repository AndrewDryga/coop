package networkstate

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/AndrewDryga/coop/internal/fsidentity"
)

const ProjectApprovalMarker = ".coop-network-approval"

var projectAnchorRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

var errProjectAnchorTransition = errors.New("project identity needs an exclusive transition")

func newProjectAnchor() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func projectAnchorName(projectID, anchor string) string {
	return "project-" + projectID + "-" + anchor + ".anchor"
}

func projectAnchorBody(projectID, anchor string) []byte {
	return []byte("coop-network-approval-v3\n" + projectID + "\n" + anchor + "\n")
}

func (s *Store) projectAnchorBinding(project, projectID, anchor string) fsidentity.Binding {
	return fsidentity.Binding{RootPath: project, MarkerName: ProjectApprovalMarker, AnchorRoot: s.root,
		AnchorName: projectAnchorName(projectID, anchor), Body: projectAnchorBody(projectID, anchor)}
}

// ensureProjectAnchor creates no network grant. It prepares (or reuses after an interrupted or
// cancelled review) the unforgeable directory binding whose random name is included in the review
// digest. Only publishing an Approval turns that binding into authority.
func (s *Store) ensureProjectAnchor(project, projectID string, allowTransition bool, prior *Approval) (string, error) {
	body, err := fsidentity.ReadMarker(project, ProjectApprovalMarker)
	switch {
	case err == nil:
		boundProject, anchor, parseErr := parseAnyProjectAnchorBody(body)
		if parseErr != nil {
			return "", fmt.Errorf("reserved network approval marker is not valid: %w; inspect it with 'ls -l -- ./.coop-network-approval', remove it with 'rm -- ./.coop-network-approval' only if it is safe to do so, then run 'coop approve' again", parseErr)
		}
		if boundProject != projectID {
			if !allowTransition {
				return "", errProjectAnchorTransition
			}
			// Moving a checkout moves its marker but changes the path-keyed project id. An explicit
			// approval is the safe re-enrollment point, but only the exact old hardlink pair proves
			// this is a move. A copied marker must never be allowed to retire another project's anchor.
			oldBinding := s.projectAnchorBinding(project, boundProject, anchor)
			root, openErr := fsidentity.Open(oldBinding)
			if openErr != nil {
				return "", fmt.Errorf("project approval marker names another checkout but does not match its private anchor: %w", openErr)
			}
			if closeErr := root.Close(); closeErr != nil {
				return "", closeErr
			}
			if err := fsidentity.Retire(oldBinding); err != nil {
				return "", fmt.Errorf("retire moved project network identity: %w", err)
			}
		} else {
			root, openErr := fsidentity.Open(s.projectAnchorBinding(project, projectID, anchor))
			if openErr != nil {
				return "", fmt.Errorf("validate pending project identity: %w", openErr)
			}
			if closeErr := root.Close(); closeErr != nil {
				return "", closeErr
			}
			return anchor, nil
		}
	case errors.Is(err, os.ErrNotExist):
		if !allowTransition {
			return "", errProjectAnchorTransition
		}
		// Fresh checkout: create the non-authoritative pair below.
	default:
		return "", fmt.Errorf("read pending project identity: %w", err)
	}
	if allowTransition && prior != nil && prior.Version == networkApprovalVersion {
		// At this point the current path has no marker: it was absent, or the exact marker from a
		// moved checkout was retired above. Retire the invalid approval's private half BEFORE a
		// replacement marker is created. A crash here leaves the old grant invalid and retryable;
		// doing this after Create would mistake the replacement marker for an interrupted old one.
		if err := s.retireProjectAnchor(project, prior); err != nil {
			return "", fmt.Errorf("retire replaced project identity: %w", err)
		}
	}
	anchor, err := newProjectAnchor()
	if err != nil {
		return "", err
	}
	root, err := fsidentity.Create(s.projectAnchorBinding(project, projectID, anchor))
	if err != nil {
		return "", fmt.Errorf("anchor project network approval: %w", err)
	}
	if err := root.Close(); err != nil {
		_ = fsidentity.Retire(s.projectAnchorBinding(project, projectID, anchor))
		return "", err
	}
	return anchor, nil
}

func parseAnyProjectAnchorBody(body []byte) (string, string, error) {
	lines := strings.Split(string(body), "\n")
	if len(lines) != 4 || lines[0] != "coop-network-approval-v3" || !lowerHex(lines[1], 64) ||
		lines[3] != "" || !projectAnchorRE.MatchString(lines[2]) {
		return "", "", errors.New("reserved network approval marker does not have Coop's expected shape")
	}
	return lines[1], lines[2], nil
}

func (s *Store) validateProjectAnchor(project string, approval *Approval) error {
	if approval == nil || approval.Version != networkApprovalVersion || !projectAnchorRE.MatchString(approval.ProjectAnchor) {
		return errors.New("network approval has no current project anchor")
	}
	root, err := fsidentity.Open(s.projectAnchorBinding(project, approval.ProjectID, approval.ProjectAnchor))
	if err != nil {
		return err
	}
	return root.Close()
}

func (s *Store) retireProjectAnchor(project string, approval *Approval) error {
	if approval == nil || approval.Version != networkApprovalVersion || !projectAnchorRE.MatchString(approval.ProjectAnchor) {
		return nil
	}
	return fsidentity.Retire(s.projectAnchorBinding(project, approval.ProjectID, approval.ProjectAnchor))
}
