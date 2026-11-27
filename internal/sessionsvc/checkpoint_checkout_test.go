package sessionsvc

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

func TestFleetCheckoutProtectsLocalWork(t *testing.T) {
	const offer = "operator-fleet-job:example"
	fixture := newUpstreamCheckpointCase(t, offer)
	sessionWorkspaceWrite(t, filepath.Join(fixture.source.Workspace, "tracked.txt"), "worker change\n")
	sessionWorkspaceWrite(t, filepath.Join(fixture.source.Workspace, "created.txt"), "worker result\n")
	captured, bundle := fixture.captureBundle(t, "result-export")
	header, err := json.Marshal(captured.Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	wire := append(append(header, '\n'), bundle...)
	parent := t.TempDir()
	sessionWorkspaceWrite(t, filepath.Join(fixture.upstream, "tracked.txt"), "new local edit\n")
	working, err := checkoutFleetResult(t.Context(), offer, fixture.upstream, parent, bytes.NewReader(wire), fixtureCheckpointReserve)
	if err != nil {
		t.Fatal(err)
	}
	for file, want := range map[string]string{
		filepath.Join(working, "tracked.txt"):          "worker change\n",
		filepath.Join(working, "created.txt"):          "worker result\n",
		filepath.Join(fixture.upstream, "tracked.txt"): "new local edit\n",
	} {
		data, err := os.ReadFile(file)
		if err != nil || string(data) != want {
			t.Fatalf("read %s: %q, %v", file, data, err)
		}
	}
	for _, tc := range []struct {
		offer string
		data  []byte
	}{{"operator-fleet-job:other", wire}, {offer, wire[:len(wire)-1]}} {
		if _, err := checkoutFleetResult(t.Context(), tc.offer, fixture.upstream, parent, bytes.NewReader(tc.data), fixtureCheckpointReserve); err == nil {
			t.Fatal("accepted wrong job or incomplete result")
		}
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 1 {
		t.Fatalf("incomplete checkout remained: %v, %v", entries, err)
	}
}

func TestFleetCheckoutUsesPinnedObjectsFromEditedSubmodule(t *testing.T) {
	leaf, gitLeaf := gitrepo.New(t)
	sessionWorkspaceWrite(t, filepath.Join(leaf, "code"), "saved by worker\n")
	gitLeaf("add", "code")
	gitLeaf("commit", "-qm", "initial")
	donor, git := gitrepo.New(t)
	git("-c", "protocol.file.allow=always", "submodule", "add", "--quiet", leaf, "child")
	git("commit", "-qm", "pinned source")
	base := gitOut(donor, "rev-parse", "HEAD")

	// The donor has moved its gitlink, and the operator has local edits as well.
	git("-C", "child", "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-qm", "new child")
	git("add", "child")
	git("commit", "-qm", "new gitlink")
	sessionWorkspaceWrite(t, filepath.Join(donor, "child", "code"), "uncommitted operator edit\n")
	target := t.TempDir()
	if err := initializeCheckpointRepository(t.Context(), donor, target, base); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runSessionCompanionGitContext(t.Context(), target, sessionWorkspaceGitOutputLimit, "reset", "--hard", base); err != nil {
		t.Fatal(err)
	}
	if err := materializeFleetResultTree(t.Context(), donor, target, base); err != nil {
		t.Fatal(err)
	}
	for dir, text := range map[string]string{donor: "uncommitted operator edit\n", target: "saved by worker\n"} {
		data, err := os.ReadFile(filepath.Join(dir, "child", "code"))
		if err != nil || string(data) != text {
			t.Fatalf("%s: %q, %v", dir, data, err)
		}
	}
	info, err := os.Stat(filepath.Join(target, "child", ".git"))
	if err != nil || !info.IsDir() {
		t.Fatal("restored submodule must own an independent Git directory")
	}
}

func TestFleetCheckoutRefusesInsufficientStorage(t *testing.T) {
	parent := t.TempDir()
	ctx, stop, err := checkpointDiskContext(t.Context(), []string{parent}, 1<<62, func(int64) int64 { return 0 })
	if ctx != nil || stop != nil || err == nil || !strings.Contains(err.Error(), "storage") {
		t.Fatalf("disk refusal: %v", err)
	}
}

func TestFleetArtifactsRequireSavedTaskOwnershipAndExactFiles(t *testing.T) {
	const offer = "operator-fleet-job:artifacts"
	fixture := newUpstreamCheckpointCase(t, offer)
	queue := filepath.Join(fixture.source.Workspace, tasks.TasksRoot)
	item, ok, err := tasks.CurrentTask(queue, fixture.source.WorkspaceTask.ID)
	if err != nil || !ok {
		t.Fatalf("task: %v %v", ok, err)
	}
	files := map[string]string{"tests/junit.xml": "<testsuite/>", "binary.dat": string([]byte{0, 255, 20, 0, 90})}
	for name, data := range files {
		path := filepath.Join(item.Dir, "artifacts", name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		sessionWorkspaceWrite(t, path, data)
	}
	captured, bundle := fixture.captureBundle(t, "artifact-export")
	desc, err := json.Marshal(captured.Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	wire := append(append(desc, '\n'), bundle...)
	parent := t.TempDir()
	read := func(job string, data []byte, names ...string) (FleetArtifacts, error) {
		return fleetResultArtifacts(t.Context(), job, parent, names, bytes.NewReader(data), fixtureCheckpointReserve)
	}
	list, err := read(offer, wire)
	if err != nil || list.Directory != "" || len(list.Files) != 2 {
		t.Fatalf("list: %+v %v", list, err)
	}
	output, err := read(offer, wire, "tests/junit.xml", "binary.dat")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range output.Files {
		data, err := os.ReadFile(filepath.Join(output.Directory, item.Name))
		if err != nil || string(data) != files[item.Name] || item.Bytes != int64(len(data)) || item.SHA256 != fmt.Sprintf("%x", sha256.Sum256(data)) || item.MediaType == "" {
			t.Fatalf("artifact: %+v %v", item, err)
		}
	}
	for _, bad := range []string{"../task.md", "/tmp/private", "absent", "binary.dat/../task.md"} {
		if _, err := read(offer, wire, "binary.dat", bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	if _, err := read("another-job", wire, "binary.dat"); err == nil {
		t.Fatal("exported another job")
	}
	if _, err := read(offer, wire[:len(wire)-1], "binary.dat"); err == nil {
		t.Fatal("accepted truncated checkpoint")
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 1 {
		t.Fatalf("partial artifacts: %v %v", entries, err)
	}
}
