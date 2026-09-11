package cli

import (
	"errors"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// probeReason turns a failed probe's stderr/error into the one sentence a reason slot holds, and
// says so explicitly when neither the runtime nor the error said anything — a blank reason under
// a failure headline is the one thing a reader cannot act on.
func TestProbeReason(t *testing.T) {
	const fallback = "Nothing was reported."
	cases := []struct {
		errOut string
		err    error
		want   string
	}{
		{"", nil, fallback},
		{"boom", nil, "Boom."},
		{"line1\nlast line", nil, "Last line."}, // only the last stderr line
		{"  \n  trailing  ", nil, "Trailing."},
		{"", errors.New("run failed"), "Run failed."}, // fall back to the run error
		{"stderr wins", errors.New("ignored"), "Stderr wins."},
	}
	for _, c := range cases {
		if got := probeReason(c.errOut, c.err, fallback); got != c.want {
			t.Errorf("probeReason(%q, %v) = %q, want %q", c.errOut, c.err, got, c.want)
		}
	}
}

// The probe speaks stable check ids; the report owns the words. A verdict the host cannot map to
// a known id must not become a silent pass, so parsing keeps ids and measured values apart.
func TestParseProbeResults(t *testing.T) {
	got := parseProbeResults("RESULT PASS sandbox.env\nRESULT FAIL host.coop_cli\nRESULT UID 1000\nnoise\nRESULT PIDS max\n")
	for id, want := range map[string]string{
		"sandbox.env":   "PASS",
		"host.coop_cli": "FAIL",
		"UID":           "1000",
		"PIDS":          "max",
	} {
		if got[id] != want {
			t.Errorf("parseProbeResults()[%q] = %q, want %q", id, got[id], want)
		}
	}
	if _, ok := got["noise"]; ok {
		t.Error("a non-RESULT line became a verdict")
	}
}

// The verdict counts what actually happened: a probe that died is one failure plus the checks it
// was carrying, never a smaller total that happens to read as clean.
func TestDoctorVerdictCountsUnrunChecks(t *testing.T) {
	report := &doctorReport{}
	s := report.section(sectionSecrets)
	s.pass("one")
	s.pass("two")
	tasks := report.section(sectionTasks)
	tasks.probeFailed("Could not run the task-channel checks", "It did not respond.", 4)
	skipped := report.section(sectionCredentials)
	skipped.skip("Settings permissions not checked", "", 1)
	got := report.tally()
	want := doctorTally{passed: 2, probeFailed: 1, uncompleted: 4, notChecked: 1}
	if got != want {
		t.Errorf("tally = %+v, want %+v", got, want)
	}
	out := captureStderr(t, func() {
		if code := report.print(); code != 1 {
			t.Errorf("a failed probe must exit 1, got %d", code)
		}
	})
	if !strings.Contains(out, "✗ 2 checks passed; 1 probe failed; 4 checks could not be completed; 1 could not be checked") {
		t.Errorf("verdict did not account for every check:\n%s", out)
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

	report := &doctorReport{}
	s := report.section(sectionCredentials)
	doctorCheckHome(s, "blocked", false)
	if got := report.tally(); got.passed != 0 || got.failed != 0 || got.notChecked != 1 {
		t.Fatalf("Alpine fallback must skip home ownership rather than judge it, got %+v", got)
	}
	doctorCheckHome(s, "writable", true)
	doctorCheckHome(s, "blocked", true)
	if got := report.tally(); got.passed != 1 || got.failed != 1 {
		t.Fatalf("real-image home results = %+v, want one pass and one fail", got)
	}
}

// doctor probes the image the repo's boxes actually run: the per-project image when its
// .agent/Dockerfile is built, else the shared base image, else the alpine stand-in (not "real").
func TestDoctorImagePrefersTheRepoImageThenBaseThenAlpine(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "Dockerfile"), []byte("FROM coop-box\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{BaseImage: "coop-box"}
	perProject := box.ImageForRepo(repo, cfg.BaseImage, "")
	if perProject == cfg.BaseImage {
		t.Fatalf("fixture repo did not resolve to a per-project image: %q", perProject)
	}
	built := map[string]bool{perProject: true, "coop-box": true}
	exists := func(image string) bool { return built[image] }
	if img, real := doctorImage(repo, cfg, exists); img != perProject || !real {
		t.Fatalf("with the per-project image built: (%q, %v), want (%q, true)", img, real, perProject)
	}
	delete(built, perProject)
	if img, real := doctorImage(repo, cfg, exists); img != "coop-box" || !real {
		t.Fatalf("with only the base image built: (%q, %v), want (coop-box, true)", img, real)
	}
	delete(built, "coop-box")
	if img, real := doctorImage(repo, cfg, exists); img != "alpine" || real {
		t.Fatalf("with nothing built: (%q, %v), want (alpine, false)", img, real)
	}
	plain := t.TempDir() // no .agent/Dockerfile: the base image is the repo's image
	built["coop-box"] = true
	if img, real := doctorImage(plain, cfg, exists); img != "coop-box" || !real {
		t.Fatalf("plain repo: (%q, %v), want (coop-box, true)", img, real)
	}
}
