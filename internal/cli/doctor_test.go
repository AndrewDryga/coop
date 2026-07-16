package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// probeWhy turns a failed probe's stderr/error into a short " : <last line>" suffix.
func TestProbeWhy(t *testing.T) {
	cases := []struct {
		errOut string
		err    error
		want   string
	}{
		{"", nil, ""},
		{"boom", nil, ": boom"},
		{"line1\nlast line", nil, ": last line"}, // only the last stderr line
		{"  \n  trailing  ", nil, ": trailing"},
		{"", errors.New("run failed"), ": run failed"}, // fall back to the run error
		{"stderr wins", errors.New("ignored"), ": stderr wins"},
	}
	for _, c := range cases {
		if got := probeWhy(c.errOut, c.err); got != c.want {
			t.Errorf("probeWhy(%q, %v) = %q, want %q", c.errOut, c.err, got, c.want)
		}
	}
}

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
	// The expanded fixture seeds a direnv file, a private key one level down, and a symlink to a
	// secret — the cases the probe needs to prove shadowing covers (.envrc/key by name, symlink
	// by following it). A missing one would make those probe checks silently fail.
	for _, rel := range []string{".envrc", filepath.Join("deploy", "id_ed25519")} {
		if fi, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("fixture missing %s: %v", rel, err)
		} else if fi.Mode().Perm()&0o004 == 0 {
			t.Errorf("%s mode = %o, want world-readable", rel, fi.Mode().Perm())
		}
	}
	if target, err := os.Readlink(filepath.Join(dir, "notes-link")); err != nil {
		t.Errorf("fixture symlink notes-link missing: %v", err)
	} else if target != ".env" {
		t.Errorf("notes-link -> %q, want .env (a symlink to a shadowed secret)", target)
	}
}

func TestDoctorCredAndHomeProbeReportsConfigWritability(t *testing.T) {
	probe := func(home string) string {
		t.Helper()
		cmd := exec.Command("sh")
		cmd.Stdin = strings.NewReader(doctorCredAndHomeProbe(home))
		cmd.Env = append(os.Environ(), "ANTHROPIC_API_KEY=fake", "OPENAI_API_KEY=", "GOOGLE_API_KEY=")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("probe failed: %v\n%s", err, out)
		}
		return string(out)
	}

	writable := t.TempDir()
	if got := probe(writable); !strings.Contains(got, "RESULT HOME writable") {
		t.Fatalf("writable home result missing:\n%s", got)
	}
	blocked := t.TempDir()
	if err := os.WriteFile(filepath.Join(blocked, ".config"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := probe(blocked); !strings.Contains(got, "RESULT HOME blocked") {
		t.Fatalf("blocked home result missing:\n%s", got)
	}

	var rep report
	doctorCheckHome(&rep, "blocked", false)
	if rep.pass != 0 || rep.fail != 0 {
		t.Fatalf("Alpine fallback must skip home ownership, got %+v", rep)
	}
	doctorCheckHome(&rep, "writable", true)
	doctorCheckHome(&rep, "blocked", true)
	if rep.pass != 1 || rep.fail != 1 {
		t.Fatalf("real-image home results = %+v, want one pass and one fail", rep)
	}
}
