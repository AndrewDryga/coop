package box

import (
	"context"
	"encoding/json"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/fsfilter"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// expiredWait is a context whose deadline has passed but which is not yet cancelled, so a lock wait
// gives up through its own deadline check rather than through ctx.Err.
type expiredWait struct{ context.Context }

func (expiredWait) Deadline() (time.Time, bool) { return time.Now().Add(-time.Second), true }

// A launch that gives up waiting on the hidden-file lock names the checkout, not the credential store.
func TestAHiddenFilesLockTimeoutNamesTheCheckout(t *testing.T) {
	pinHome(t)
	repo := t.TempDir()
	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- withHiddenFilesLock(t.Context(), repo, func() error { close(held); <-release; return nil })
	}()
	<-held
	err := withHiddenFilesLock(expiredWait{t.Context()}, repo, func() error { return nil })
	close(release)
	if waitErr := <-done; waitErr != nil {
		t.Fatal(waitErr)
	}
	if err == nil || strings.Contains(err.Error(), "credential") || !strings.Contains(err.Error(), "hidden-file record") || !strings.Contains(err.Error(), repo) {
		t.Fatalf("a busy hidden-file lock = %v, want a message naming %s and its hidden-file record", err, repo)
	}
}

// launch computes a launch's mounts and remembers what they hid, as box.Run does.
func launch(t *testing.T, repo string) []Mount {
	t.Helper()
	mounts, err := ComputeMounts(repo, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if err := RememberHiddenFiles(repo, "/workspace", mounts); err != nil {
		t.Fatal(err)
	}
	return mounts
}

// A box can move a hidden file, or the folder holding it, to a path no rule names. Whatever a
// launch hid stays hidden there, from the next launch and from Coop's own tools: a file a wildcard
// path rule hid, and a folder a nested .coopignore hid.
func TestAHiddenFileStaysHiddenAfterABoxMovesIt(t *testing.T) {
	pinHome(t)
	t.Run("wildcard path rule", func(t *testing.T) {
		repo := t.TempDir()
		writeTree(t, repo, map[string]string{".coopignore": "config/*.yml\n", "config/prod.yml": "secret: 1\n"})
		if m := find(launch(t, repo), "/workspace/config/prod.yml"); m == nil || m.Kind != Decoy {
			t.Fatalf("config/prod.yml was not hidden at first: %+v", m)
		}
		if err := os.Rename(filepath.Join(repo, "config"), filepath.Join(repo, "cfg")); err != nil {
			t.Fatal(err)
		}
		if m := find(launch(t, repo), "/workspace/cfg/prod.yml"); m == nil || m.Kind != Decoy {
			t.Fatalf("the moved file was shown to the next launch: %+v", m)
		}
		if hidden, err := NewShadowDecider(repo)("cfg/prod.yml"); err != nil || !hidden {
			t.Fatalf("Coop's host tools read the moved file as visible: %v, %v", hidden, err)
		}
		// Still hidden on every launch after, not only the first one after the move.
		if m := find(launch(t, repo), "/workspace/cfg/prod.yml"); m == nil || m.Kind != Decoy {
			t.Fatalf("the moved file came back on a later launch: %+v", m)
		}
	})
	t.Run("nested .coopignore", func(t *testing.T) {
		repo := t.TempDir()
		writeTree(t, repo, map[string]string{"config/.coopignore": "secrets/\n", "config/secrets/token": "t0k3n\n", "other/readme": "x\n"})
		if m := find(launch(t, repo), "/workspace/config/secrets"); m == nil || m.Kind != DirDecoy {
			t.Fatalf("config/secrets was not hidden at first: %+v", m)
		}
		if err := os.Rename(filepath.Join(repo, "config", "secrets"), filepath.Join(repo, "other", "secrets")); err != nil {
			t.Fatal(err)
		}
		if m := find(launch(t, repo), "/workspace/other/secrets"); m == nil || m.Kind != DirDecoy {
			t.Fatalf("the moved folder was shown to the next launch: %+v", m)
		}
		if hidden, err := NewShadowDecider(repo)("other/secrets/token"); err != nil || !hidden {
			t.Fatalf("Coop's host tools read a file in the moved folder as visible: %v, %v", hidden, err)
		}
	})
}

// A mount-protected launch walks the repository once, and still hides a hard link to a file that
// same walk hid: the record it writes names the file at the hidden path, so the link is an alias.
func TestALaunchWalksTheRepositoryOnceAndHidesHardLinks(t *testing.T) {
	pinHome(t)
	repo := t.TempDir()
	writeTree(t, repo, map[string]string{".env": "KEY=1\n", "src/main.go": "package main\n"})
	if err := os.Link(filepath.Join(repo, ".env"), filepath.Join(repo, "src", "copy")); err != nil {
		t.Fatal(err)
	}
	walks := 0
	original := onMountWalk
	onMountWalk = func() { walks++ }
	t.Cleanup(func() { onMountWalk = original })
	mounts, _, err := launchMounts(&config.Config{}, repo, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if walks != 1 {
		t.Fatalf("a launch walked the repository %d times, want once", walks)
	}
	for _, target := range []string{"/workspace/.env", "/workspace/src/copy"} {
		if m := find(mounts, target); m == nil || m.Kind != Decoy {
			t.Fatalf("%s was not hidden on the first launch: %+v", target, mounts)
		}
	}
	if got := loadHiddenFiles(repo); len(got) != 1 || !slices.ContainsFunc(slices.Collect(maps.Values(got)), func(o hiddenOrigin) bool { return o.path == ".env" }) {
		t.Fatalf("remembered %v, want .env alone", got)
	}
	// The next launch's own decider agrees, so both launches hide the same paths.
	walks = 0
	again, _, err := launchMounts(&config.Config{}, repo, "/workspace")
	if err != nil || walks != 1 || !slices.EqualFunc(again, mounts, func(a, b Mount) bool {
		return a.Kind == b.Kind && a.Target == b.Target && a.Source == b.Source && a.RO == b.RO && slices.Equal(a.Spellings, b.Spellings)
	}) {
		t.Fatalf("the next launch = %+v (%d walks), %v; want %+v", again, walks, err, mounts)
	}
}

// renumberHiddenRecord rewrites repo's hidden-file record as if it was written before a reboot gave
// the repository's filesystem another device number: every file recorded on the root's device, and
// the root's device itself, move to old. It returns the live root device.
func renumberHiddenRecord(t *testing.T, repo string, old uint64) uint64 {
	t.Helper()
	path, err := hiddenFilesPath(repo)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record hiddenFilesRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	live, ok := rootDevice(repo)
	if !ok || record.RootDev != live || len(record.Files) == 0 {
		t.Fatalf("record = %+v, want the root's live device %d and its files", record, live)
	}
	for i := range record.Files {
		if record.Files[i].Dev == live {
			record.Files[i].Dev = old
		}
	}
	record.RootDev = old
	if data, err = json.Marshal(record); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return live
}

// A reboot can renumber the repository's device, which no remembered identity then matches. A file
// a box moved must stay hidden all the same, under both the mount backend and the kernel one.
func TestAMovedHiddenFileStaysHiddenAfterTheDeviceIsRenumbered(t *testing.T) {
	pinHome(t)
	repo := t.TempDir()
	writeTree(t, repo, map[string]string{".coopignore": "config/*.yml\n", "config/prod.yml": "secret: 1\n"})
	launch(t, repo)
	if err := os.Rename(filepath.Join(repo, "config"), filepath.Join(repo, "cfg")); err != nil {
		t.Fatal(err)
	}
	live := renumberHiddenRecord(t, repo, 1<<40+7)
	if hidden, err := NewShadowDecider(repo)("cfg/prod.yml"); err != nil || !hidden {
		t.Fatalf("Coop's host tools read the moved file as visible after a renumbering: %v, %v", hidden, err)
	}
	inventory, err := filesystemObjects(repo)
	if err != nil {
		t.Fatal(err)
	}
	flags, err := filesystemNamedFlags(repo, inventory, loadHiddenFiles(repo))
	if err != nil || flags["cfg/prod.yml"]&fsfilter.Secret == 0 {
		t.Fatalf("the kernel backend showed the moved file after a renumbering: %v, %v", flags["cfg/prod.yml"], err)
	}
	if m := find(launch(t, repo), "/workspace/cfg/prod.yml"); m == nil || m.Kind != Decoy {
		t.Fatalf("the moved file was shown to the next launch after a renumbering: %+v", m)
	}
	// That launch rewrote the record on the live device, still remembering the file.
	if remembered := loadHiddenFiles(repo); len(remembered) != 1 || remembered[fileIdentity{dev: live, ino: inodeOf(t, filepath.Join(repo, "cfg/prod.yml"))}].path != "config/prod.yml" {
		t.Fatalf("the rewritten record = %v, want the moved file at its first path", remembered)
	}
}

// Only files recorded on the root's own device are re-anchored: one recorded on another filesystem
// mounted inside the repository keeps its device, so it never names an unrelated file on the root.
func TestAHiddenFileOnAnotherFilesystemKeepsItsDevice(t *testing.T) {
	pinHome(t)
	repo := t.TempDir()
	live, ok := rootDevice(repo)
	if !ok {
		t.Fatal("no root device")
	}
	record := hiddenFilesRecord{Version: hiddenFilesVersion, Files: []hiddenFile{{Dev: 11, Ino: 1, Path: "root.env"}, {Dev: 12, Ino: 2, Path: "mnt/nested.env"}}}
	path, err := hiddenFilesPath(repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	record.RootDev = 11
	data, _ := json.Marshal(record)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	want := rememberedHidden{{dev: live, ino: 1}: {path: "root.env"}, {dev: 12, ino: 2}: {path: "mnt/nested.env"}}
	if got := loadHiddenFiles(repo); !maps.Equal(got, want) {
		t.Fatalf("loaded %v, want %v", got, want)
	}
}

// Linux reuses a deleted file's inode number, so a remembered identity can name an ordinary file
// the agent wrote next. A record entry whose proof does not match the file under its identity
// leaves that file visible to both backends and to the next launch, and forgets it; one whose
// proof matches is a moved secret and stays hidden.
func TestAReusedInodeDoesNotHideAnOrdinaryFile(t *testing.T) {
	pinHome(t)
	repo := t.TempDir()
	writeTree(t, repo, map[string]string{"notes.txt": "ordinary\n"})
	live, ok := rootDevice(repo)
	if !ok {
		t.Fatal("no root device")
	}
	path, err := hiddenFilesPath(repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	original := hiddenFileProof
	t.Cleanup(func() { hiddenFileProof = original })
	for _, tc := range []struct {
		now    string
		hidden bool
	}{{"gen:2", false}, {"gen:1", true}} {
		record := hiddenFilesRecord{Version: hiddenFilesVersion, RootDev: live,
			Files: []hiddenFile{{Dev: live, Ino: inodeOf(t, filepath.Join(repo, "notes.txt")), Path: ".env", Proof: "gen:1"}}}
		data, _ := json.Marshal(record)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		hiddenFileProof = func(string, fs.FileInfo) string { return tc.now }
		if hidden, err := NewShadowDecider(repo)("notes.txt"); err != nil || hidden != tc.hidden {
			t.Fatalf("proof %s: Coop's host tools read notes.txt hidden=%v, %v; want %v", tc.now, hidden, err, tc.hidden)
		}
		inventory, err := filesystemObjects(repo)
		if err != nil {
			t.Fatal(err)
		}
		flags, err := filesystemNamedFlags(repo, inventory, loadHiddenFiles(repo))
		if err != nil || (flags["notes.txt"]&fsfilter.Secret != 0) != tc.hidden {
			t.Fatalf("proof %s: the kernel backend flagged notes.txt %v, %v; want hidden=%v", tc.now, flags["notes.txt"], err, tc.hidden)
		}
		if m := find(launch(t, repo), "/workspace/notes.txt"); (m != nil) != tc.hidden {
			t.Fatalf("proof %s: the next launch covered notes.txt with %+v; want hidden=%v", tc.now, m, tc.hidden)
		}
		if remembered := loadHiddenFiles(repo); (len(remembered) == 1) != tc.hidden {
			t.Fatalf("proof %s: remembered %v after the launch; want it kept only while it matches", tc.now, remembered)
		}
	}
}

// Where the filesystem keeps no inode generation, a file proves itself by its size and modification
// time: a move keeps them and an edit changes them. A folder has only its identity, since its size
// and time change with every entry written in it. Where it keeps one, the generation alone decides.
func TestAHiddenFileProofFallsBackToSizeAndTime(t *testing.T) {
	original := inodeGenerationOf
	t.Cleanup(func() { inodeGenerationOf = original })
	inodeGenerationOf = func(string, fs.FileInfo) (uint32, bool) { return 0, false }
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"secret": "KEY=1\n", "folder/inside": "x\n"})
	proof := func(rel string) string {
		info, err := os.Lstat(filepath.Join(dir, rel))
		if err != nil {
			t.Fatal(err)
		}
		return hiddenFileProof(filepath.Join(dir, rel), info)
	}
	before := proof("secret")
	if !strings.HasPrefix(before, "stat:") {
		t.Fatalf("fallback proof = %q, want size and time", before)
	}
	if err := os.Rename(filepath.Join(dir, "secret"), filepath.Join(dir, "moved")); err != nil {
		t.Fatal(err)
	}
	if got := proof("moved"); got != before {
		t.Fatalf("a move changed the proof: %q -> %q", before, got)
	}
	if err := os.WriteFile(filepath.Join(dir, "moved"), []byte("KEY=22\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := proof("moved"); got == before {
		t.Fatal("an edit kept the size-and-time proof")
	}
	if got := proof("folder"); got != "" {
		t.Fatalf("a folder's fallback proof = %q, want none (identity alone)", got)
	}
	inodeGenerationOf = func(string, fs.FileInfo) (uint32, bool) { return 7, true }
	if got := proof("moved"); got != "gen:7" {
		t.Fatalf("generation proof = %q, want gen:7", got)
	}
}

func inodeOf(t *testing.T, path string) uint64 {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	id, ok := identityOf(info)
	if !ok {
		t.Fatal("no identity")
	}
	return id.ino
}

// A hidden folder Coop cannot read (a database folder a container user owns, listed in .coopignore)
// does not fail a launch once a remembered hidden file is replaced: the alias walk skips it, since
// the box sees only its decoy.
func TestAnUnreadableHiddenFolderDoesNotFailTheAliasWalk(t *testing.T) {
	pinHome(t)
	repo := t.TempDir()
	writeTree(t, repo, map[string]string{".coopignore": "pgdata/\n", "pgdata/base/1": "x\n", ".env": "KEY=1\n"})
	launch(t, repo)
	if err := os.Chmod(filepath.Join(repo, "pgdata"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(repo, "pgdata"), 0o700) })
	// An atomic save gives .env a new inode, so the remembered one is missing and the walk runs.
	if err := os.Remove(filepath.Join(repo, ".env")); err != nil {
		t.Fatal(err)
	}
	writeTree(t, repo, map[string]string{".env": "KEY=2\n"})
	for range 2 {
		launch(t, repo)
	}
}

// Remembering never overrides the person's rules where the file still is: back at its path, a file
// whose rule was taken out of .coopignore is shown, and forgotten.
func TestAFileStillAtItsPathFollowsTheCurrentRules(t *testing.T) {
	pinHome(t)
	repo := t.TempDir()
	writeTree(t, repo, map[string]string{".coopignore": "config/*.yml\n", "config/prod.yml": "secret: 1\n"})
	launch(t, repo)
	if err := os.WriteFile(filepath.Join(repo, ".coopignore"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if m := find(launch(t, repo), "/workspace/config/prod.yml"); m != nil {
		t.Fatalf("a file whose rule was removed stayed hidden: %+v", m)
	}
	if remembered := loadHiddenFiles(repo); len(remembered) != 0 {
		t.Fatalf("a shown file is still remembered: %v", remembered)
	}
}
