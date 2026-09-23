package box

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/networkstate"
)

func TestFirstNetworkApprovalWaitsForExistingSandboxMountWindows(t *testing.T) {
	cfg, repo, _ := postureFixture(t, requestFixtureYAML)
	shared, err := forkspace.LockServiceLaunch(context.Background(), repo, false)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		review *ProjectNetworkApproval
		err    error
	}
	done := make(chan result, 1)
	go func() {
		review, err := ReviewProjectNetwork(cfg, repo)
		done <- result{review, err}
	}()
	select {
	case got := <-done:
		shared()
		if got.review != nil {
			_ = got.review.Close()
		}
		t.Fatalf("first approval crossed a live sandbox mount window: %v", got.err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := os.Lstat(filepath.Join(repo, networkstate.ProjectApprovalMarker)); !os.IsNotExist(err) {
		shared()
		t.Fatalf("approval marker appeared before live sandboxes drained: %v", err)
	}
	updated := strings.Replace(requestFixtureYAML, "docs.example.com", "after-drain.example.com", 1)
	if err := os.WriteFile(filepath.Join(repo, ".agent", "project.yaml"), []byte(updated), 0o644); err != nil {
		shared()
		t.Fatal(err)
	}
	shared()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		defer got.review.Close()
		if _, err := os.Lstat(filepath.Join(repo, networkstate.ProjectApprovalMarker)); err != nil {
			t.Fatalf("first approval did not anchor the project: %v", err)
		}
		if got.review.After() == nil || len(got.review.After().Envelope) != 1 || got.review.After().Envelope[0].To.Domain != "after-drain.example.com" {
			t.Fatalf("review used policy read before the transition lock: %+v", got.review.After())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first approval did not resume after live sandboxes drained")
	}
}

func TestFirstNetworkApprovalCanCancelWhileWaitingForSandboxMounts(t *testing.T) {
	cfg, repo, _ := postureFixture(t, requestFixtureYAML)
	shared, err := forkspace.LockServiceLaunch(context.Background(), repo, false)
	if err != nil {
		t.Fatal(err)
	}
	defer shared()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		review, err := ReviewProjectNetworkContext(ctx, cfg, repo)
		if review != nil {
			_ = review.Close()
		}
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled review = %v, want context cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled approval stayed blocked behind a sandbox")
	}
	if _, err := os.Lstat(filepath.Join(repo, networkstate.ProjectApprovalMarker)); !os.IsNotExist(err) {
		t.Fatalf("cancelled approval published a project marker: %v", err)
	}
}

func TestNetworkApprovalMarkerDisappearanceTransitionsUnderLaunchLock(t *testing.T) {
	cfg, repo, _ := postureFixture(t, requestFixtureYAML)
	approveFixture(t, cfg, repo)
	updated := strings.Replace(requestFixtureYAML, "docs.example.com", "changed.example.com", 1)
	if err := os.WriteFile(filepath.Join(repo, ".agent", "project.yaml"), []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}
	shared, err := forkspace.LockServiceLaunch(context.Background(), repo, false)
	if err != nil {
		t.Fatal(err)
	}
	hookRan := make(chan struct{})
	previousHook := afterNetworkReviewInputsLoaded
	afterNetworkReviewInputsLoaded = func() {
		if err := os.Remove(filepath.Join(repo, networkstate.ProjectApprovalMarker)); err != nil {
			t.Errorf("remove marker at scheduling point: %v", err)
		}
		close(hookRan)
	}
	t.Cleanup(func() { afterNetworkReviewInputsLoaded = previousHook })
	type result struct {
		review *ProjectNetworkApproval
		err    error
	}
	done := make(chan result, 1)
	go func() {
		review, err := ReviewProjectNetwork(cfg, repo)
		done <- result{review, err}
	}()
	<-hookRan
	select {
	case got := <-done:
		shared()
		if got.review != nil {
			_ = got.review.Close()
		}
		t.Fatalf("marker disappearance crossed a live sandbox mount window: %v", got.err)
	case <-time.After(100 * time.Millisecond):
	}
	shared()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		defer got.review.Close()
		if got.review.After() == nil || len(got.review.After().Envelope) != 1 ||
			got.review.After().Envelope[0].To.Domain != "changed.example.com" {
			t.Fatalf("review after marker transition = %+v", got.review.After())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("marker transition did not resume after live sandboxes drained")
	}
}

func TestMovedNetworkApprovalTransitionsUnderNewCheckoutLaunchLock(t *testing.T) {
	cfg, repo, _ := postureFixture(t, requestFixtureYAML)
	approveFixture(t, cfg, repo)
	access, err := ProjectNetworkAccess(context.Background(), cfg, repo)
	if err != nil || access.Approval == nil {
		t.Fatalf("read original approval: %+v, %v", access, err)
	}
	originalProjectID := access.Approval.ProjectID
	moved := repo + "-moved"
	if err := os.Rename(repo, moved); err != nil {
		t.Fatal(err)
	}
	shared, err := forkspace.LockServiceLaunch(context.Background(), moved, false)
	if err != nil {
		t.Fatal(err)
	}
	hookRan := make(chan struct{})
	previousHook := afterNetworkReviewInputsLoaded
	afterNetworkReviewInputsLoaded = func() { close(hookRan) }
	t.Cleanup(func() { afterNetworkReviewInputsLoaded = previousHook })
	type result struct {
		review *ProjectNetworkApproval
		err    error
	}
	done := make(chan result, 1)
	go func() {
		review, err := ReviewProjectNetwork(cfg, moved)
		done <- result{review, err}
	}()
	<-hookRan
	select {
	case got := <-done:
		shared()
		if got.review != nil {
			_ = got.review.Close()
		}
		t.Fatalf("moved checkout crossed its live sandbox mount window: %v", got.err)
	case <-time.After(100 * time.Millisecond):
	}
	shared()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		defer got.review.Close()
		if got.review.After() == nil || got.review.After().ProjectID == originalProjectID {
			t.Fatalf("moved checkout was not re-enrolled: before=%+v after=%+v", got.review.Before(), got.review.After())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("moved-checkout transition did not resume after live sandboxes drained")
	}
}

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

// Reading the access must not be how a host acquires network authority: a
// query that created an owner key would make "never used" indistinguishable
// from "used once".
func TestPostureOnAFreshHostCreatesNoAuthority(t *testing.T) {
	cfg, repo, root := postureFixture(t, "")
	access, err := ProjectNetworkAccess(context.Background(), cfg, repo)
	if err != nil {
		t.Fatalf("ProjectNetworkAccess: %v", err)
	}
	if access.Mode != egress.Open || access.Source != AccessFromDefault {
		t.Errorf("access = %q from %q, want the built-in open default", access.Mode, access.Source)
	}
	if access.Approval != nil || access.Pending != nil {
		t.Errorf("a fresh host reported approval=%v pending=%v", access.Approval, access.Pending)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("the authority root was created by a read: %v", err)
	}
}

func TestPostureShowsAnUnapprovedProjectRequestInsteadOfRefusing(t *testing.T) {
	cfg, repo, _ := postureFixture(t, "box:\n  egress: filtered\n  egress_rules:\n    - to:\n        domain: \"docs.example.com\"\n      protocol: tls\n      ports: [443]\n")
	access, err := ProjectNetworkAccess(context.Background(), cfg, repo)
	if err != nil {
		t.Fatalf("ProjectNetworkAccess: %v", err)
	}
	if access.Mode != egress.Filtered || access.Source != AccessFromProject {
		t.Errorf("access = %q from %q, want filtered from the project request", access.Mode, access.Source)
	}
	// The launch would refuse; the view must SAY that, not inherit the refusal.
	if access.Pending == nil {
		t.Error("an unapproved request was not reported as pending")
	}
	if len(access.Add) != 1 || access.Add[0].To.Domain != "docs.example.com" {
		t.Errorf("pending additions = %+v", access.Add)
	}
	if len(access.Remove) != 0 {
		t.Errorf("pending removals = %+v", access.Remove)
	}
}

func TestPostureFollowsAnExplicitHostPreference(t *testing.T) {
	cfg, repo, _ := postureFixture(t, "")
	cfg.SetEgress(string(egress.None))
	access, err := ProjectNetworkAccess(context.Background(), cfg, repo)
	if err != nil {
		t.Fatalf("ProjectNetworkAccess: %v", err)
	}
	if access.Mode != egress.None || access.Source != AccessFromHost {
		t.Errorf("access = %q from %q, want none from COOP_EGRESS", access.Mode, access.Source)
	}
}

func TestPostureRefusesToDescribeADirectoryThatIsNotThere(t *testing.T) {
	cfg, repo, _ := postureFixture(t, "")
	if _, err := ProjectNetworkAccess(context.Background(), cfg, filepath.Join(repo, "missing")); err == nil {
		t.Fatal("a missing project directory was described")
	}
}

// The whole approval loop without a container: review what the repository asks
// for, commit it, and see the access become remembered host authority.
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
	access, err := ProjectNetworkAccess(context.Background(), cfg, repo)
	if err != nil {
		t.Fatalf("ProjectNetworkAccess: %v", err)
	}
	if access.Source != AccessFromApproval || access.Mode != egress.Filtered {
		t.Errorf("access = %q from %q, want filtered from the remembered approval", access.Mode, access.Source)
	}
	if access.Pending != nil || len(access.Add) != 0 || len(access.Remove) != 0 {
		t.Errorf("an approved request still reads as pending: %v %+v %+v", access.Pending, access.Add, access.Remove)
	}
}

func TestReviewCanReplaceRememberedOfflineWithFiltered(t *testing.T) {
	cfg, repo, root := postureFixture(t, "box:\n  egress: offline\n")
	store, err := networkstate.Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.ReviewApproval(repo, egress.None, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Approve(t.Context(), repo, egress.None, nil, nil, nil, before.Digest); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "project.yaml"), []byte(requestFixtureYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	review, err := ReviewProjectNetwork(cfg, repo)
	if err != nil {
		t.Fatalf("offline-to-filtered review: %v", err)
	}
	defer review.Close()
	if review.Unchanged() || review.Before().Posture != egress.None || review.After().Posture != egress.Filtered {
		t.Fatalf("review = before %+v, after %+v, unchanged %v", review.Before(), review.After(), review.Unchanged())
	}
	if err := review.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	access, err := ProjectNetworkAccess(t.Context(), cfg, repo)
	if err != nil || access.Pending != nil || access.Mode != egress.Filtered {
		t.Fatalf("approved access = %+v, err %v", access, err)
	}
}

// A directory moved aside and replaced at the same path inherits nothing, so
// the next launch refuses. Nothing in the rule diff can show that, which is why
// the access view reports it as pending in its own right.
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
	access, err := ProjectNetworkAccess(context.Background(), cfg, repo)
	if err != nil {
		t.Fatalf("ProjectNetworkAccess: %v", err)
	}
	if access.Pending == nil || !strings.Contains(access.Pending.Reason, "was replaced since it was approved") {
		t.Fatalf("pending = %v, want the replaced directory reported", access.Pending)
	}
	// The reason is a plain sentence; every view adds the one review command itself.
	if strings.Contains(access.Pending.Reason, "coop net") {
		t.Errorf("pending = %v, want a reason without the remedy baked in", access.Pending)
	}
	// The rule diff is empty: without the pending line this view would look
	// exactly like a project that is good to go.
	if len(access.Add) != 0 || len(access.Remove) != 0 {
		t.Errorf("add=%+v remove=%+v, want the notice to be the only sign", access.Add, access.Remove)
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
	access, err := ProjectNetworkAccess(context.Background(), cfg, repo)
	if err != nil {
		t.Fatalf("ProjectNetworkAccess: %v", err)
	}
	if access.Pending == nil || !strings.Contains(access.Pending.Reason, "unrestricted") || access.RequestedMode != egress.Open {
		t.Fatalf("an unapproved open request is not pending: pending=%v requested=%q", access.Pending, access.RequestedMode)
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
	access, err = ProjectNetworkAccess(context.Background(), cfg, repo)
	if err != nil || access.Pending != nil || access.Mode != egress.Open || access.Source != AccessFromApproval {
		t.Errorf("after approval: pending=%v mode=%q source=%q err=%v", access.Pending, access.Mode, access.Source, err)
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
				t.Fatal("a project asking for nothing beyond the safe access was put up for approval")
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

func TestReviewOfUnchangedApprovalConfirmsPriorPublication(t *testing.T) {
	cfg, repo, _ := postureFixture(t, requestFixtureYAML)
	approveFixture(t, cfg, repo)
	previous := confirmNetworkStoreDurability
	t.Cleanup(func() { confirmNetworkStoreDurability = previous })
	failure := errors.New("synthetic approval directory sync failure")
	confirmNetworkStoreDurability = func(*networkstate.Store) error { return failure }
	if review, err := ReviewProjectNetwork(cfg, repo); !errors.Is(err, failure) {
		if review != nil {
			_ = review.Close()
		}
		t.Fatalf("unchanged review = %v, want durability failure", err)
	}
	confirmNetworkStoreDurability = previous
	review, err := ReviewProjectNetwork(cfg, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer review.Close()
	if !review.Unchanged() {
		t.Fatal("durability retry no longer recognized the unchanged approval")
	}
}

func TestApprovalRecoversAnEmptyRootLeftByFailedFirstCreation(t *testing.T) {
	cfg, repo, root := postureFixture(t, requestFixtureYAML)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	review, err := ReviewProjectNetwork(cfg, repo)
	if err != nil {
		t.Fatalf("review after interrupted authority-root creation: %v", err)
	}
	defer review.Close()
	if review.Unchanged() || review.After() == nil {
		t.Fatalf("interrupted empty root was mistaken for an existing approval: %+v", review)
	}
	if err := review.Commit(context.Background()); err != nil {
		t.Fatalf("approve after interrupted authority-root creation: %v", err)
	}
}

// An approval every launch would refuse by name is not a decision worth
// remembering: `coop approve` applies the same capability gate, so the
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
