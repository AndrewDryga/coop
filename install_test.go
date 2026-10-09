package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// Exercise the actual installer, including verification and extraction, without reaching the
// network, a real runtime, the user's binary directory or their shell startup files.
func TestInstallSetupOutcome(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	for _, tc := range []struct {
		name, build, doctor, calls string
		ok, signed, noDocker       bool
	}{
		{"build fails", "23", "0", "build --egress open\n", false, false, false},
		{"doctor fails", "0", "24", "build --egress open\ndoctor\n", false, false, false},
		{"ready", "0", "0", "build --egress open\ndoctor\n", true, false, false},
		{"ready with signed release and Zsh", "0", "0", "build --egress open\ndoctor\n", true, true, false},
		{"Apple container needs Docker", "0", "0", "", true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			bin := filepath.Join(root, "tools")
			if err := os.Mkdir(bin, 0o755); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"tr", "mktemp", "rm", "awk", "cut", "tar", "gzip", "dirname", "mkdir", "install", "mv", "cp"} {
				path, err := exec.LookPath(name)
				if err != nil {
					t.Skipf("%s unavailable: %v", name, err)
				}
				if err := os.Symlink(path, filepath.Join(bin, name)); err != nil {
					t.Fatal(err)
				}
			}
			sha, err := exec.LookPath("sha256sum")
			if err != nil {
				sha, err = exec.LookPath("shasum")
			}
			if err != nil {
				t.Skip("no SHA-256 utility available")
			}
			if err := os.Symlink(sha, filepath.Join(bin, filepath.Base(sha))); err != nil {
				t.Fatal(err)
			}
			write := func(path, body string, mode os.FileMode) {
				t.Helper()
				if err := os.WriteFile(path, []byte(body), mode); err != nil {
					t.Fatal(err)
				}
			}
			stub := `#!/bin/sh
case "$1" in
  version) echo fixture ;;
  build)
    printf 'build %s %s\n' "$2" "$3" >> "$TEST_CALLS"
    if test "$TEST_BUILD_EXIT" = 0; then
      printf '✓ Box image built\n  14 supervised editor sessions will reconnect with the new image.\n'
    fi
    exit "$TEST_BUILD_EXIT" ;;
  doctor)
    printf 'doctor\n' >> "$TEST_CALLS"
    printf 'Checking the Coop box on the Docker runtime\n'
    exit "$TEST_DOCTOR_EXIT" ;;
  *) exit 97 ;;
esac
`
			var archive bytes.Buffer
			gz := gzip.NewWriter(&archive)
			tw := tar.NewWriter(gz)
			if err := tw.WriteHeader(&tar.Header{Name: "coop", Mode: 0o755, Size: int64(len(stub))}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write([]byte(stub)); err != nil {
				t.Fatal(err)
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			if err := gz.Close(); err != nil {
				t.Fatal(err)
			}
			write(filepath.Join(root, "archive"), archive.String(), 0o600)
			write(filepath.Join(root, "checksums"), fmt.Sprintf("%x  coop_9.9.9_linux_amd64.tar.gz\n", sha256.Sum256(archive.Bytes())), 0o600)
			write(filepath.Join(bin, "uname"), "#!/bin/sh\ncase \"$1\" in -s) echo Linux;; -m) echo x86_64;; *) exit 97;; esac\n", 0o755)
			runtime := "docker"
			if tc.noDocker {
				runtime = "container"
			}
			write(filepath.Join(bin, runtime), "#!/bin/sh\nexit 97\n", 0o755)
			write(filepath.Join(bin, "curl"), `#!/bin/sh
test "$#" -eq 4 && test "$1" = -fsSL && test "$3" = -o || exit 97
case "$2" in
  https://github.com/AndrewDryga/coop/releases/download/v9.9.9/coop_9.9.9_linux_amd64.tar.gz) cp "$TEST_ROOT/archive" "$4" ;;
  https://github.com/AndrewDryga/coop/releases/download/v9.9.9/checksums.txt) cp "$TEST_ROOT/checksums" "$4" ;;
  https://github.com/AndrewDryga/coop/releases/download/v9.9.9/checksums.txt.bundle) printf fixture > "$4" ;;
  *) exit 97 ;;
esac
`, 0o755)
			shell := "/bin/sh"
			if tc.signed {
				write(filepath.Join(bin, "cosign"), "#!/bin/sh\ntest \"$1\" = verify-blob\n", 0o755)
				shell = "/bin/zsh"
			}
			calls := filepath.Join(root, "calls")
			installed := filepath.Join(root, "installed", "coop")
			cmd := exec.Command(sh, "./install.sh")
			cmd.Env = []string{"PATH=" + bin, "HOME=" + root, "TMPDIR=" + root, "SHELL=" + shell,
				"COOP_VERSION=v9.9.9", "COOP_NO_BUILD=0", "COOP_BIN_DIR=" + filepath.Dir(installed),
				"TEST_ROOT=" + root, "TEST_CALLS=" + calls, "TEST_BUILD_EXIT=" + tc.build, "TEST_DOCTOR_EXIT=" + tc.doctor}
			out, err := cmd.CombinedOutput()
			if (err == nil) != tc.ok {
				t.Errorf("installer success = %v, want %v: %v\n%s", err == nil, tc.ok, err, out)
			}
			if got, err := os.ReadFile(calls); err != nil && !(tc.noDocker && os.IsNotExist(err)) || string(got) != tc.calls {
				t.Errorf("setup calls = %q, want %q: %v\n%s", got, tc.calls, err, out)
			}
			footer := "\nDone. Now run in any repo:\n\n" +
				"  coop claude   # start a sandboxed agent\n\n" +
				"For more information:\n\n  coop help\n"
			if strings.Contains(string(out), "Done.") != tc.ok || (tc.ok && !strings.Contains(string(out), footer)) {
				t.Errorf("final success must match setup outcome:\n%s", out)
			}
			for _, want := range []string{
				"Installing Coop 9.9.9\n  Downloading coop_9.9.9_linux_amd64.tar.gz…\n\n",
				"  ✓ Download checksum verified\n",
				fmt.Sprintf("  ✓ Installed %s (fixture)\n", installed),
			} {
				if !strings.Contains(string(out), want) {
					t.Errorf("installer output is missing %q:\n%s", want, out)
				}
			}
			if tc.build == "0" && !tc.noDocker && !strings.Contains(string(out), "sessions will reconnect with the new image.\n\nChecking the Coop box") {
				t.Errorf("build and doctor output need a blank line between sections:\n%s", out)
			}
			if tc.noDocker && !strings.Contains(string(out), "online agents require Docker — install and start Docker") {
				t.Errorf("Apple-only setup must direct online agents to Docker:\n%s", out)
			}
			if tc.signed {
				completion := "\nZsh completion (optional)\n\n" +
					"  Coop does not edit your shell files. Run once:\n\n" +
					"    mkdir -p ~/.config/coop\n" +
					"    coop completion zsh > ~/.config/coop/completion.zsh\n\n" +
					"  Then add this to ~/.zshrc, AFTER your compinit line:\n\n" +
					"    source ~/.config/coop/completion.zsh\n\n" +
					"  Spelling correction stays on everywhere; only Coop's own arguments are exempt.\n"
				if !strings.Contains(string(out), completion) || !strings.Contains(string(out), "  ✓ Release signature verified with Cosign\n") {
					t.Errorf("signed Zsh setup output is incomplete or incorrectly spaced:\n%s", out)
				}
				t.Logf("installer output:\n%s", out)
			} else if strings.Contains(string(out), "✓ Release signature verified") || strings.Contains(string(out), "Zsh completion") {
				t.Errorf("installer claims verification or guidance that did not apply:\n%s", out)
			}
			if !tc.ok && (!strings.Contains(string(out), "setup is incomplete") || !strings.Contains(string(out), "coop doctor")) {
				t.Errorf("failed setup needs an accurate recovery action:\n%s", out)
			}
			if tc.build != "0" && !strings.Contains(string(out), "Retry: coop build --egress open && coop doctor") {
				t.Errorf("failed image build needs its retry action:\n%s", out)
			}
			if got, err := os.ReadFile(installed); err != nil || string(got) != stub {
				t.Errorf("installed binary was not preserved: %v", err)
			}
			if info, err := os.Stat(installed); err != nil || info.Mode().Perm() != 0o755 {
				t.Errorf("installed binary is not executable: %v", err)
			}
		})
	}
}

// TestInstallVerifyChecksum exercises install.sh's verify_checksum helper without network: it
// sources the script (COOP_INSTALL_LIB=1 stops it before any download) and checks that a matching
// entry passes while missing metadata/archive bytes, a mismatch, or an unavailable SHA tool all
// fail closed.
func TestInstallVerifyChecksum(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	dir := t.TempDir()
	archive := filepath.Join(dir, "coop.tar.gz")
	payload := []byte("the release archive bytes")
	if err := os.WriteFile(archive, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	good := hex.EncodeToString(sum[:])

	sums := filepath.Join(dir, "checksums.txt")
	content := good + "  coop_match.tar.gz\n" +
		"0000000000000000000000000000000000000000000000000000000000000000  coop_wrong.tar.gz\n"
	if err := os.WriteFile(sums, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	// Source install.sh for its functions only, then call verify_checksum with our fixture.
	verify := func(asset string) error {
		cmd := exec.Command("sh", "-c", `. ./install.sh; verify_checksum "$1" "$2" "$3"`,
			"sh", asset, sums, archive)
		cmd.Env = append(os.Environ(), "COOP_INSTALL_LIB=1")
		return cmd.Run()
	}

	if err := verify("coop_match.tar.gz"); err != nil {
		t.Errorf("a matching checksum entry should verify, got: %v", err)
	}
	if verify("coop_missing.tar.gz") == nil {
		t.Error("a missing checksum entry must fail closed, not install unverified")
	}
	if verify("coop_wrong.tar.gz") == nil {
		t.Error("a present-but-wrong checksum must abort")
	}
	missingSums := filepath.Join(dir, "missing-checksums.txt")
	cmd := exec.Command("sh", "-c", `. ./install.sh; verify_checksum "$1" "$2" "$3"`,
		"sh", "coop_match.tar.gz", missingSums, archive)
	cmd.Env = append(os.Environ(), "COOP_INSTALL_LIB=1")
	if cmd.Run() == nil {
		t.Error("an unreadable checksum file must fail closed")
	}
	missingArchive := filepath.Join(dir, "missing-coop.tar.gz")
	cmd = exec.Command("sh", "-c", `. ./install.sh; verify_checksum "$1" "$2" "$3"`,
		"sh", "coop_match.tar.gz", sums, missingArchive)
	cmd.Env = append(os.Environ(), "COOP_INSTALL_LIB=1")
	if cmd.Run() == nil {
		t.Error("an unreadable release archive must fail closed")
	}
}

func TestInstallVerifyChecksumRequiresTool(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	awk, err := exec.LookPath("awk")
	if err != nil {
		t.Skip("awk not available")
	}
	dir := t.TempDir()
	if err := os.Symlink(awk, filepath.Join(dir, "awk")); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(dir, "coop.tar.gz")
	if err := os.WriteFile(archive, []byte("archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	sums := filepath.Join(dir, "checksums.txt")
	if err := os.WriteFile(sums, []byte(strings.Repeat("0", 64)+"  coop.tar.gz\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(sh, "-c", `. ./install.sh; verify_checksum "$1" "$2" "$3"`,
		"sh", "coop.tar.gz", sums, archive)
	cmd.Env = []string{"COOP_INSTALL_LIB=1", "HOME=" + t.TempDir(), "PATH=" + dir}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("verification without sha256sum or shasum must fail")
	}
	if !strings.Contains(string(out), "no SHA-256 tool found") {
		t.Errorf("missing actionable SHA-tool error: %s", out)
	}
}

// TestInstallAtomicInstall exercises install.sh's atomic_install helper without network
// (COOP_INSTALL_LIB=1 stops the script before any download): it must land the file with
// mode 0755, overwrite an existing destination, and do so by rename — the destination's
// inode changes — proving a self-update can replace the *running* binary without ETXTBSY
// or corruption, and leave no .coop.new.* staging file behind.
func TestInstallAtomicInstall(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("new binary v2"), 0o755); err != nil {
		t.Fatal(err)
	}
	bindir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bindir, 0o755); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(bindir, "coop")
	// Pre-existing destination with different content, to prove overwrite + inode swap.
	if err := os.WriteFile(dest, []byte("old binary v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldIno := inodeOf(t, dest)

	cmd := exec.Command("sh", "-c", `. ./install.sh; atomic_install "$1" "$2"`, "sh", src, dest)
	cmd.Env = append(os.Environ(), "COOP_INSTALL_LIB=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("atomic_install failed: %v\n%s", err, out)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new binary v2" {
		t.Errorf("destination content = %q, want the new binary", got)
	}
	if fi, err := os.Stat(dest); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm() != 0o755 {
		t.Errorf("destination mode = %v, want 0755", fi.Mode().Perm())
	}
	if newIno := inodeOf(t, dest); newIno == oldIno {
		t.Errorf("destination inode unchanged (%d) — atomic_install must rename, not overwrite in place", newIno)
	}

	entries, err := os.ReadDir(bindir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".coop.new.") {
			t.Errorf("staging file left behind: %s", e.Name())
		}
	}
}

func inodeOf(t *testing.T, path string) uint64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("no syscall.Stat_t on this platform")
	}
	return uint64(st.Ino)
}

// TestInstallBundleRequired pins the installer's Sigstore policy: every release from v2.2.2 on
// ships checksums.txt.bundle, so with cosign present a missing bundle aborts the install instead
// of downgrading to a warning; older tags, which were never signed, still install on the checksum
// alone; a tag that is not a plain version fails closed.
func TestInstallBundleRequired(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	required := func(tag string) bool {
		cmd := exec.Command("sh", "-c", `. ./install.sh; bundle_required "$1"`, "sh", tag)
		cmd.Env = append(os.Environ(), "COOP_INSTALL_LIB=1")
		return cmd.Run() == nil
	}
	for tag, want := range map[string]bool{
		"v2.2.2": true, "v2.2.10": true, "v2.10.0": true, "v9.1.0": true, "v10.0.0": true,
		"v2.2.1": false, "v1.9.9": false, "v0.1.0": false,
		"latest": true, "": true, "v2.2.2-rc1": true,
	} {
		if got := required(tag); got != want {
			t.Errorf("bundle_required(%q) = %v, want %v", tag, got, want)
		}
	}
}

// The Zsh guidance has to WORK, not merely read well: the retired form sent the user at the first
// fpath entry, which on a clean macOS Zsh is /usr/local/share/zsh/site-functions — absent and
// unwritable — so the printed sequence failed at the redirect. This runs the printed sequence in a
// disposable HOME with `zsh -f`, and checks the one thing sourcing is for: the nocorrect alias,
// which autoloading alone would never define.
func TestInstallZshGuidanceActuallyWorks(t *testing.T) {
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh is not installed")
	}
	binary := filepath.Join(t.TempDir(), "coop")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build coop: %v\n%s", err, out)
	}
	home := t.TempDir()
	script := "set -e\n" +
		"mkdir -p ~/.config/coop\n" +
		binary + " completion zsh > ~/.config/coop/completion.zsh\n" +
		"autoload -Uz compinit; compinit -u 2>/dev/null\n" +
		"source ~/.config/coop/completion.zsh\n" +
		"alias coop\n"
	cmd := exec.Command(zsh, "-f", "-c", script)
	cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the printed Zsh instructions failed on a clean shell: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "nocorrect coop") {
		t.Errorf("sourcing the integration did not define the nocorrect alias:\n%s", out)
	}
}

// TestInstallZshGuidanceOnly pins the supported Zsh setup path: the installer TELLS a Zsh user
// where the generated integration goes — sourced after compinit, because autoloading alone never
// runs the file's nocorrect alias — and never edits a shell startup file itself.
func TestInstallZshGuidanceOnly(t *testing.T) {
	script, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	for _, want := range []string{
		"mkdir -p ~/.config/coop",
		"coop completion zsh > ~/.config/coop/completion.zsh",
		"AFTER your compinit line",
		"source ~/.config/coop/completion.zsh",
		"Coop does not edit your shell files",
		"Spelling correction stays on everywhere",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("install.sh is missing the Zsh guidance %q", want)
		}
	}
	// The retired form, which sent the user at ${fpath[1]}: on a clean macOS Zsh that is
	// /usr/local/share/zsh/site-functions — absent and unwritable — and where another tool's
	// completions come first in fpath, it is that tool's directory.
	if strings.Contains(text, "fpath[1]") {
		t.Error("install.sh still points Zsh users at ${fpath[1]}, which is not theirs to write")
	}
	// The guidance is printed, never applied: no redirect at any shell startup file.
	for _, rc := range []string{".zshrc", ".bashrc", ".bash_profile", ".profile", ".zprofile"} {
		for _, line := range strings.Split(text, "\n") {
			if strings.Contains(line, rc) && (strings.Contains(line, ">>") || strings.Contains(line, "tee")) {
				t.Errorf("install.sh writes to %s — setup guidance must stay guidance: %s", rc, line)
			}
		}
	}
}
