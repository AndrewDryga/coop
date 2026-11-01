package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/teststate"
)

func TestMain(m *testing.M) { os.Exit(teststate.Isolate(m.Run)) }

func TestFixtureCleanupPreservesReplacement(t *testing.T) {
	parent := t.TempDir()
	f, err := newFixture(parent)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.seed(); err != nil {
		t.Fatal(err)
	}
	old := f.path + "-moved"
	if err := os.Rename(f.path, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(f.path, 0700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(f.path, "user-file")
	if err := os.WriteFile(keep, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.close(); err == nil {
		t.Fatal("replaced fixture accepted")
	}
	if data, err := os.ReadFile(keep); err != nil || string(data) != "keep" {
		t.Fatalf("replacement touched: %q %v", data, err)
	}
	entries, err := os.ReadDir(old)
	if err != nil || len(entries) == 0 {
		t.Fatalf("changed fixture should be preserved: %v %v", entries, err)
	}
}

func TestFixtureWriteCannotFollowEscapingLink(t *testing.T) {
	parent := t.TempDir()
	f, err := newFixture(parent)
	if err != nil {
		t.Fatal(err)
	}
	defer f.close()
	outside := filepath.Join(parent, "outside")
	if err := os.WriteFile(outside, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.root.Symlink(outside, "link"); err != nil {
		t.Fatal(err)
	}
	if err := f.write("link", "overwrite"); err == nil {
		t.Fatal("escaping link followed")
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "keep" {
		t.Fatalf("outside changed: %q %v", data, err)
	}
}

func TestIdentityRequiresMatchingFixtureAndCgroup(t *testing.T) {
	for _, data := range []string{`{}`, `{"cgroup":1}`, `{"cgroup":1,"fixture":{"result":"read","body":"another"}}`, `{"cgroup":0,"fixture":{"result":"read","body":"ours"}}`, `{"cgroup":1`} {
		if _, err := cgroupIdentity([]byte(data), "ours"); err == nil {
			t.Fatalf("accepted %s", data)
		}
	}
	if id, err := cgroupIdentity([]byte(`{"cgroup":42,"fixture":{"result":"read","body":"ours"}}`), "ours"); err != nil || id != 42 {
		t.Fatalf("%d %v", id, err)
	}
}

func TestEvidenceCannotTurnMissingOrTruncatedResultsIntoDenial(t *testing.T) {
	for _, data := range []string{`{}`, `null`, `{".env":{"result":"error","errno":1}}`, `{".env":`} {
		if _, err := reads([]byte(data), "read"); err == nil {
			t.Fatalf("accepted incomplete reads: %s", data)
		}
	}
	values := map[string]readResult{}
	for _, name := range readPaths {
		values[name] = readResult{Result: "error", Errno: 1}
	}
	values["replacement-hard"] = readResult{Result: "read", Body: "INERT_REPLACEMENT"}
	data, _ := json.Marshal(values)
	parsed, err := reads(data, "read")
	if err != nil {
		t.Fatal(err)
	}
	if paths := exposure(parsed); len(paths) != 1 || paths[0] != "replacement-hard" {
		t.Fatalf("missed fresh inode alias: %v", paths)
	}
	delete(values, "env-hard")
	values["junk"] = readResult{Result: "error", Errno: 1}
	data, _ = json.Marshal(values)
	if _, err := reads(data, "read"); err == nil {
		t.Fatal("missing alias replaced with junk accepted")
	}
	delete(values, "junk")
	values["env-hard"] = readResult{Result: "error", Errno: 5}
	data, _ = json.Marshal(values)
	if _, err := reads(data, "read"); err == nil {
		t.Fatal("I/O failure counted as denial")
	}
}

func TestAcquisitionRefusesSymlinkAndPopulatedDirectory(t *testing.T) {
	parentPath := t.TempDir()
	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	if err := parent.Mkdir("user", 0700); err != nil {
		t.Fatal(err)
	}
	if err := parent.WriteFile("user/keep", []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := parent.Symlink("user", "fixture"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"fixture", "user"} {
		if root, _, err := openFixture(parent, name); err == nil {
			root.Close()
			t.Fatalf("accepted %s", name)
		}
	}
	if data, err := parent.ReadFile("user/keep"); err != nil || string(data) != "keep" {
		t.Fatal("user data changed")
	}
}

func TestCleanupPreservesUnexpectedEntries(t *testing.T) {
	f, err := newFixture(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.seed(); err != nil {
		t.Fatal(err)
	}
	if err := f.root.WriteFile("user-data", []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.close(); err == nil {
		t.Fatal("unknown entry silently removed")
	}
	if data, err := os.ReadFile(filepath.Join(f.path, "user-data")); err != nil || string(data) != "keep" {
		t.Fatal("unknown data changed")
	}
}

func TestFixtureManifestDoesNotAcceptRootOrNestedSiblings(t *testing.T) {
	for _, path := range []string{"public.txt", ".env", "private/key", "nested", "workspace/nested/old-nested/.env", "workspace/old-nested/nested/.env"} {
		if fixturePath(path) {
			t.Fatalf("unexpected path accepted: %s", path)
		}
	}
}

func TestFixtureBindRejectsReplacedChild(t *testing.T) {
	f, err := newFixture(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.seed(); err != nil {
		t.Fatal(err)
	}
	defer f.close()
	if err := f.verifyBind("workspace/nested"); err != nil {
		t.Fatal(err)
	}
	if err := f.root.Rename("workspace/nested", "workspace/old-nested"); err != nil {
		t.Fatal(err)
	}
	if err := f.root.Symlink("old-nested", "workspace/nested"); err != nil {
		t.Fatal(err)
	}
	if err := f.verifyBind("workspace/nested"); err == nil {
		t.Fatal("substituted bind source accepted")
	}
}

func TestCustodyRequiresDeniedWritesAndNoPrivileges(t *testing.T) {
	status := "CapEff:\t0000000000000000\nCapPrm:\t0000000000000000\nCapBnd:\t0000000000000000\nNoNewPrivs:\t1\n"
	good := map[string]any{"cgroup": 30, "loader": 30, "status": status}
	if err := custody(raw(good)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []json.RawMessage{raw(map[string]any{}), raw(map[string]any{"cgroup": "writable", "loader": 30, "status": status}), raw(map[string]any{"cgroup": 30, "loader": 30, "status": strings.ReplaceAll(status, "NoNewPrivs:\t1", "NoNewPrivs:\t0")})} {
		if err := custody(bad); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}

func TestBoundedOutputRemainsExplicitlyIncomplete(t *testing.T) {
	var output boundedBuffer
	data := []byte(strings.Repeat("x", 256<<10))
	if n, err := output.Write(data); n != len(data) || err != nil {
		t.Fatalf("write=%d,%v", n, err)
	}
	if !output.overflow || len(output.String()) != 128<<10 {
		t.Fatalf("overflow=%v size=%d", output.overflow, len(output.String()))
	}
}

func TestMountRejectsGrammarInjection(t *testing.T) {
	for _, path := range []string{"/tmp/a,readonly", "/tmp/a\n", "/tmp/a\x00"} {
		if _, err := mount(path, "/workspace", false); err == nil {
			t.Fatalf("accepted %q", path)
		}
	}
}
