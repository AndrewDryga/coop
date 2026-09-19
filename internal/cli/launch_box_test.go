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
		"case \"$1\" in build) exit " + strconv.Itoa(buildExit) + " ;; info) [ \"$2\" = --format ] && echo linux/aarch64 && exit 0; exit 1 ;; esac\n"
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

func TestLoginDoesNotRebuildAStaleImage(t *testing.T) {
	a, recorder := boxCheckApp(t, "rt", 1)
	a.loginProvider = "gemini"
	stampBoxDefinition(t, a.cfg, "coop old\ndef stale\n")
	got := captureStderr(t, func() {
		if err := a.checkCoopBox(t.TempDir(), boxCheckImage); err != nil {
			t.Errorf("login image check: %v", err)
		}
	})
	if got != "" || builds(t, recorder) != 0 {
		t.Fatalf("login tried to update its image: %s", got)
	}
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
		"        rt build exited with status 1\n" +
		"\n" +
		"    Fix what the build reported above, then run 'coop codex' again — the update is retried automatically.\n"
	if !strings.HasSuffix(got, want) {
		t.Fatalf("build-failure section ended:\n%q\nwant:\n%q", got, want)
	}
	if strings.Contains(got, "✓") {
		t.Fatalf("a failed update claimed success:\n%s", got)
	}
}

const (
	olderBase = "coop-box:0123456789abcdef0123456789abcdef"
	newerBase = "coop-box:fedcba9876543210fedcba9876543210"
)

// tagShimApp is a launch-bound app over a runtime that knows which image tags exist: `image
// inspect` answers from that list and a build adds the tag it was given, which is enough to model
// two Coop versions' bases on one host.
func tagShimApp(t *testing.T, base string, present ...string) (*app, string) {
	t.Helper()
	dir := t.TempDir()
	recorder, tags := filepath.Join(dir, "runtime-args"), filepath.Join(dir, "tags")
	if err := os.WriteFile(tags, []byte(strings.Join(present, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho \"$@\" >> " + strconv.Quote(recorder) + "\n" +
		"case \"$1\" in\n" +
		"image) grep -qxF \"$3\" " + strconv.Quote(tags) + "; exit $? ;;\n" +
		"build) while [ $# -gt 0 ]; do [ \"$1\" = -t ] && echo \"$2\" >> " + strconv.Quote(tags) + "; shift; done; exit 0 ;;\n" +
		"info) [ \"$2\" = --format ] && echo linux/aarch64 && exit 0; exit 1 ;;\n" +
		"esac\n"
	shim := filepath.Join(dir, "rt")
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ConfigDir: t.TempDir(), BoxHome: t.TempDir(), BaseImage: base}
	return &app{cfg: cfg, rt: runtime.Runtime{Name: shim}, rtSet: true, argv: []string{"codex"}}, recorder
}

// stampImage writes the build record a Coop leaves beside an image it built.
func stampImage(t *testing.T, cfg *config.Config, image, contents string) {
	t.Helper()
	path := filepath.Join(cfg.BoxHome, "image-meta", strings.NewReplacer("/", "_", ":", "_").Replace(image))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Acceptance: upgrading Coop changes the definition and so the tag. A launch that finds this Coop's
// base missing where a Coop built one before builds it in the section a mismatched base always
// got, naming who built the earlier one, and leaves that earlier base alone.
func TestEnsureManagedBaseBuildsThisDefinitionBesideAnEarlierOne(t *testing.T) {
	a, recorder := tagShimApp(t, newerBase, olderBase)
	stampImage(t, a.cfg, olderBase, "coop v9.0.0-226-g87e5d13\ndef older\n")
	var err error
	got := captureStderr(t, func() { err = a.ensureManagedBase() })
	if err != nil {
		t.Fatalf("ensureManagedBase: %v", err)
	}
	for _, want := range []string{
		"\nChecking the Coop box\n",
		"  Current image was built by Coop v9.0.0-226-g87e5d13\n",
		"  ✓ Box updated\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("section is missing %q:\n%s", want, got)
		}
	}
	args, _ := os.ReadFile(recorder)
	if n := builds(t, recorder); n != 1 || !strings.Contains(string(args), "-t "+newerBase) || strings.Contains(string(args), "rmi") {
		t.Fatalf("%d builds, runtime calls:\n%s\nwant one build of %s and nothing done to %s", n, args, newerBase, olderBase)
	}
	if got := captureStderr(t, func() { err = a.ensureManagedBase() }); err != nil || got != "" || builds(t, recorder) != 1 {
		t.Fatalf("a built base was built again: %q (%v)", got, err)
	}
}

// Acceptance: a first build stays the operator's, so a host that never built a Coop base keeps
// the plain "run 'coop build'"; and an operator's COOP_BASE_IMAGE is never built on their behalf.
func TestEnsureManagedBaseLeavesAFirstBuildAndAnOperatorsBaseAlone(t *testing.T) {
	fresh, freshRecorder := tagShimApp(t, newerBase)
	operator, operatorRecorder := tagShimApp(t, "mybase:1")
	stampImage(t, operator.cfg, olderBase, "coop v9.0.0-226-g87e5d13\ndef older\n")
	for name, a := range map[string]*app{"a fresh host": fresh, "an operator's base": operator} {
		if got := captureStderr(t, func() {
			if err := a.ensureManagedBase(); err != nil {
				t.Errorf("%s: %v", name, err)
			}
		}); got != "" {
			t.Errorf("%s printed %q", name, got)
		}
	}
	if builds(t, freshRecorder)+builds(t, operatorRecorder) != 0 {
		t.Fatal("a base nobody asked Coop to build was built")
	}
}

// Acceptance: two Coop versions on one host each run their own base. With both present neither
// launch builds anything, whatever the other's record says — before the tag named the definition,
// each one's record made the other rebuild the one shared tag.
func TestTwoCoopVersionsKeepTheirOwnBases(t *testing.T) {
	older, olderRecorder := tagShimApp(t, olderBase, olderBase, newerBase)
	newer, newerRecorder := tagShimApp(t, newerBase, olderBase, newerBase)
	newer.cfg.BoxHome = older.cfg.BoxHome // one host
	stampImage(t, older.cfg, olderBase, "coop v9.0.0-226-g87e5d13\ndef older\n")
	stampImage(t, older.cfg, newerBase, "coop v9.0.0-375-gc0660cc\ndef newer\n")
	stampImage(t, older.cfg, boxCheckImage, "coop v8.9.0\ndef untagged\n")
	for name, a := range map[string]*app{"the older Coop": older, "the newer Coop": newer} {
		if got := captureStderr(t, func() {
			if err := a.ensureManagedBase(); err != nil {
				t.Errorf("%s: %v", name, err)
			}
			if err := a.checkCoopBox(t.TempDir(), a.cfg.BaseImage); err != nil {
				t.Errorf("%s: %v", name, err)
			}
		}); got != "" {
			t.Errorf("%s printed %q", name, got)
		}
	}
	if n := builds(t, olderRecorder) + builds(t, newerRecorder); n != 0 {
		t.Fatalf("%d builds ran on a host where both bases were current", n)
	}
}

// Acceptance: after an upgrade each launch gets this Coop's base where it can use it. An ordinary
// launch resolves without building and its box check, run once the posture is known, builds it; a
// filtered one never runs the base, so it builds nothing; a fork, loop or ACP caller of
// resolveImage and a restricted launch build it straight away.
func TestLaunchesBuildThisCoopsBaseAfterAnUpgrade(t *testing.T) {
	for _, tc := range []struct {
		name   string
		launch func(*app) (string, error)
		builds int
	}{
		{"an ordinary launch", func(a *app) (string, error) {
			repo, img, err := a.resolveLaunchImage(true)
			if err == nil {
				err = a.checkCoopBox(repo, img)
			}
			return img, err
		}, 1},
		{"a filtered launch", func(a *app) (string, error) { _, img, err := a.resolveLaunchImage(true); return img, err }, 0},
		{"a fork, loop or ACP launch", func(a *app) (string, error) { _, img, err := a.resolveImage(); return img, err }, 1},
		{"a restricted launch", func(a *app) (string, error) { img, _, err := a.restrictedImage(); return img, err }, 1},
	} {
		a, recorder := tagShimApp(t, newerBase, olderBase)
		a.cfg.RepoOverride = t.TempDir()
		stampImage(t, a.cfg, olderBase, "coop v9.0.0-226-g87e5d13\ndef older\n")
		var img string
		var err error
		captureStderr(t, func() { img, err = tc.launch(a) })
		if err != nil || img != newerBase || builds(t, recorder) != tc.builds {
			t.Errorf("%s = %q, %v after %d builds; want this Coop's base and %d builds", tc.name, img, err, builds(t, recorder), tc.builds)
		}
	}
}

// Acceptance: a stopped daemon reads as a missing image, and must not be narrated as an update:
// the launch reports the daemon, as before the tag named the definition.
func TestEnsureManagedBaseLeavesAStoppedDaemonToTheLaunch(t *testing.T) {
	a, recorder := tagShimApp(t, newerBase)
	shim := filepath.Join(t.TempDir(), "docker") // EnsureDaemon probes only the Docker kind
	if err := os.WriteFile(shim, []byte("#!/bin/sh\necho \"$@\" >> "+strconv.Quote(recorder)+"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	a.rt = runtime.Runtime{Name: shim}
	a.cfg.RepoOverride = t.TempDir()
	stampImage(t, a.cfg, newerBase, "coop v9.0.0-375-gc0660cc\ndef newer\n")
	var err error
	got := captureStderr(t, func() { _, _, err = a.resolveImage() })
	if err == nil || !strings.Contains(err.Error(), "Docker") || strings.Contains(got, "Checking the Coop box") || builds(t, recorder) != 0 {
		t.Fatalf("a stopped daemon = %v, narrated %q after %d builds; want the daemon named and nothing built", err, got, builds(t, recorder))
	}
}

// Acceptance: a base this Coop built that has since been removed (a prune) is rebuilt, and said
// plainly — it is missing, not the image of another Coop.
func TestEnsureManagedBaseRebuildsARemovedBase(t *testing.T) {
	a, recorder := tagShimApp(t, newerBase)
	stampImage(t, a.cfg, newerBase, "coop v9.0.0-375-gc0660cc\ndef newer\n")
	var err error
	got := captureStderr(t, func() { err = a.ensureManagedBase() })
	if err != nil || !strings.Contains(got, "  The box image is missing.\n") || strings.Contains(got, "built by") || builds(t, recorder) != 1 {
		t.Fatalf("removed base = %v after %d builds:\n%s", err, builds(t, recorder), got)
	}
}
