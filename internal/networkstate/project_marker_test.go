package networkstate

import (
	"os"
	"path/filepath"
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
