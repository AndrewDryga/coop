package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

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
		`coop completion zsh > \"\${fpath[1]}/_coop\"`,
		"AFTER your compinit line",
		`source \"\${fpath[1]}/_coop\"`,
		"coop does not edit your shell files",
		"Spelling correction stays on everywhere",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("install.sh is missing the Zsh guidance %q", want)
		}
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
