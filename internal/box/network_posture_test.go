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

func postureFixture(t *testing.T, projectYAML string) (*config.Config, string, string) {
	t.Helper()
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	repo := t.TempDir()
	if projectYAML != "" {
		if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, ".agent", "project.yaml"), []byte(projectYAML), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &config.Config{ConfigDir: t.TempDir()}, repo, filepath.Join(state, "coop", "network")
}

// Reading the posture must not be how a host acquires network authority: a
// query that created an owner key would make "never used" indistinguishable
// from "used once".
func TestPostureOnAFreshHostCreatesNoAuthority(t *testing.T) {
	cfg, repo, root := postureFixture(t, "")
	posture, err := ProjectNetworkPosture(context.Background(), cfg, repo)
	if err != nil {
		t.Fatalf("ProjectNetworkPosture: %v", err)
	}
	if posture.Mode != egress.Open || posture.Source != PostureFromDefault {
		t.Errorf("posture = %q from %q, want the built-in open default", posture.Mode, posture.Source)
	}
	if posture.Approval != nil || posture.Setup != nil || posture.Pending != nil {
		t.Errorf("a fresh host reported approval=%v setup=%v pending=%v", posture.Approval, posture.Setup, posture.Pending)
	}
	if posture.SetupCurrent() {
		t.Error("a host with no record claims to be set up")
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("the authority root was created by a read: %v", err)
	}
}

func TestPostureShowsAnUnapprovedProjectRequestInsteadOfRefusing(t *testing.T) {
	cfg, repo, _ := postureFixture(t, "box:\n  egress: filtered\n  egress_rules:\n    - to:\n        domain: \"docs.example.com\"\n      protocol: tls\n      ports: [443]\n")
	posture, err := ProjectNetworkPosture(context.Background(), cfg, repo)
	if err != nil {
		t.Fatalf("ProjectNetworkPosture: %v", err)
	}
	if posture.Mode != egress.Filtered || posture.Source != PostureFromProject {
		t.Errorf("posture = %q from %q, want filtered from the project request", posture.Mode, posture.Source)
	}
	// The launch would refuse; the view must SAY that, not inherit the refusal.
	if posture.Pending == nil {
		t.Error("an unapproved request was not reported as pending")
	}
	if len(posture.Add) != 1 || posture.Add[0].To.Domain != "docs.example.com" {
		t.Errorf("pending additions = %+v", posture.Add)
	}
	if len(posture.Remove) != 0 {
		t.Errorf("pending removals = %+v", posture.Remove)
	}
}

func TestPostureFollowsAnExplicitHostPreference(t *testing.T) {
	cfg, repo, _ := postureFixture(t, "")
	cfg.SetEgress(string(egress.None))
	posture, err := ProjectNetworkPosture(context.Background(), cfg, repo)
	if err != nil {
		t.Fatalf("ProjectNetworkPosture: %v", err)
	}
	if posture.Mode != egress.None || posture.Source != PostureFromHost {
		t.Errorf("posture = %q from %q, want none from COOP_EGRESS", posture.Mode, posture.Source)
	}
}

func TestPostureRefusesToDescribeADirectoryThatIsNotThere(t *testing.T) {
	cfg, repo, _ := postureFixture(t, "")
	if _, err := ProjectNetworkPosture(context.Background(), cfg, filepath.Join(repo, "missing")); err == nil {
		t.Fatal("a missing project directory was described")
	}
}

// The whole approval loop without a container: review what the repository asks
// for, commit it, and see the posture become remembered host authority.
func TestReviewAndApproveRemembersTheProjectRequest(t *testing.T) {
	cfg, repo, root := postureFixture(t, "box:\n  egress: filtered\n  egress_rules:\n    - to:\n        domain: \"docs.example.com\"\n      protocol: tls\n      ports: [443]\n")
	review, err := ReviewProjectNetwork(cfg, repo, nil)
	if err != nil {
		t.Fatalf("ReviewProjectNetwork: %v", err)
	}
	defer review.Close()
	if review.Before() != nil {
		t.Errorf("before = %+v, want nothing remembered", review.Before())
	}
	after := review.After()
	if after == nil || after.Posture != egress.Filtered || len(after.Envelope) != 1 || after.Envelope[0].To.Domain != "docs.example.com" {
		t.Fatalf("after = %+v", after)
	}
	if err := review.Commit(context.Background()); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// One review, one decision: a second commit is a bug, not a retry.
	if err := review.Commit(context.Background()); err == nil {
		t.Error("a consumed review committed twice")
	}
	if err := review.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := review.Close(); err != nil {
		t.Fatalf("Close is not idempotent: %v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("approving did not create the authority root: %v", err)
	}
	posture, err := ProjectNetworkPosture(context.Background(), cfg, repo)
	if err != nil {
		t.Fatalf("ProjectNetworkPosture: %v", err)
	}
	if posture.Source != PostureFromApproval || posture.Mode != egress.Filtered {
		t.Errorf("posture = %q from %q, want filtered from the remembered approval", posture.Mode, posture.Source)
	}
	if posture.Pending != nil || len(posture.Add) != 0 || len(posture.Remove) != 0 {
		t.Errorf("an approved request still reads as pending: %v %+v %+v", posture.Pending, posture.Add, posture.Remove)
	}
}

func TestReviewRefusesAnUnqualifiedProviderFeatureRequest(t *testing.T) {
	cfg, repo, _ := postureFixture(t, "box:\n  egress: filtered\n  egress_rules:\n    - to:\n        provider: claude\n        features: [cloud-mcp]\n")
	_, err := ReviewProjectNetwork(cfg, repo, nil)
	if err == nil {
		t.Fatal("a provider feature request was reviewed")
	}
	if !strings.Contains(err.Error(), "captured automatically at launch") {
		t.Errorf("error = %v, want it to say where core access comes from", err)
	}
}

func TestReviewHonorsAnExplicitMode(t *testing.T) {
	cfg, repo, _ := postureFixture(t, "")
	open := egress.Open
	review, err := ReviewProjectNetwork(cfg, repo, &open)
	if err != nil {
		t.Fatalf("ReviewProjectNetwork: %v", err)
	}
	defer review.Close()
	if review.Mode() != egress.Open || review.After().Posture != egress.Open {
		t.Errorf("mode = %q, want the explicit open", review.Mode())
	}
	bad := egress.Mode("sideways")
	if _, err := ReviewProjectNetwork(cfg, repo, &bad); err == nil {
		t.Error("an invalid --mode was reviewed")
	}
}
