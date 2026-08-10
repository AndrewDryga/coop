package forkspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
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

func TestDriverNeutralizer(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ws := initRepo(t)
	// A legit clone has no local filter/merge/diff config → nothing to neutralize.
	if got := DriverNeutralizer(ws); len(got) != 0 {
		t.Errorf("clean repo should yield no neutralizer flags, got %v", got)
	}
	// Plant all three driver kinds locally (what an agent would do alongside an in-tree .gitattributes).
	gitIn(t, ws, "config", "filter.x.smudge", "/evil")
	gitIn(t, ws, "config", "filter.x.clean", "/evil")
	gitIn(t, ws, "config", "merge.y.driver", "/evil %O %A %B")
	gitIn(t, ws, "config", "diff.z.command", "/evil")
	got := DriverNeutralizer(ws)
	if len(got)%2 != 0 {
		t.Fatalf("neutralizer must be -c/value pairs, got odd count: %v", got)
	}
	joined := strings.Join(got, " ")
	for _, want := range []string{
		"filter.x.smudge=", "filter.x.clean=", "filter.x.process=", "filter.x.required=false",
		"merge.y.driver=", "diff.z.command=", "diff.z.textconv=",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("neutralizer missing blank for %q:\n%v", want, got)
		}
	}
}
