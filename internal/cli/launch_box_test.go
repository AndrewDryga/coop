package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/ui"
)

const boxCheckImage = "coop-box"

// boxCheckApp is a launch-bound app over a runtime shim that records every invocation and exits
// with buildExit on `build`; name is the shim's basename, which is how the runtime kind is told.
func boxCheckApp(t *testing.T, name string, buildExit int) (*app, string) {
	t.Helper()
	recorder := filepath.Join(t.TempDir(), "runtime-args")
	shim := filepath.Join(t.TempDir(), name)
	script := "#!/bin/sh\necho \"$@\" >> " + strconv.Quote(recorder) + "\n" +
		"case \"$1\" in build) exit " + strconv.Itoa(buildExit) + " ;; info) exit 1 ;; esac\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ConfigDir: t.TempDir(), BoxHome: t.TempDir(), BaseImage: boxCheckImage}
	return &app{cfg: cfg, rt: runtime.Runtime{Name: shim}, rtSet: true, argv: []string{"codex"}}, recorder
}

func stampBoxDefinition(t *testing.T, cfg *config.Config, contents string) {
	t.Helper()
	path := filepath.Join(cfg.BoxHome, "image-meta", boxCheckImage)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func builds(t *testing.T, recorder string) int {
	t.Helper()
	data, _ := os.ReadFile(recorder)
	return strings.Count(string(data), "build ")
}

// Acceptance: a current image prints no section, and an age-only nudge is not a mismatch — it
// must not force an unpinned update. Neither an unstamped (foreign) image nor an old one is
// touched.
func TestCheckCoopBoxIsSilentWhenTheImageIsCurrentOrMerelyOld(t *testing.T) {
	a, recorder := boxCheckApp(t, "rt", 0)
	repo := t.TempDir()
	if got := captureStderr(t, func() {
		if err := a.checkCoopBox(repo, boxCheckImage); err != nil {
			t.Errorf("unstamped image: %v", err)
		}
	}); got != "" {
		t.Fatalf("an unstamped image printed %q", got)
	}
	box.StampImageMeta(a.cfg, boxCheckImage, "v9.0.0")
	old := time.Now().Add(-40 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(a.cfg.BoxHome, "image-meta", boxCheckImage), old, old); err != nil {
		t.Fatal(err)
	}
	if got := captureStderr(t, func() {
		if err := a.checkCoopBox(repo, boxCheckImage); err != nil {
			t.Errorf("old but current image: %v", err)
		}
	}); got != "" {
		t.Fatalf("an age-only nudge rendered a section: %q", got)
	}
	if n := builds(t, recorder); n != 0 {
		t.Fatalf("%d builds ran for an image that needed none", n)
	}
}

// Acceptance: a Coop-managed image whose recorded definition differs from this binary's is
// rebuilt through the ordinary build path, inside one `Checking the Coop box` section that names
// the prior and current Coop versions and ends with the green result. Afterwards the image is
// current, so the next launch prints nothing.
func TestCheckCoopBoxRebuildsAMismatchedManagedImage(t *testing.T) {
	a, recorder := boxCheckApp(t, "rt", 0)
	repo := t.TempDir()
	stampBoxDefinition(t, a.cfg, "coop v9.0.0-144-g091fdf9\ndef stale\n")
	var err error
	got := captureStderr(t, func() { err = a.checkCoopBox(repo, boxCheckImage) })
	if err != nil {
		t.Fatalf("checkCoopBox: %v", err)
	}
	for _, want := range []string{
		"\nChecking the Coop box\n",
		"  Current image was built by Coop v9.0.0-144-g091fdf9\n",
		"  Updating it for Coop " + resolveVersion() + "…\n",
		"  ✓ Box updated\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("section is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "run 'coop build'") {
		t.Fatalf("a known repair must not nag for a manual build:\n%s", got)
	}
	if n := builds(t, recorder); n != 1 {
		t.Fatalf("%d builds ran, want exactly one", n)
	}
	if _, skewed := box.BaseImageSkew(a.cfg, boxCheckImage); skewed {
		t.Fatal("the rebuilt image is still recorded as mismatched")
	}
	if got := captureStderr(t, func() { err = a.checkCoopBox(repo, boxCheckImage) }); err != nil || got != "" {
		t.Fatalf("a repaired image printed %q (%v)", got, err)
	}
}

// Acceptance: a failed section ends with the indented red headline, a blank line, the concrete
// reason six spaces further in, and the remedy one level under the section — which repeats the
// original command, never a manual build. The error comes back marked reported so the dispatcher
// does not print it a second time.
func TestCheckCoopBoxFailureIsNestedAndReportedOnce(t *testing.T) {
	// Docker is not answering: the shim is called docker, and its `info` fails.
	a, recorder := boxCheckApp(t, "docker", 0)
	stampBoxDefinition(t, a.cfg, "coop v9.0.0-144-g091fdf9\ndef stale\n")
	var err error
	got := captureStderr(t, func() { err = a.checkCoopBox(t.TempDir(), boxCheckImage) })
	if !errors.Is(err, ui.ErrReported) {
		t.Fatalf("a rendered failure must come back reported, got %v", err)
	}
	want := "  ✗ Could not update the box\n" +
		"\n" +
		"        Docker is unavailable.\n" +
		"\n" +
		"    Start Docker, then run 'coop codex' again.\n"
	if !strings.HasSuffix(got, want) {
		t.Fatalf("Docker-unavailable section ended:\n%q\nwant:\n%q", got, want)
	}
	if n := builds(t, recorder); n != 0 {
		t.Fatalf("%d builds ran without a daemon", n)
	}

	// The daemon answers but the build fails: the build's own error is the reason.
	a, _ = boxCheckApp(t, "rt", 1)
	stampBoxDefinition(t, a.cfg, "coop v9.0.0-144-g091fdf9\ndef stale\n")
	got = captureStderr(t, func() { err = a.checkCoopBox(t.TempDir(), boxCheckImage) })
	if !errors.Is(err, ui.ErrReported) {
		t.Fatalf("a rendered build failure must come back reported, got %v", err)
	}
	want = "  ✗ Could not update the box\n" +
		"\n" +
		"        image build failed (exit 1)\n" +
		"\n" +
		"    Fix what the build reported above, then run 'coop codex' again — the update is retried automatically.\n"
	if !strings.HasSuffix(got, want) {
		t.Fatalf("build-failure section ended:\n%q\nwant:\n%q", got, want)
	}
	if strings.Contains(got, "✓") {
		t.Fatalf("a failed update claimed success:\n%s", got)
	}
}
