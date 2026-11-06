package box

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/networkstate"
)

func forkApprovalWorkspace(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func forkServiceCompose(secondSecret, namedVolume bool) []byte {
	body := "services:\n  keycloak:\n    image: quay.io/keycloak/keycloak:26\n    volumes:\n      - \"./tls.key:/run/tls.key:ro\"\n"
	if secondSecret {
		body += "      - \"./client.env:/run/client.env:ro\"\n"
	}
	if namedVolume {
		body += "      - \"existing-data:/var/lib/keycloak:ro\"\nvolumes:\n  existing-data:\n    external: true\n"
	}
	return []byte(body)
}

func writeForkServiceInputs(t *testing.T, workspace string, compose []byte, secondSecret bool) string {
	t.Helper()
	for name, data := range map[string]string{
		"tls.key":    "fixture-only placeholder, not a credential\n",
		"client.env": "FIXTURE=not-a-secret\n",
	} {
		if name == "client.env" && !secondSecret {
			continue
		}
		if err := os.WriteFile(filepath.Join(workspace, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(workspace, "compose.yml")
	if err := os.WriteFile(file, compose, 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

func prepareForkServiceReview(t *testing.T, workspace string, compose []byte, secondSecret, namedVolume bool) (string, ServiceApproval) {
	t.Helper()
	file := writeForkServiceInputs(t, workspace, compose, secondSecret)
	if namedVolume {
		state := filepath.Join(t.TempDir(), "volume-present")
		if err := os.WriteFile(state, []byte("fixture volume exists\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("COOP_TEST_VOLUME_STATE", state)
		t.Setenv("COOP_TEST_VOLUME_MOUNTPOINT", t.TempDir())
	}
	runtime := serviceReviewRuntime(t, "")
	review, err := ReviewServiceStart(workspace, file, runtime, true)
	if err != nil {
		t.Fatal(err)
	}
	if review.Secrets == nil {
		t.Fatal("fixture secret was not presented for approval")
	}
	if namedVolume {
		if !review.VolumeApprovalNeeded {
			t.Fatal("fixture external volume did not require approval")
		}
		if err := review.ApproveVolumes(); err != nil {
			t.Fatal(err)
		}
	}
	if err := review.Secrets.Approve(); err != nil {
		t.Fatal(err)
	}
	approval, ok := ApprovedServiceSecrets(workspace, file, compose)
	if !ok {
		t.Fatal("fixture service approval was not usable")
	}
	return file, approval
}

func TestSnapshotForkServiceApprovalCopiesReviewedAuthority(t *testing.T) {
	t.Setenv(ServiceStateRootEnv, t.TempDir())
	parent, fork := forkApprovalWorkspace(t), forkApprovalWorkspace(t)
	compose := forkServiceCompose(false, true)
	parentFile, parentApproval := prepareForkServiceReview(t, parent, compose, false, true)
	forkFile := writeForkServiceInputs(t, fork, compose, false)
	root, err := serviceApprovalRoot()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := snapshotForkServiceApprovals(parent, fork, root); err != nil {
		t.Fatal(err)
	}
	forkApproval, ok := ApprovedServiceSecrets(fork, forkFile, compose)
	if !ok {
		t.Fatal("fork did not receive the parent service approval")
	}
	if forkApproval.Anchor == parentApproval.Anchor || forkApproval.Workspace != fork || parentApproval.Workspace != parent {
		t.Fatalf("service approval anchors or workspaces were not rebound: parent=%+v fork=%+v", parentApproval, forkApproval)
	}
	if forkApproval.Digest != parentApproval.Digest || forkApproval.Digest != composeDigest(compose) || forkApproval.File != parentApproval.File || forkApproval.File != "compose.yml" {
		t.Fatalf("fork changed the reviewed Compose identity: parent=%+v fork=%+v", parentApproval, forkApproval)
	}
	if !reflect.DeepEqual(forkApproval.Paths, parentApproval.Paths) || !reflect.DeepEqual(forkApproval.Images, parentApproval.Images) {
		t.Fatalf("fork changed secret paths or image pins: parent=%+v fork=%+v", parentApproval, forkApproval)
	}
	if !reflect.DeepEqual(forkApproval.Volumes, parentApproval.Volumes) || !reflect.DeepEqual(forkApproval.VolumeBinding, parentApproval.VolumeBinding) {
		t.Fatalf("fork changed volume authority: parent=%+v fork=%+v", parentApproval, forkApproval)
	}
	if forkApproval.VolumeBinding.Endpoint == "" || forkApproval.VolumeBinding.DaemonID == "" || len(forkApproval.VolumeBinding.Objects) == 0 {
		t.Fatalf("fixture did not exercise a concrete volume binding: %+v", forkApproval.VolumeBinding)
	}
	if _, ok := ApprovedServiceSecrets(parent, parentFile, compose); !ok {
		t.Fatal("copying the service approval changed the parent's grant")
	}

	changed := forkServiceCompose(true, true)
	changedForkFile := writeForkServiceInputs(t, fork, changed, true)
	if _, ok := ApprovedServiceSecrets(fork, changedForkFile, changed); ok {
		t.Fatal("a changed Compose file reused the copied service approval")
	}
	parentChangedFile := writeForkServiceInputs(t, parent, changed, true)
	parentReview := serviceSecretReviewForApproval(t, parent, parentChangedFile)
	if parentReview == nil {
		t.Fatal("the second secret did not require a new parent review")
	}
	if err := parentReview.Approve(); err != nil {
		t.Fatal(err)
	}
	if _, ok := ApprovedServiceSecrets(parent, parentChangedFile, changed); !ok {
		t.Fatal("the later parent approval was not usable by the parent")
	}
	if _, ok := ApprovedServiceSecrets(fork, changedForkFile, changed); ok {
		t.Fatal("a later parent approval expanded the old fork's service grant")
	}
	if got, ok := ApprovedServiceSecrets(fork, forkFile, compose); !ok || !reflect.DeepEqual(got.Paths, parentApproval.Paths) {
		t.Fatalf("the original fork grant changed after parent re-approval: %+v, ok=%v", got, ok)
	}
}

func TestSnapshotForkServiceApprovalRefusesCopiedMarkerAndExposedAuthority(t *testing.T) {
	t.Run("copied marker", func(t *testing.T) {
		t.Setenv(ServiceStateRootEnv, t.TempDir())
		parent, fork := forkApprovalWorkspace(t), forkApprovalWorkspace(t)
		compose := forkServiceCompose(false, false)
		parentFile, _ := prepareForkServiceReview(t, parent, compose, false, false)
		forkFile := writeForkServiceInputs(t, fork, compose, false)
		marker, err := os.ReadFile(filepath.Join(parent, serviceApprovalMarker))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(fork, serviceApprovalMarker), marker, 0o600); err != nil {
			t.Fatal(err)
		}
		root, err := serviceApprovalRoot()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := snapshotForkServiceApprovals(parent, fork, root); err == nil || !strings.Contains(err.Error(), "new fork already has a service approval marker") {
			t.Fatalf("copied marker snapshot error = %v, want marker refusal", err)
		}
		if _, ok := ApprovedServiceSecrets(fork, forkFile, compose); ok {
			t.Fatal("a copied service marker authorized the fork")
		}
		if _, ok := ApprovedServiceSecrets(parent, parentFile, compose); !ok {
			t.Fatal("refusing a copied marker changed parent authority")
		}
	})

	t.Run("exposed authority root", func(t *testing.T) {
		t.Setenv(ServiceStateRootEnv, t.TempDir())
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		parent, fork := forkApprovalWorkspace(t), forkApprovalWorkspace(t)
		compose := forkServiceCompose(false, false)
		parentFile, _ := prepareForkServiceReview(t, parent, compose, false, false)
		forkFile := writeForkServiceInputs(t, fork, compose, false)
		root, err := serviceApprovalRoot()
		if err != nil {
			t.Fatal(err)
		}
		if err := SnapshotForkApprovals(t.Context(), &config.Config{ConfigDir: root}, parent, fork); err == nil || !strings.Contains(err.Error(), "must live outside every directory an agent can reach") {
			t.Fatalf("exposed service authority snapshot error = %v, want exposure refusal", err)
		}
		if _, ok := ApprovedServiceSecrets(fork, forkFile, compose); ok {
			t.Fatal("an exposed service authority root was copied into the fork")
		}
		if _, ok := ApprovedServiceSecrets(parent, parentFile, compose); !ok {
			t.Fatal("refusing the exposed root changed parent authority")
		}
	})
}

func TestSnapshotForkServiceApprovalRollsBackWhenNetworkSnapshotFails(t *testing.T) {
	t.Setenv(ServiceStateRootEnv, t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	parent, fork := forkApprovalWorkspace(t), forkApprovalWorkspace(t)
	compose := forkServiceCompose(false, false)
	parentFile, _ := prepareForkServiceReview(t, parent, compose, false, false)
	forkFile := writeForkServiceInputs(t, fork, compose, false)
	networkRoot, err := NetworkStatePath()
	if err != nil {
		t.Fatal(err)
	}
	store, err := networkstate.Open(networkRoot, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	requests := []egress.Rule{{To: egress.Destination{Domain: "parent.example.com"}, Protocol: "tls", Ports: []int{443}}}
	review, err := store.ReviewApproval(parent, egress.Filtered, requests, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Approve(t.Context(), parent, egress.Filtered, requests, nil, nil, review.Digest); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReviewApproval(fork, egress.Filtered, requests, nil, nil); err != nil {
		t.Fatal(err)
	}
	parentNetworkApproval, err := store.Approval(parent)
	if err != nil {
		t.Fatal(err)
	}
	err = SnapshotForkApprovals(t.Context(), nil, parent, fork)
	if err == nil || !strings.Contains(err.Error(), "new fork already has a network approval marker") {
		t.Fatalf("network snapshot error = %v, want the preexisting marker refusal", err)
	}
	if _, ok := ApprovedServiceSecrets(fork, forkFile, compose); ok {
		t.Fatal("service grant remained after the later network snapshot step failed")
	}
	if _, err := os.Lstat(filepath.Join(fork, serviceApprovalMarker)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed combined snapshot left a child service marker: %v", err)
	}
	if _, ok := ApprovedServiceSecrets(parent, parentFile, compose); !ok {
		t.Fatal("rolling back the child service grant changed parent authority")
	}
	if got, err := store.Approval(parent); err != nil || !reflect.DeepEqual(got, parentNetworkApproval) {
		t.Fatalf("failed combined snapshot changed parent network authority: %+v, %v", got, err)
	}
}

func commitForkBuildFixture(t *testing.T, repo string) {
	t.Helper()
	configRoot := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(configRoot, "global"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(configRoot, "system"))
	t.Setenv("GIT_CONFIG_COUNT", "2")
	t.Setenv("GIT_CONFIG_KEY_0", "maintenance.auto")
	t.Setenv("GIT_CONFIG_VALUE_0", "false")
	t.Setenv("GIT_CONFIG_KEY_1", "core.hooksPath")
	t.Setenv("GIT_CONFIG_VALUE_1", "/dev/null")
	t.Setenv("GIT_AUTHOR_NAME", "Coop Test")
	t.Setenv("GIT_AUTHOR_EMAIL", "coop-test@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "Coop Test")
	t.Setenv("GIT_COMMITTER_EMAIL", "coop-test@example.invalid")
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	run("add", "--all")
	run("commit", "--message", "fixture inputs")
}

func TestForkBuildApprovalSnapshotsOnlyTheApprovedIncludedFiles(t *testing.T) {
	x := newApprovedRebuildFixture(t)
	commitForkBuildFixture(t, x.repo)
	x.explicitBuild()
	if got := x.builds(); got != 1 {
		t.Fatalf("explicit parent build count = %d, want 1", got)
	}
	fork, err := forkspace.Setup(x.repo, "fork-build-approval")
	if err != nil {
		t.Fatal(err)
	}
	if err := x.f.store.SnapshotForkApprovals(t.Context(), x.repo, fork, ComposeProject(fork)+"-filtered"); err != nil {
		t.Fatal(err)
	}
	readinessErr := checkFilteredProjectReadiness(t.Context(), x.d, x.f.store, RunSpec{Repo: fork}, x.candidate.Runtime)
	var required *ProjectBuildRequired
	if !errors.As(readinessErr, &required) || !required.Approved {
		t.Fatalf("fork readiness = %v; want an approved build that only needs its child image", readinessErr)
	}
	if _, err := rebuildApprovedLocked(t.Context(), x.rt, x.cfg, x.d, x.f.store, fork, x.candidate, nil, io.Discard); err != nil {
		t.Fatalf("fork could not rebuild the copied approved files automatically: %v", err)
	}
	if got := x.builds(); got != 2 {
		t.Fatalf("build count after the approved automatic fork build = %d, want 2", got)
	}

	changed := reusableDockerfile + "RUN echo edited-in-fork > /opt/edited\n"
	childDockerfile := filepath.Join(fork, ".agent", "Dockerfile")
	if err := os.WriteFile(childDockerfile, []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}
	childRequiredErr := checkFilteredProjectReadiness(t.Context(), x.d, x.f.store, RunSpec{Repo: fork}, x.candidate.Runtime)
	if !errors.As(childRequiredErr, &required) || required.Approved {
		t.Fatalf("edited fork readiness = %v; want an unapproved build miss", childRequiredErr)
	}
	if _, err := rebuildApprovedLocked(t.Context(), x.rt, x.cfg, x.d, x.f.store, fork, x.candidate, nil, io.Discard); err == nil {
		t.Fatal("automatic rebuild ran an included build file changed in the fork")
	}
	if got := x.builds(); got != 2 {
		t.Fatalf("refused fork rebuild ran the builder: count = %d, want 2", got)
	}

	x.write(".agent/Dockerfile", changed)
	x.explicitBuild()
	if got := x.builds(); got != 3 {
		t.Fatalf("explicit parent re-approval build count = %d, want 3", got)
	}
	if err := checkFilteredProjectReadiness(t.Context(), x.d, x.f.store, RunSpec{Repo: x.repo}, x.candidate.Runtime); err != nil {
		t.Fatalf("later parent approval did not build its changed files: %v", err)
	}
	childRequiredErr = checkFilteredProjectReadiness(t.Context(), x.d, x.f.store, RunSpec{Repo: fork}, x.candidate.Runtime)
	if !errors.As(childRequiredErr, &required) || required.Approved {
		t.Fatalf("later parent approval expanded the edited fork: %v", childRequiredErr)
	}
	if _, err := rebuildApprovedLocked(t.Context(), x.rt, x.cfg, x.d, x.f.store, fork, x.candidate, nil, io.Discard); err == nil {
		t.Fatal("later parent approval authorized the edited fork's automatic rebuild")
	}
	if got := x.builds(); got != 3 {
		t.Fatalf("later parent approval caused an unauthorized fork build: count = %d, want 3", got)
	}
}

func TestForkBuildApprovalCreationClearsPreviousImage(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv(ServiceStateRootEnv, t.TempDir())
	parent, fork := forkApprovalWorkspace(t), forkApprovalWorkspace(t)
	root, err := NetworkStatePath()
	if err != nil {
		t.Fatal(err)
	}
	store, err := networkstate.Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	inputs, image := strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
	tag := filteredProjectTag(fork, "fixture-client")
	if err := store.ApproveProjectBuild(tag, inputs, image); err != nil {
		t.Fatal(err)
	}
	if err := SnapshotForkApprovals(t.Context(), nil, parent, fork); err != nil {
		t.Fatal(err)
	}
	if got := store.ApprovedProjectBuild(tag, inputs); got != "" {
		t.Fatalf("creation kept the previous checkout's image approval: %s", got)
	}
}

// A case-exposure approval keys on the same private anchor as the service approvals, and a parent can
// hold one without any. A new fork takes it like the others: the paths the parent accepted, nothing
// more, and nothing the parent accepts later.
func TestSnapshotForkServiceApprovalCopiesCaseExposure(t *testing.T) {
	t.Setenv(ServiceStateRootEnv, t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	parent, fork := forkApprovalWorkspace(t), forkApprovalWorkspace(t)
	anchor, err := serviceApprovalAnchor(parent, true)
	if err != nil {
		t.Fatal(err)
	}
	accept := func(paths ...string) {
		t.Helper()
		path, err := caseExposurePath(anchor)
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(caseExposureApproval{Version: 1, Anchor: anchor, Paths: paths})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	accepted := []string{"config/.env"}
	accept(accepted...)
	if pending := UnapprovedCaseExposures(parent, accepted); len(pending) != 0 {
		t.Fatalf("fixture parent exposure approval is not usable: %v", pending)
	}

	if err := SnapshotForkApprovals(t.Context(), nil, parent, fork); err != nil {
		t.Fatal(err)
	}
	if pending := UnapprovedCaseExposures(fork, accepted); len(pending) != 0 {
		t.Fatalf("the fork did not take the parent's case-exposure approval: pending %v", pending)
	}
	if forkAnchor, err := serviceApprovalAnchor(fork, false); err != nil || forkAnchor == anchor {
		t.Fatalf("fork anchor = %q, %v; want its own", forkAnchor, err)
	}
	if pending := UnapprovedCaseExposures(fork, []string{"other/.env"}); len(pending) != 1 {
		t.Errorf("the fork accepted a path the parent never approved: pending %v", pending)
	}
	accept("config/.env", "later/.env")
	if pending := UnapprovedCaseExposures(fork, []string{"later/.env"}); len(pending) != 1 {
		t.Errorf("a later parent approval reached the fork: pending %v", pending)
	}
}

// forkWithSnapshotApprovals approves a service file and network rules in parent, then snapshots them
// into a new fork the way fork creation does.
func forkWithSnapshotApprovals(t *testing.T) (parent, fork, parentFile, forkFile string, compose []byte, store *networkstate.Store) {
	t.Helper()
	t.Setenv(ServiceStateRootEnv, t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	parent, fork = forkApprovalWorkspace(t), forkApprovalWorkspace(t)
	compose = forkServiceCompose(false, false)
	parentFile, _ = prepareForkServiceReview(t, parent, compose, false, false)
	forkFile = writeForkServiceInputs(t, fork, compose, false)
	root, err := NetworkStatePath()
	if err != nil {
		t.Fatal(err)
	}
	store, err = networkstate.Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	requests := []egress.Rule{{To: egress.Destination{Domain: "parent.example.com"}, Protocol: "tls", Ports: []int{443}}}
	review, err := store.ReviewApproval(parent, egress.Filtered, requests, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Approve(t.Context(), parent, egress.Filtered, requests, nil, nil, review.Digest); err != nil {
		t.Fatal(err)
	}
	if err := SnapshotForkApprovals(t.Context(), nil, parent, fork); err != nil {
		t.Fatal(err)
	}
	if _, ok := ApprovedServiceSecrets(fork, forkFile, compose); !ok {
		t.Fatal("fixture fork did not receive the parent's service approval")
	}
	if approval, err := store.Approval(fork); err != nil || approval == nil {
		t.Fatalf("fixture fork network approval = %+v, %v", approval, err)
	}
	return parent, fork, parentFile, forkFile, compose, store
}

func TestRetireForkApprovalsRetiresOnlyTheForks(t *testing.T) {
	parent, fork, parentFile, forkFile, compose, store := forkWithSnapshotApprovals(t)
	other := forkApprovalWorkspace(t)
	otherFile, _ := prepareForkServiceReview(t, other, compose, false, false)
	anchor, err := serviceApprovalAnchor(fork, false)
	if err != nil {
		t.Fatal(err)
	}
	exposed := []string{"secrets/.env"}
	casePath, err := caseExposurePath(anchor)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(caseExposureApproval{Version: 1, Anchor: anchor, Paths: exposed})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(casePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(casePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if pending := UnapprovedCaseExposures(fork, exposed); len(pending) != 0 {
		t.Fatalf("fixture case-exposure approval is not usable: %v", pending)
	}
	inputs, image := strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
	tag := filteredProjectTag(fork, "fixture-client")
	if err := store.ApproveProjectBuild(tag, inputs, image); err != nil {
		t.Fatal(err)
	}
	parentNetwork, err := store.Approval(parent)
	if err != nil {
		t.Fatal(err)
	}

	for range 2 { // a retry after a finished retirement finds nothing left and succeeds
		if err := RetireForkApprovals(t.Context(), parent, fork); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := ApprovedServiceSecrets(fork, forkFile, compose); ok {
		t.Error("the fork kept its service approval")
	}
	if pending := UnapprovedCaseExposures(fork, exposed); len(pending) != len(exposed) {
		t.Errorf("the fork kept its case-exposure approval: pending %v", pending)
	}
	root, err := serviceApprovalRoot()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{casePath, filepath.Join(root, "service-"+anchor+".anchor"), filepath.Join(fork, serviceApprovalMarker), filepath.Join(fork, networkstate.ProjectApprovalMarker)} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("retirement left %s: %v", path, err)
		}
	}
	if approval, err := store.Approval(fork); err != nil || approval != nil {
		t.Errorf("the fork kept its network approval: %+v, %v", approval, err)
	}
	if got := store.ApprovedProjectBuild(tag, inputs); got != "" {
		t.Errorf("the fork kept its image grant: %s", got)
	}
	if _, ok := ApprovedServiceSecrets(parent, parentFile, compose); !ok {
		t.Error("retiring the fork's approvals removed the parent's service approval")
	}
	if _, ok := ApprovedServiceSecrets(other, otherFile, compose); !ok {
		t.Error("retiring the fork's approvals removed another project's service approval")
	}
	if got, err := store.Approval(parent); err != nil || !reflect.DeepEqual(got, parentNetwork) {
		t.Errorf("retiring the fork's approvals changed the parent's network approval: %+v, %v", got, err)
	}
}

func TestRetireForkApprovalsKeepsTheForkProvableAfterAFailure(t *testing.T) {
	parent, fork, parentFile, forkFile, compose, _ := forkWithSnapshotApprovals(t)
	root, err := serviceApprovalRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })
	if err := RetireForkApprovals(t.Context(), parent, fork); err == nil {
		t.Fatal("retirement in a folder it cannot change reported success")
	}
	if _, ok := ApprovedServiceSecrets(fork, forkFile, compose); !ok {
		t.Fatal("a failed retirement lost the proof a retry needs")
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := RetireForkApprovals(t.Context(), parent, fork); err != nil {
		t.Fatalf("retry after the failure: %v", err)
	}
	if _, ok := ApprovedServiceSecrets(fork, forkFile, compose); ok {
		t.Error("the retry left the fork's service approval")
	}
	if _, ok := ApprovedServiceSecrets(parent, parentFile, compose); !ok {
		t.Error("the retry removed the parent's service approval")
	}
}

func TestRetireForkApprovalsWithoutACheckoutDoesNothing(t *testing.T) {
	t.Setenv(ServiceStateRootEnv, t.TempDir())
	if err := RetireForkApprovals(t.Context(), forkApprovalWorkspace(t), filepath.Join(t.TempDir(), "gone")); err != nil {
		t.Fatalf("retirement for a missing checkout = %v, want nothing to do", err)
	}
}
