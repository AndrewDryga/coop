package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
		{"signed Zsh with absent shell files", "0", "0", "build --egress open\ndoctor\n", true, true, false},
		{"Apple container needs Docker", "0", "0", "", true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			rcFiles := map[string]string{}
			for _, rc := range []string{".zshrc", ".bashrc", ".bash_profile", ".profile", ".zprofile"} {
				if tc.signed && tc.name != "signed Zsh with absent shell files" {
					body := "# existing user file " + rc + "\nexport USER_SETTING=kept\n"
					rcFiles[rc] = body
					if err := os.WriteFile(filepath.Join(root, rc), []byte(body), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			bin := installerFixture(t, root)
			runtime := "docker"
			if tc.noDocker {
				runtime = "container"
			}
			writeFixture(t, filepath.Join(bin, runtime), "#!/bin/sh\nexit 97\n", 0o755)
			shell := "/bin/sh"
			if tc.signed {
				writeFixture(t, filepath.Join(bin, "cosign"), "#!/bin/sh\ntest \"$1\" = verify-blob\n", 0o755)
				shell = "/bin/zsh"
			}
			calls := filepath.Join(root, "calls")
			installed := filepath.Join(root, "installed", "coop")
			cmd := exec.Command(sh, "./install.sh")
			cmd.Env = []string{"PATH=" + bin, "HOME=" + root, "TMPDIR=" + root, "SHELL=" + shell,
				"COOP_VERSION=v9.9.9", "COOP_NO_BUILD=0", "COOP_BIN_DIR=" + filepath.Dir(installed),
				"TEST_ROOT=" + root, "TEST_CALLS=" + calls, "TEST_BUILD_EXIT=" + tc.build, "TEST_DOCTOR_EXIT=" + tc.doctor}
			out, err := cmd.CombinedOutput()
			for _, rc := range []string{".zshrc", ".bashrc", ".bash_profile", ".profile", ".zprofile"} {
				got, readErr := os.ReadFile(filepath.Join(root, rc))
				if want, present := rcFiles[rc]; present {
					if readErr != nil || string(got) != want {
						t.Errorf("installer changed %s: %q %v", rc, got, readErr)
					}
				} else if !os.IsNotExist(readErr) {
					t.Errorf("installer created missing %s: %q %v", rc, got, readErr)
				}
			}
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
			if tc.noDocker && !strings.Contains(string(out), "Online agents require Docker. Install and start Docker") {
				t.Errorf("Apple-only setup must direct online agents to Docker:\n%s", out)
			}
			if tc.signed {
				completion := "\nZsh completion (optional)\n\n" +
					"  Coop does not edit your shell files. Run once:\n\n" +
					"    mkdir -p ~/.config/coop\n" +
					"    coop completion zsh > ~/.config/coop/completion.zsh\n\n" +
					"  Then add this to ~/.zshrc, after your compinit line:\n\n" +
					"    source ~/.config/coop/completion.zsh\n\n" +
					"  Spelling correction stays on everywhere; only Coop's own arguments are exempt.\n"
				if !strings.Contains(string(out), completion) || !strings.Contains(string(out), "  ✓ Release signature verified with Cosign\n") {
					t.Errorf("signed Zsh setup output is incomplete or incorrectly spaced:\n%s", out)
				}
				if strings.Contains(string(out), "fpath[1]") {
					t.Error("installer directs Zsh to an unowned completion directory")
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
			if got, err := os.ReadFile(installed); err != nil || string(got) != installStub {
				t.Errorf("installed binary was not preserved: %v", err)
			}
			if info, err := os.Stat(installed); err != nil || info.Mode().Perm() != 0o755 {
				t.Errorf("installed binary is not executable: %v", err)
			}
		})
	}
}

// A pinned version installs with or without the tag's v, since a release binary prints its version
// without one. A version with no release says so in one plain line, without curl's error, and
// installs nothing. Any other download failure keeps curl's error and the URL that failed.
func TestInstallPinnedVersion(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	const missing = "No Coop release %s. Release tags look like v12.1.0.\n"
	for _, tc := range []struct{ version, refusal string }{
		{"9.9.9", ""},
		{"v9.9.9", ""},
		{"v0.0.0-qa", fmt.Sprintf(missing, "v0.0.0-qa")},
		{"9.9.8", fmt.Sprintf(missing, "v9.9.8")},
		{"v5.0.0", "curl: (22) The requested URL returned error: 500\n" +
			"Download failed: https://github.com/AndrewDryga/coop/releases/download/v5.0.0/coop_5.0.0_linux_amd64.tar.gz\n"},
	} {
		t.Run(tc.version, func(t *testing.T) {
			root := t.TempDir()
			bin := installerFixture(t, root)
			bindir := filepath.Join(root, "installed")
			cmd := exec.Command(sh, "./install.sh")
			cmd.Env = []string{"PATH=" + bin, "HOME=" + root, "TMPDIR=" + root, "SHELL=/bin/sh",
				"COOP_VERSION=" + tc.version, "COOP_NO_BUILD=1", "COOP_BIN_DIR=" + bindir, "TEST_ROOT=" + root}
			var stderr strings.Builder
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			if tc.refusal == "" {
				got, readErr := os.ReadFile(filepath.Join(bindir, "coop"))
				if err != nil || readErr != nil || string(got) != installStub || !strings.HasPrefix(string(out), "Installing Coop 9.9.9\n") {
					t.Errorf("COOP_VERSION=%s: %v %v\n%s%s", tc.version, err, readErr, out, stderr.String())
				}
				return
			}
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 || stderr.String() != tc.refusal {
				t.Errorf("COOP_VERSION=%s = %v with errors %q, want exit 1 with %q", tc.version, err, stderr.String(), tc.refusal)
			}
			if entries, err := os.ReadDir(bindir); !os.IsNotExist(err) || len(entries) > 0 {
				t.Errorf("a failed download installed %v into COOP_BIN_DIR (%v)", entries, err)
			}
		})
	}
}

// installStub is the coop binary in the fixture release: it prints its version, and records the
// build and doctor calls with the exit codes the test chose.
const installStub = `#!/bin/sh
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

// installerFixture puts under root the one release install.sh can download, v9.9.9 for Linux on
// x86-64, and returns a PATH folder with the tools the installer runs. uname and curl are fakes, so
// nothing reaches the network. Any other release download fails as GitHub's does: v5.0.0 with a 500,
// and every other version with a 404. The run needs TEST_ROOT=root in its environment.
func installerFixture(t *testing.T, root string) string {
	t.Helper()
	bin := filepath.Join(root, "tools")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tr", "mktemp", "rm", "awk", "cut", "tar", "gzip", "dirname", "mkdir", "install", "mv", "cp", "cat"} {
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
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "coop", Mode: 0o755, Size: int64(len(installStub))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(installStub)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(root, "archive"), archive.String(), 0o600)
	writeFixture(t, filepath.Join(root, "checksums"), fmt.Sprintf("%x  coop_9.9.9_linux_amd64.tar.gz\n", sha256.Sum256(archive.Bytes())), 0o600)
	writeFixture(t, filepath.Join(bin, "uname"), "#!/bin/sh\ncase \"$1\" in -s) echo Linux;; -m) echo x86_64;; *) exit 97;; esac\n", 0o755)
	// It takes only the forms install.sh uses: -fsSL, an optional -w '%{http_code}', -o <file> and
	// one URL. Like curl, a failed request writes no file and still prints its status.
	writeFixture(t, filepath.Join(bin, "curl"), `#!/bin/sh
out= url= print_code=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -fsSL) ;;
    -w) test "$2" = '%{http_code}' || exit 97; print_code=1; shift ;;
    -o) out=$2; shift ;;
    https://*) test -z "$url" || exit 97; url=$1 ;;
    *) exit 97 ;;
  esac
  shift
done
test -n "$out" && test -n "$url" || exit 97
release=https://github.com/AndrewDryga/coop/releases/download
case "$url" in
  "$release"/v9.9.9/coop_9.9.9_linux_amd64.tar.gz) cp "$TEST_ROOT/archive" "$out"; code=200 ;;
  "$release"/v9.9.9/checksums.txt) cp "$TEST_ROOT/checksums" "$out"; code=200 ;;
  "$release"/v9.9.9/checksums.txt.bundle) printf fixture > "$out"; code=200 ;;
  "$release"/v5.0.0/*) code=500 ;;
  "$release"/*) code=404 ;;
  *) exit 97 ;;
esac
test -z "$print_code" || printf %s "$code"
test "$code" = 200 && exit 0
echo "curl: (22) The requested URL returned error: $code" >&2
exit 22
`, 0o755)
	return bin
}

func writeFixture(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
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
	if _, err := exec.LookPath("sha256sum"); err != nil {
		if _, err := exec.LookPath("shasum"); err != nil {
			t.Skip("no SHA-256 utility (sha256sum or shasum) available")
		}
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
	if !strings.Contains(string(out), "No SHA-256 tool found") {
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

// The Zsh guidance has to WORK, not merely read well: the first fpath entry on a clean macOS Zsh
// is /usr/local/share/zsh/site-functions, absent and unwritable, so a sequence aimed there fails
// at the redirect. This runs the printed sequence in a
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
