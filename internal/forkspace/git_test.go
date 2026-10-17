package forkspace

import (
	"crypto/rand"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// gitIn runs git in dir inheriting the PROCESS environment, so a t.Setenv'd GIT_CONFIG_GLOBAL /
// GIT_CONFIG_SYSTEM reaches the setup commands AND the helpers under test, which shell out to
// `git config --global` themselves. internal/testutil/gitrepo pins its own isolated config on the
// child instead, which would send these tests' `--global` writes to a file the helpers never read.
func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// initRepo makes an empty repo to hold the local config these tests plant. Nothing here commits.
func initRepo(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "myrepo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "init", "-q")
	return repo
}

// WantsSigning must read your signing preference from the GLOBAL config only: a poisoned
// agent-writable repo that forces commit.gpgsign=true would otherwise get its planted gpg.program
// run on the host.
func TestWantsSigning(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "global"))
	repo := initRepo(t)

	if WantsSigning() {
		t.Error("WantsSigning = true with commit.gpgsign unset, want false")
	}
	gitIn(t, repo, "config", "commit.gpgsign", "true") // repo-local: must NOT turn signing on
	t.Chdir(repo)
	if WantsSigning() {
		t.Error("WantsSigning = true from a repo-local commit.gpgsign, want false (global only)")
	}
	gitIn(t, repo, "config", "--global", "commit.gpgsign", "true")
	if !WantsSigning() {
		t.Error("WantsSigning = false with a global commit.gpgsign=true, want true")
	}
}

// TrustedSignArgs must read signing config from your GLOBAL git config — so neither a fork nor the
// agent-writable parent repo can point gpg.program at a planted binary — and track gpg.format to
// the matching program key. (`git config --global` ignores -C, writing the GIT_CONFIG_GLOBAL file.)
func TestTrustedSignArgs(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	t.Run("openpgp default ignores a repo-local poison", func(t *testing.T) {
		t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "global"))
		repo := initRepo(t)
		gitIn(t, repo, "config", "--global", "commit.gpgsign", "true")
		gitIn(t, repo, "config", "--global", "user.signingkey", "ABCD1234")
		gitIn(t, repo, "config", "gpg.program", "/tmp/evil") // repo-local: must be ignored
		t.Chdir(repo)
		want := []string{
			"-c", "commit.gpgsign=true",
			"-c", "user.signingkey=",
			"-c", "gpg.ssh.defaultKeyCommand=",
			"-c", "gpg.program=gpg",
			"-c", "user.signingkey=ABCD1234",
		}
		if got := TrustedSignArgs(); !slices.Equal(got, want) {
			t.Errorf("TrustedSignArgs = %v, want %v (gpg.program must come from global, not the repo)", got, want)
		}
	})
	t.Run("ssh format picks gpg.ssh.program", func(t *testing.T) {
		t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "global"))
		repo := initRepo(t)
		gitIn(t, repo, "config", "--global", "commit.gpgsign", "true")
		gitIn(t, repo, "config", "--global", "gpg.format", "ssh")
		gitIn(t, repo, "config", "--global", "user.signingkey", "/k.pub")
		gitIn(t, repo, "config", "gpg.ssh.program", "/tmp/evil")
		t.Chdir(repo)
		want := []string{
			"-c", "commit.gpgsign=true",
			"-c", "user.signingkey=",
			"-c", "gpg.ssh.defaultKeyCommand=",
			"-c", "gpg.format=ssh",
			"-c", "gpg.ssh.program=ssh-keygen",
			"-c", "user.signingkey=/k.pub",
		}
		if got := TrustedSignArgs(); !slices.Equal(got, want) {
			t.Errorf("TrustedSignArgs = %v, want %v", got, want)
		}
	})
	t.Run("ssh default key command comes only from global config", func(t *testing.T) {
		t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "global"))
		repo := initRepo(t)
		gitIn(t, repo, "config", "--global", "gpg.format", "ssh")
		gitIn(t, repo, "config", "--global", "gpg.ssh.defaultKeyCommand", "trusted-key-command")
		gitIn(t, repo, "config", "gpg.ssh.defaultKeyCommand", "untrusted-local-command")
		t.Chdir(repo)
		want := []string{
			"-c", "commit.gpgsign=true",
			"-c", "user.signingkey=",
			"-c", "gpg.ssh.defaultKeyCommand=",
			"-c", "gpg.format=ssh",
			"-c", "gpg.ssh.program=ssh-keygen",
			"-c", "gpg.ssh.defaultKeyCommand=trusted-key-command",
		}
		if got := TrustedSignArgs(); !slices.Equal(got, want) {
			t.Errorf("TrustedSignArgs = %v, want %v", got, want)
		}
	})
}

// pinnedHistoryCopy runs GitCopyPinnedHistoryContext into a new directory, as a companion's
// staging directory is new.
func pinnedHistoryCopy(t *testing.T, repo, commit string) (string, error) {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "copy")
	if err := os.Mkdir(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	return dst, GitCopyPinnedHistoryContext(t.Context(), repo, dst, commit)
}

func commitFile(t *testing.T, repo, name string, content []byte) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), content, 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", name)
	gitIn(t, repo, "commit", "-qm", name)
	return strings.TrimSpace(string(readOut(t, repo, "rev-parse", "HEAD")))
}

// A companion copies its source's history in the form the worker stored it. A non-local clone
// searched objects for deltas again across the source's packs and re-hashed all of them: one
// 6.2 GB history in two packs took five of the six minutes a tenant's Work session waited
// before its agent started (2026-10-17). Two near-identical blobs the source stores whole, in
// two packs as a later fetch leaves them, stay whole; a fresh delta search stores one as a
// delta of the other.
func TestPinnedHistoryCopyKeepsTheStoredFormWithoutADeltaSearch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := committedSetupRepo(t)
	asset := make([]byte, 1<<20)
	if _, err := rand.Read(asset); err != nil {
		t.Fatal(err)
	}
	commitFile(t, repo, "asset.bin", asset)
	gitIn(t, repo, "repack", "-a", "-d", "-q", "--window=0")
	asset[0]++
	commit := commitFile(t, repo, "asset.bin", asset)
	gitIn(t, repo, "repack", "-d", "-q", "--window=0")

	dst, err := pinnedHistoryCopy(t, repo, commit)
	if err != nil {
		t.Fatal(err)
	}
	bases := readOut(t, dst, "cat-file", "--batch-all-objects", "--batch-check=%(deltabase)")
	for _, base := range strings.Fields(string(bases)) {
		if strings.Trim(base, "0") != "" {
			t.Fatalf("the copy searched for deltas again: an object is now a delta of %s", base)
		}
	}
}

// The copy holds exactly the pinned history, an unreferenced pin included. Another branch, an
// unreachable object and the source's configuration stay behind; no ref, remote, alternate or
// hardlink ties the copy back, and the source gains no file, not even a temporary one.
func TestPinnedHistoryCopyOwnsOnlyThePinnedHistory(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := committedSetupRepo(t)
	pinned := commitFile(t, repo, "README.md", []byte("selected\n"))
	gitIn(t, repo, "reset", "--hard", "-q", "HEAD^")
	gitIn(t, repo, "checkout", "-q", "-b", "other")
	other := commitFile(t, repo, "other.txt", []byte("other\n"))
	gitIn(t, repo, "checkout", "-q", "main")
	gitIn(t, repo, "repack", "-a", "-d", "-q")
	stray := strings.TrimSpace(string(readOut(t, repo, "hash-object", "-w", "--stdin")))
	marker := filepath.Join(t.TempDir(), "source-config-ran")
	gitIn(t, repo, "config", "uploadpack.packObjectsHook", "touch "+marker)
	gitIn(t, repo, "config", "core.fsmonitor", "touch "+marker)
	before := objectFiles(t, repo)

	dst, err := pinnedHistoryCopy(t, repo, pinned)
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.TrimSpace(string(readOut(t, dst, "rev-list", "--count", pinned))); count != "2" {
		t.Fatalf("pinned history has %s commits, want 2", count)
	}
	for _, missing := range []string{other, stray} {
		if exec.Command("git", "-C", dst, "cat-file", "-e", missing).Run() == nil {
			t.Fatalf("the copy holds %s, which is not in the pinned history", missing)
		}
	}
	if refs := readOut(t, dst, "for-each-ref"); len(refs) != 0 {
		t.Fatalf("the copy has refs: %s", refs)
	}
	if remotes := readOut(t, dst, "remote"); len(remotes) != 0 {
		t.Fatalf("the copy has a remote: %s", remotes)
	}
	if _, err := os.Lstat(filepath.Join(dst, ".git", "objects", "info", "alternates")); !os.IsNotExist(err) {
		t.Fatalf("the copy has a persistent object alternate: %v", err)
	}
	for name := range objectFiles(t, dst) {
		info, err := os.Lstat(filepath.Join(dst, ".git", "objects", name))
		if err != nil {
			t.Fatal(err)
		}
		if links := info.Sys().(*syscall.Stat_t).Nlink; links != 1 {
			t.Fatalf("%s has %d links: the copy shares a file with the source", name, links)
		}
	}
	if after := objectFiles(t, repo); !maps.Equal(before, after) {
		t.Fatalf("the source's objects changed: %v, then %v", before, after)
	}
	if _, err := os.Lstat(marker); !os.IsNotExist(err) {
		t.Fatalf("the source's configuration ran: %v", err)
	}
}

// A shallow source's history ends at its boundary: the copy is shallow there too, and still has
// no ref or remote.
func TestPinnedHistoryCopyOfAShallowSourceKeepsItsBoundary(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := committedSetupRepo(t)
	commitFile(t, repo, "README.md", []byte("second\n"))
	shallow := filepath.Join(t.TempDir(), "shallow")
	gitIn(t, filepath.Dir(shallow), "clone", "-q", "--depth=1", "file://"+repo, shallow)
	commit := strings.TrimSpace(string(readOut(t, shallow, "rev-parse", "HEAD")))

	dst, err := pinnedHistoryCopy(t, shallow, commit)
	if err != nil {
		t.Fatal(err)
	}
	if state := strings.TrimSpace(string(readOut(t, dst, "rev-parse", "--is-shallow-repository"))); state != "true" {
		t.Fatalf("shallow source copied as shallow = %s", state)
	}
	if count := strings.TrimSpace(string(readOut(t, dst, "rev-list", "--count", commit))); count != "1" {
		t.Fatalf("shallow history has %s commits, want 1", count)
	}
	if refs, remotes := readOut(t, dst, "for-each-ref"), readOut(t, dst, "remote"); len(refs)+len(remotes) != 0 {
		t.Fatalf("the copy kept the clone's refs %q or remote %q", refs, remotes)
	}
}

// A source whose path holds the path separator cannot be named in an alternate list, which
// Git splits there: it is cloned instead, with the same result.
func TestPinnedHistoryCopyOfASourceWhosePathHoldsThePathSeparator(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := committedSetupRepo(t)
	moved := filepath.Join(t.TempDir(), "with"+string(filepath.ListSeparator)+"separator")
	if err := os.Rename(repo, moved); err != nil {
		t.Fatal(err)
	}
	commit := strings.TrimSpace(string(readOut(t, moved, "rev-parse", "HEAD")))

	dst, err := pinnedHistoryCopy(t, moved, commit)
	if err != nil {
		t.Fatal(err)
	}
	if head := strings.TrimSpace(string(readOut(t, dst, "rev-parse", commit+"^{commit}"))); head != commit {
		t.Fatalf("copied commit = %s, want %s", head, commit)
	}
	if refs, remotes := readOut(t, dst, "for-each-ref"), readOut(t, dst, "remote"); len(refs)+len(remotes) != 0 {
		t.Fatalf("the copy kept the clone's refs %q or remote %q", refs, remotes)
	}
}

// A history missing an object is refused, never copied in part.
func TestPinnedHistoryCopyRefusesAnIncompleteHistory(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := committedSetupRepo(t)
	commit := strings.TrimSpace(string(readOut(t, repo, "rev-parse", "HEAD")))
	blob := strings.TrimSpace(string(readOut(t, repo, "rev-parse", "HEAD:README.md")))
	if err := os.Remove(filepath.Join(repo, ".git", "objects", blob[:2], blob[2:])); err != nil {
		t.Fatal(err)
	}
	if _, err := pinnedHistoryCopy(t, repo, commit); err == nil {
		t.Fatal("copied a history with a missing object")
	}
}

// objectFiles names every file under a repository's object directory, relative to it.
func objectFiles(t *testing.T, repo string) map[string]bool {
	t.Helper()
	root := filepath.Join(repo, ".git", "objects")
	files := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		name, err := filepath.Rel(root, path)
		files[name] = true
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
