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

// requestFixtureYAML is a project asking for one website: the smallest request
// that is pending until a human approves it.
const requestFixtureYAML = "box:\n  egress_rules:\n    - to:\n        domain: \"docs.example.com\"\n      protocol: tls\n      ports: [443]\n"

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
	if posture.Approval != nil || posture.Pending != nil {
		t.Errorf("a fresh host reported approval=%v pending=%v", posture.Approval, posture.Pending)
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
	review, err := ReviewProjectNetwork(cfg, repo)
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

// A directory moved aside and replaced at the same path inherits nothing, so
// the next launch refuses. Nothing in the rule diff can show that, which is why
// the posture view reports it as pending in its own right.
func TestPostureReportsAnApprovedDirectoryThatWasReplaced(t *testing.T) {
	cfg, repo, _ := postureFixture(t, "box:\n  egress_rules:\n    - to:\n        domain: \"docs.example.com\"\n      protocol: tls\n      ports: [443]\n")
	approveFixture(t, cfg, repo)
	if err := os.Rename(repo, repo+"-moved-aside"); err != nil {
		t.Fatal(err)
	}
	// The replacement asks for exactly the same thing, so nothing in the rule
	// diff can show what changed.
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "project.yaml"), []byte(requestFixtureYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	posture, err := ProjectNetworkPosture(context.Background(), cfg, repo)
	if err != nil {
		t.Fatalf("ProjectNetworkPosture: %v", err)
	}
	if posture.Pending == nil || !strings.Contains(posture.Pending.Reason, "was replaced since it was approved") {
		t.Fatalf("pending = %v, want the replaced directory reported", posture.Pending)
	}
	// The reason is a plain sentence; every view adds the one review command itself.
	if strings.Contains(posture.Pending.Reason, "coop net") {
		t.Errorf("pending = %v, want a reason without the remedy baked in", posture.Pending)
	}
	// The rule diff is empty: without the pending line this view would look
	// exactly like a project that is good to go.
	if len(posture.Add) != 0 || len(posture.Remove) != 0 {
		t.Errorf("add=%+v remove=%+v, want the notice to be the only sign", posture.Add, posture.Remove)
	}
}

func TestReviewRefusesAnUnqualifiedProviderFeatureRequest(t *testing.T) {
	cfg, repo, _ := postureFixture(t, "box:\n  egress: filtered\n  egress_rules:\n    - to:\n        provider: claude\n        features: [cloud-mcp]\n")
	_, err := ReviewProjectNetwork(cfg, repo)
	if err == nil {
		t.Fatal("a provider feature request was reviewed")
	}
	if !strings.Contains(err.Error(), "core endpoints are allowed automatically") {
		t.Errorf("error = %v, want it to say where core access comes from", err)
	}
}

// The mode comes from .agent/project.yaml and nowhere else. A request for open
// access is a widening the repository cannot grant itself: it is pending until a
// human approves exactly that, and the review says open because the file does.
func TestReviewTakesTheModeFromTheProjectFileOnly(t *testing.T) {
	cfg, repo, _ := postureFixture(t, "box:\n  egress: open\n")
	posture, err := ProjectNetworkPosture(context.Background(), cfg, repo)
	if err != nil {
		t.Fatalf("ProjectNetworkPosture: %v", err)
	}
	if posture.Pending == nil || !strings.Contains(posture.Pending.Reason, "unrestricted") || posture.RequestedMode != egress.Open {
		t.Fatalf("an unapproved open request is not pending: pending=%v requested=%q", posture.Pending, posture.RequestedMode)
	}
	review, err := ReviewProjectNetwork(cfg, repo)
	if err != nil {
		t.Fatalf("ReviewProjectNetwork: %v", err)
	}
	defer review.Close()
	if review.Unchanged() || review.Mode() != egress.Open || review.After().Posture != egress.Open {
		t.Errorf("mode = %q unchanged=%v, want the file's open", review.Mode(), review.Unchanged())
	}
	if err := review.Commit(context.Background()); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	posture, err = ProjectNetworkPosture(context.Background(), cfg, repo)
	if err != nil || posture.Pending != nil || posture.Mode != egress.Open || posture.Source != PostureFromApproval {
		t.Errorf("after approval: pending=%v mode=%q source=%q err=%v", posture.Pending, posture.Mode, posture.Source, err)
	}
}

// A request that is exactly what was approved — or one that asks for nothing an
// approval must cover — has no question to answer: the review says so without
// creating authority state or touching the stored decision.
func TestReviewOfAnUnchangedRequestWritesNothing(t *testing.T) {
	for name, yaml := range map[string]string{"default": "", "filtered": "box:\n  egress: filtered\n", "offline": "box:\n  egress: offline\n"} {
		t.Run(name, func(t *testing.T) {
			cfg, repo, root := postureFixture(t, yaml)
			review, err := ReviewProjectNetwork(cfg, repo)
			if err != nil {
				t.Fatalf("ReviewProjectNetwork: %v", err)
			}
			if !review.Unchanged() {
				t.Fatal("a project asking for nothing beyond the safe posture was put up for approval")
			}
			if err := review.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Errorf("a no-op review created the authority root: %v", err)
			}
		})
	}
	// Approved once, the same request is a no-op the next time, byte for byte.
	cfg, repo, root := postureFixture(t, "box:\n  egress_rules:\n    - to:\n        domain: \"docs.example.com\"\n      protocol: tls\n      ports: [443]\n")
	approveFixture(t, cfg, repo)
	before, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	review, err := ReviewProjectNetwork(cfg, repo)
	if err != nil {
		t.Fatalf("ReviewProjectNetwork: %v", err)
	}
	if !review.Unchanged() {
		t.Fatal("an approved request was put up for approval again")
	}
	after, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Errorf("a no-op review changed the store: %d entries before, %d after", len(before), len(after))
	}
	for i := range before {
		b, _ := before[i].Info()
		a, _ := after[i].Info()
		if before[i].Name() != after[i].Name() || !b.ModTime().Equal(a.ModTime()) {
			t.Errorf("a no-op review touched %s", before[i].Name())
		}
	}
}

// An approval every launch would refuse by name is not a decision worth
// remembering: `coop net approve` applies the same capability gate, so the
// operator learns now instead of at the next unattended run.
func TestReviewRefusesARuleNoLaunchCouldEnforce(t *testing.T) {
	for _, test := range []struct{ yaml, want string }{
		{"box:\n  egress_rules:\n    - to:\n        domain: api.example.com\n      protocol: tls\n      ports: [53]\n",
			"TLS on port 53 is not allowed here"},
		{"box:\n  egress_rules:\n    - to:\n        cidr: 169.254.0.0/16\n      protocol: tcp\n      ports: [80]\n",
			"is a protected range"},
		{"box:\n  egress_rules:\n    - to:\n        ip: 2606:4700:4700::1111\n      protocol: tcp\n      ports: [5432]\n",
			"IPv6 destinations are not supported yet"},
	} {
		cfg, repo, root := postureFixture(t, test.yaml)
		_, err := ReviewProjectNetwork(cfg, repo)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("review = %v, want a refusal naming %q", err, test.want)
		}
		// The refusal comes before any authority exists: nothing was created to
		// hold a decision nobody could act on.
		if _, err := os.Stat(root); !os.IsNotExist(err) {
			t.Errorf("an unenforceable request created the authority root: %v", err)
		}
	}
	// The same shape, on a port this runtime does enforce, is reviewable.
	cfg, repo, _ := postureFixture(t, "box:\n  egress_rules:\n    - to:\n        domain: api.example.com\n      protocol: tls\n      ports: [443]\n")
	review, err := ReviewProjectNetwork(cfg, repo)
	if err != nil {
		t.Fatalf("an enforceable request was refused: %v", err)
	}
	defer review.Close()
}
