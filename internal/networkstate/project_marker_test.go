package networkstate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/egress"
)

func TestProjectMarkerInfoDoesNotGrantNetworkAuthority(t *testing.T) {
	s, project := openStore(t), t.TempDir()
	if _, err := s.ReviewApproval(project, egress.None, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProjectMarkerInfo(project); err != nil {
		t.Fatal(err)
	}
	if approval, err := s.Approval(project); err != nil || approval != nil {
		t.Fatalf("marker validation granted authority: %v, %v", approval, err)
	}
	copyProject := t.TempDir()
	body, err := os.ReadFile(filepath.Join(project, ProjectApprovalMarker))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(copyProject, ProjectApprovalMarker), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProjectMarkerInfo(copyProject); err == nil {
		t.Fatal("copied marker carried a shared-inode exception")
	}
	if err := os.Link(filepath.Join(project, ProjectApprovalMarker), filepath.Join(t.TempDir(), "alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProjectMarkerInfo(project); err == nil {
		t.Fatal("third hardlink kept marker binding valid")
	}
}

// overwriteMarker writes the checkout's marker the way `printf junk > .coop-network-approval` does:
// through the hard link, so Coop's private anchor holds the same bytes.
func overwriteMarker(t *testing.T, project string) {
	t.Helper()
	file, err := os.OpenFile(filepath.Join(project, ProjectApprovalMarker), os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("junk\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

// A marker written in place, by a person or by an agent in a box, overwrites Coop's private anchor
// through the hard link. The approval it belonged to grants nothing from then on, and the checkout
// can be approved again: by one review, by the printed steps (look, remove the marker, approve), or
// after a forget.
func TestAnOverwrittenMarkerCanBeApprovedAgain(t *testing.T) {
	requests := []egress.Rule{rule("docs.example.com")}
	for _, recover := range []string{"one review", "printed steps", "forget"} {
		t.Run(recover, func(t *testing.T) {
			s, project := openStore(t), t.TempDir()
			if err := approve(s, project, egress.Filtered, requests, nil); err != nil {
				t.Fatal(err)
			}
			old, err := s.Approval(project)
			if err != nil || old == nil {
				t.Fatal(old, err)
			}
			oldAnchor := projectAnchorName(old.ProjectID, old.ProjectAnchor)
			overwriteMarker(t, project)

			// The overwritten pair grants nothing.
			if _, err := s.Admit(project, Admission{Requests: requests}); err == nil {
				t.Fatal("an overwritten marker still admitted the approved rules")
			}
			if preview, err := s.admissionPreview(project, Admission{Requests: requests}); err != nil || preview.Pending == nil {
				t.Fatalf("preview = %+v, %v; want a pending review", preview, err)
			}
			// The review that reuses a binding without the transition lock hands this to the locked one,
			// and removes nothing itself.
			if _, transition, err := s.ReviewApprovalExisting(project, egress.Filtered, requests, nil, nil); err != nil || !transition {
				t.Fatalf("existing review: transition=%v err=%v", transition, err)
			}
			for _, name := range []string{filepath.Join(project, ProjectApprovalMarker), filepath.Join(s.Path(), oldAnchor)} {
				if _, err := os.Lstat(name); err != nil {
					t.Fatalf("the review without the lock removed %s: %v", name, err)
				}
			}

			switch recover {
			case "printed steps":
				if err := os.Remove(filepath.Join(project, ProjectApprovalMarker)); err != nil {
					t.Fatal(err)
				}
			case "forget":
				record, err := s.ApprovalAt(project)
				if err != nil {
					t.Fatal(err)
				}
				if removed, err := s.Forget(context.Background(), record); err != nil || !removed {
					t.Fatalf("Forget = %v, %v", removed, err)
				}
				if _, err := os.Lstat(filepath.Join(project, ProjectApprovalMarker)); !os.IsNotExist(err) {
					t.Fatalf("forget left the overwritten marker: %v", err)
				}
			}
			if err := approve(s, project, egress.Filtered, requests, nil); err != nil {
				t.Fatalf("approving again after the overwrite: %v", err)
			}
			if _, err := s.root.Lstat(oldAnchor); !os.IsNotExist(err) {
				t.Fatalf("the overwritten private anchor stayed: %v", err)
			}
			current, err := s.Approval(project)
			if err != nil || current == nil || current.ProjectAnchor == old.ProjectAnchor {
				t.Fatalf("approval = %+v, %v; want a new anchor", current, err)
			}
			if _, err := s.Admit(project, Admission{Requests: requests}); err != nil {
				t.Fatalf("the new approval does not admit: %v", err)
			}
		})
	}
}

// Only the pair the approval names goes by identity. A stranger file put at the marker's name after
// the overwrite stays for the person to look at, and a checkout moved away with its marker
// overwritten takes no approval along.
func TestAnOverwrittenMarkerRetiresOnlyItsOwnPair(t *testing.T) {
	requests := []egress.Rule{rule("docs.example.com")}
	s, project := openStore(t), t.TempDir()
	if err := approve(s, project, egress.Filtered, requests, nil); err != nil {
		t.Fatal(err)
	}
	overwriteMarker(t, project)
	marker := filepath.Join(project, ProjectApprovalMarker)
	stranger := filepath.Join(project, "stranger")
	if err := os.WriteFile(stranger, []byte("junk\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(stranger, marker); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReviewApproval(project, egress.Filtered, requests, nil, nil); err == nil || !strings.Contains(err.Error(), "rm -- ./.coop-network-approval") {
		t.Fatalf("review over a stranger marker = %v, want the steps to look and remove it", err)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "junk\n" {
		t.Fatalf("the stranger marker was not left alone: %q, %v", data, err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := approve(s, project, egress.Filtered, requests, nil); err != nil {
		t.Fatal(err)
	}

	// The new folder has no approval of its own and gets none from the old one.
	overwriteMarker(t, project)
	moved := filepath.Join(t.TempDir(), "moved")
	if err := os.Rename(project, moved); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReviewApproval(moved, egress.Filtered, requests, nil, nil); err == nil {
		t.Fatal("a moved overwritten marker was reviewed without a look")
	}
	if approval, err := s.Approval(moved); err != nil || approval != nil {
		t.Fatalf("the moved checkout holds approval %+v, %v", approval, err)
	}
	if _, err := s.Admit(moved, Admission{Requests: requests}); err == nil {
		t.Fatal("the moved checkout was admitted under the old approval")
	}
}
