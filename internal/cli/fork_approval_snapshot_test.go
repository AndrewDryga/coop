package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/project"
)

func TestForkApprovalSnapshot(t *testing.T) {
	for _, isolated := range []bool{false, true} {
		boundary := "ordinary"
		if isolated {
			boundary = "isolated"
		}
		t.Run(boundary, func(t *testing.T) {
			t.Run("approval at creation is fork-local", func(t *testing.T) {
				repo := initRepo(t)
				writeForkNetworkPreflightProject(t, repo)
				commitForkNetworkPreflightFixture(t, repo)
				a := forkNetworkPreflightApp(t, repo)
				store := openForkApprovalSnapshotStore(t)
				approveForkApprovalSnapshotProject(t, store, repo)

				name := "approved"
				workspace := createForkApprovalSnapshotBoundary(t, repo, name, isolated)
				if err := checkForkApprovalSnapshot(a, workspace); err != nil {
					t.Fatalf("fork with the approved parent rules: %v", err)
				}
				assertForkApprovalSnapshotAnchor(t, store, repo, workspace)

				writeForkApprovalSnapshotRule(t, workspace, "changed.example")
				if err := checkForkApprovalSnapshot(a, workspace); err == nil || !strings.Contains(err.Error(), "Review it: coop approve") {
					t.Fatalf("fork with changed rules = %v, want the approval refusal", err)
				}

				writeForkApprovalSnapshotRule(t, repo, "changed.example")
				commitForkNetworkPreflightFixture(t, repo)
				approveForkApprovalSnapshotProject(t, store, repo)
				if err := checkForkApprovalSnapshot(a, workspace); err == nil || !strings.Contains(err.Error(), "Review it: coop approve") {
					t.Fatalf("fork after the parent approved changed rules = %v, want the fork-local approval refusal", err)
				}
			})

			t.Run("approval after creation is not inherited", func(t *testing.T) {
				repo := initRepo(t)
				writeForkNetworkPreflightProject(t, repo)
				commitForkNetworkPreflightFixture(t, repo)
				a := forkNetworkPreflightApp(t, repo)
				store := openForkApprovalSnapshotStore(t)

				workspace := createForkApprovalSnapshotBoundary(t, repo, "unapproved", isolated)
				if err := checkForkApprovalSnapshot(a, workspace); err == nil || !strings.Contains(err.Error(), "Review it: coop approve") {
					t.Fatalf("fork created before parent approval = %v, want the approval refusal", err)
				}

				approveForkApprovalSnapshotProject(t, store, repo)
				if err := checkForkApprovalSnapshot(a, workspace); err == nil || !strings.Contains(err.Error(), "Review it: coop approve") {
					t.Fatalf("earlier fork after the parent was approved = %v, want the approval refusal", err)
				}
				approval, err := store.Approval(workspace)
				if err != nil || approval != nil {
					t.Fatalf("earlier fork approval = (%+v, %v), want no inherited approval", approval, err)
				}
			})
		})
	}
}

func TestForkRefusedLoopListing(t *testing.T) {
	repo := initRepo(t)
	taskID := seedForkNetworkPreflightTask(t, repo)
	writeForkNetworkPreflightProject(t, repo)
	commitForkNetworkPreflightFixture(t, repo)
	a := forkNetworkPreflightApp(t, repo)
	t.Setenv(detachedChildEnv, "1")

	var code int
	var runErr error
	output := captureStdout(t, func() {
		captureStderr(t, func() {
			code, runErr = a.forkCreate([]string{"networked", "claude", "--loop", "-d"})
		})
	})
	if code != 1 || runErr == nil || !strings.Contains(runErr.Error(), "Review it: coop approve") {
		t.Fatalf("detached fork refusal = (%d, %v), want the network approval refusal", code, runErr)
	}
	if strings.Contains(output, "Started fork") {
		t.Fatalf("refused detached fork reported a successful start:\n%s", output)
	}
	if pathExists(forkspace.PidPath(repo, "networked")) {
		t.Fatal("refused detached fork left a worker pid record")
	}
	assertForkNetworkTaskUnassigned(t, repo, taskID)

	reason, _, _ := strings.Cut(runErr.Error(), "\n")
	listing := captureStdout(t, func() {
		if code, err := a.forkctl().ForkLs(nil); code != 0 || err != nil {
			t.Fatalf("fork ls = (%d, %v)", code, err)
		}
	})
	if !strings.Contains(listing, "networked · stopped") || !strings.Contains(listing, reason) || strings.Contains(listing, "networked · idle") {
		t.Errorf("fork ls does not show the stopped fork and its approval refusal:\n%s", listing)
	}
}

func openForkApprovalSnapshotStore(t *testing.T) *networkstate.Store {
	t.Helper()
	root, err := box.NetworkStatePath()
	if err != nil {
		t.Fatal(err)
	}
	store, err := networkstate.Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close network state store: %v", err)
		}
	})
	return store
}

func approveForkApprovalSnapshotProject(t *testing.T, store *networkstate.Store, repo string) {
	t.Helper()
	loaded, err := project.Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	mode := egress.Mode(loaded.Box.Egress)
	review, err := store.ReviewApproval(repo, mode, loaded.Box.EgressRules, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Approve(context.Background(), repo, mode, loaded.Box.EgressRules, nil, nil, review.Digest); err != nil {
		t.Fatal(err)
	}
}

func createForkApprovalSnapshotBoundary(t *testing.T, repo, name string, isolated bool) string {
	t.Helper()
	unlock, err := forkspace.LockState(repo, name)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := setupForkBoundary(nil, repo, name, isolated); err != nil {
		t.Fatal(err)
	}
	if _, err := bindForkBoundary(repo, name, isolated); err != nil {
		t.Fatal(err)
	}
	return forkspace.Workspace(repo, name)
}

func checkForkApprovalSnapshot(a *app, repo string) error {
	return box.CheckNetworkApproval(a.cfg.Clone(), box.RunSpec{Repo: repo, NetworkAdmission: true}, box.NetworkAdmission{})
}

func assertForkApprovalSnapshotAnchor(t *testing.T, store *networkstate.Store, parent, child string) {
	t.Helper()
	parentApproval, err := store.Approval(parent)
	if err != nil || parentApproval == nil {
		t.Fatalf("parent approval = (%+v, %v), want an approval", parentApproval, err)
	}
	childApproval, err := store.Approval(child)
	if err != nil || childApproval == nil {
		t.Fatalf("fork approval = (%+v, %v), want a creation-time snapshot", childApproval, err)
	}
	if childApproval.ProjectAnchor == parentApproval.ProjectAnchor {
		t.Fatalf("fork reused the parent network anchor %q", parentApproval.ProjectAnchor)
	}
}

func writeForkApprovalSnapshotRule(t *testing.T, repo, domain string) {
	t.Helper()
	path := filepath.Join(repo, project.File)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data := "box:\n  egress: filtered\n  egress_rules:\n    - to: {domain: " + domain + "}\n      protocol: tls\n      ports: [443]\n"
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}
