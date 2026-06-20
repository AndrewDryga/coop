package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestInstallVerifyChecksum exercises install.sh's verify_checksum helper without network: it
// sources the script (COOP_INSTALL_LIB=1 stops it before any download) and checks the three
// outcomes — a matching entry passes, a missing entry fails closed (the bug this guards), and
// a present-but-wrong entry still aborts.
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

	// The mismatch path only aborts when a sha256 tool is present to compute the real hash;
	// without one verify_checksum returns 0 with a warning (an entry exists, just uncheckable).
	if hasSHATool() {
		if verify("coop_wrong.tar.gz") == nil {
			t.Error("a present-but-wrong checksum must abort")
		}
	}
}

func hasSHATool() bool {
	for _, t := range []string{"sha256sum", "shasum"} {
		if _, err := exec.LookPath(t); err == nil {
			return true
		}
	}
	return false
}
