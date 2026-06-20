package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The doctor mounts its fixture into a box that runs the probe as a uid which may not own it;
// under --cap-drop ALL (no CAP_DAC_OVERRIDE) the box reaches it only if it's world-readable. Guard
// that buildFixture leaves the tree world-traversable/readable — MkdirTemp defaults to 0700, which
// silently produced an unreadable /workspace on rootful Linux Docker (the box read nothing).
func TestBuildFixtureWorldReadable(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir, err := buildFixture()
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	// The root must grant "other" r-x, so a non-owner box can cd /workspace and stat the fixtures.
	if fi, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm()&0o005 != 0o005 {
		t.Errorf("fixture root mode = %o, want world rx so a non-owner box can enter it", fi.Mode().Perm())
	}
	// A seeded source file must be world-readable (the probe reads it as that same non-owner uid).
	if fi, err := os.Stat(filepath.Join(dir, "src", "app.js")); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm()&0o004 == 0 {
		t.Errorf("src/app.js mode = %o, want world-readable", fi.Mode().Perm())
	}
}
