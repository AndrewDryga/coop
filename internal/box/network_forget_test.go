package box

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
)

func approveFixture(t *testing.T, cfg *config.Config, repo string) {
	t.Helper()
	review, err := ReviewProjectNetwork(cfg, repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := review.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := review.Close(); err != nil {
		t.Fatal(err)
	}
}

// The whole revocation loop without a container: approve what the repository
// asks for, forget it, and watch the posture fall back to nothing remembered.
func TestForgetRemovesWhatAProjectRemembered(t *testing.T) {
	cfg, repo, _ := postureFixture(t, "box:\n  egress: filtered\n  egress_rules:\n    - to:\n        domain: \"docs.example.com\"\n      protocol: tls\n      ports: [443]\n")
	approveFixture(t, cfg, repo)
	review, err := ReviewProjectNetworkForget(repo)
	if err != nil {
		t.Fatalf("ReviewProjectNetworkForget: %v", err)
	}
	defer review.Close()
	approval := review.Approval()
	if approval == nil || approval.Posture != egress.Filtered || len(approval.Envelope) != 1 {
		t.Fatalf("the review does not show what would be removed: %+v", approval)
	}
	if review.Gone() {
		t.Error("a project that is right there was reported gone")
	}
	if err := review.Commit(context.Background()); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// One review, one removal: a second commit is a bug, not a retry.
	if err := review.Commit(context.Background()); err == nil {
		t.Error("a consumed forget committed twice")
	}
	if err := review.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := review.Close(); err != nil {
		t.Fatalf("Close is not idempotent: %v", err)
	}
	posture, err := ProjectNetworkPosture(context.Background(), cfg, repo)
	if err != nil {
		t.Fatalf("ProjectNetworkPosture: %v", err)
	}
	if posture.Approval != nil || posture.Source != PostureFromProject {
		t.Errorf("posture after a forget = %+v from %q, want the project's own request back", posture.Approval, posture.Source)
	}
	// The request is unapproved again, which is what makes a wrong forget cheap:
	// the next filtered run asks instead of reaching.
	if posture.Pending == nil || len(posture.Add) != 1 {
		t.Errorf("a forgotten project does not ask again: pending=%v add=%+v", posture.Pending, posture.Add)
	}
	if again, err := ReviewProjectNetworkForget(repo); err != nil || again.Approval() != nil {
		t.Errorf("a forgotten approval came back: %+v %v", again, err)
	}
}

// The reason --project exists: the checkout is gone, so there is no git top
// level to resolve and no directory to stat, and the record it left behind is
// reachable only by the path it was approved under.
func TestForgetAProjectDirectoryThatIsGone(t *testing.T) {
	cfg, parent, _ := postureFixture(t, "")
	repo := filepath.Join(parent, "checkout")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	approveFixture(t, cfg, repo)
	if err := os.RemoveAll(repo); err != nil {
		t.Fatal(err)
	}
	if _, err := ProjectNetworkPosture(context.Background(), cfg, repo); err == nil {
		t.Fatal("a deleted checkout still had a posture to describe")
	}
	review, err := ReviewProjectNetworkForget(repo)
	if err != nil {
		t.Fatalf("ReviewProjectNetworkForget: %v", err)
	}
	defer review.Close()
	if review.Approval() == nil || !review.Gone() {
		t.Fatalf("a deleted checkout's record was not offered for removal: %+v gone=%v", review.Approval(), review.Gone())
	}
	if err := review.Commit(context.Background()); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	after, err := ReviewProjectNetworkForget(repo)
	if err != nil || after.Approval() != nil {
		t.Fatalf("the deleted checkout's record outlived its removal: %+v %v", after, err)
	}
	defer after.Close()
	if err := after.Commit(context.Background()); err == nil {
		t.Error("committing with nothing to forget reported success")
	}
}

// Reading what is remembered must not be how a host acquires authority: a
// forget on a host that never approved anything answers, and creates nothing.
func TestForgetOnAHostThatRememberedNothing(t *testing.T) {
	_, repo, root := postureFixture(t, "")
	review, err := ReviewProjectNetworkForget(repo)
	if err != nil {
		t.Fatalf("ReviewProjectNetworkForget: %v", err)
	}
	defer review.Close()
	if review.Approval() != nil {
		t.Errorf("a fresh host remembered %+v", review.Approval())
	}
	if review.Project() == "" || review.Gone() {
		t.Errorf("project = %q gone = %v", review.Project(), review.Gone())
	}
	if err := review.Commit(context.Background()); err == nil {
		t.Error("a forget with nothing to forget reported success")
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("the authority root was created by a forget: %v", err)
	}
	if _, err := ReviewProjectNetworkForget("relative/path"); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Errorf("a relative project path was accepted: %v", err)
	}
}
