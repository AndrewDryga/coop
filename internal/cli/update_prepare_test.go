package cli

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

func updateReleaseFixture(t *testing.T, binary string) string {
	t.Helper()
	t.Cleanup(stub(&Version, "2.7.2"))
	releaseAPI(t, `{"tag_name":"v2.7.3"}`, 0)
	archive := testReleaseArchive(t, binary, "coop")
	asset := fmt.Sprintf("coop_2.7.3_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	server := testReleaseServer(t, archive, releaseChecksum(asset)(archive), nil)
	t.Cleanup(server.Close)
	t.Cleanup(stub(&releaseFileURLFor, func(tag, name string) string {
		return server.URL + "/" + tag + "/" + name
	}))
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "installed coop")
	mustWrite(t, exe, "old executable")
	t.Cleanup(stub(&executablePath, func() (string, error) { return exe, nil }))
	return exe
}

func TestUpdateHandsOffToTheVerifiedInstalledExecutable(t *testing.T) {
	exe := updateReleaseFixture(t, "new executable")
	link := filepath.Join(t.TempDir(), "coop")
	if err := os.Symlink(exe, link); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stub(&executablePath, func() (string, error) { return link, nil }))
	t.Setenv("COOP_TEST_UPDATE_CUSTODY", "original environment")
	called := false
	t.Cleanup(stub(&updateExec, func(path string, args, env []string) error {
		called = true
		if path != exe || !reflect.DeepEqual(args, []string{exe, "update", "--box-only"}) || !reflect.DeepEqual(env, os.Environ()) {
			t.Fatalf("wrong installed-binary handoff: %q %q", path, args)
		}
		if data, err := os.ReadFile(path); err != nil || string(data) != "new executable" {
			t.Fatalf("handoff preceded verified installation: %q / %v", data, err)
		}
		return nil
	}))
	a := &app{cfg: &config.Config{BaseImage: "old-definition-must-not-be-exported"}}
	code, err := a.cmdUpdate(nil)
	if code != 0 || err != nil || !called || a.rtSet {
		t.Fatalf("update handoff: %d / %v / called=%v / old runtime=%v", code, err, called, a.rtSet)
	}
}

func TestUpdateHandoffFailureNeverBuildsWithOldDefinitions(t *testing.T) {
	updateReleaseFixture(t, "new executable")
	t.Cleanup(stub(&updateExec, func(string, []string, []string) error { return errors.New("exec denied") }))
	a := &app{cfg: &config.Config{}}
	var code int
	var err error
	out := captureTerminal(t, func() { code, err = a.cmdUpdate(nil) })
	if code != 1 || err == nil || !strings.Contains(out, "v2.7.3 is installed") ||
		!strings.Contains(out, "coop update --box-only") || a.rtSet {
		t.Fatalf("handoff failure lost installed version or continued old build: %d / %v\n%s", code, err, out)
	}
}

func TestSelfOnlyUpdateDoesNotPrepareImages(t *testing.T) {
	updateReleaseFixture(t, "new executable")
	t.Cleanup(stub(&updateExec, func(string, []string, []string) error {
		t.Fatal("self-only attempted image handoff")
		return nil
	}))
	a := &app{cfg: &config.Config{}}
	if code, err := a.cmdUpdate([]string{"--self-only"}); code != 0 || err != nil || a.rtSet {
		t.Fatalf("self-only: %d / %v", code, err)
	}
}

func TestUpdateBoxUsesTheEffectiveNetworkMode(t *testing.T) {
	for _, mode := range []string{"open", "none", "filtered"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			cfg := projectBoxConfig(t, true)
			cfg.SetEgress(mode)
			a := &app{cfg: cfg, rt: buildShim{daemonUp: true}.build(t), rtSet: true}
			var code int
			var err error
			out := captureTerminal(t, func() { code, err = a.cmdUpdate([]string{"--box-only"}) })
			if mode == "filtered" {
				// This fixture has no Docker API. It must reach the filtered builder rather than
				// execute ordinary project instructions and falsely report a ready image.
				if code != 1 || err == nil || !strings.Contains(out, "Updating the filtered box image") ||
					strings.Contains(out, "[Build output]") || strings.Contains(out, "Box image updated") {
					t.Fatalf("filtered update used ordinary build: %d / %v\n%s", code, err, out)
				}
			} else if code != 0 || err != nil || !strings.Contains(out, "[Build output]") || !strings.Contains(out, "Box image updated") {
				t.Fatalf("%s update did not retain ordinary explicit build: %d / %v\n%s", mode, code, err, out)
			}
		})
	}
}

func TestUpdateCheckDoesNotReportOrdinaryImageForFilteredProject(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Cleanup(stub(&Version, "2.7.2"))
	releaseAPI(t, `{"tag_name":"v2.7.2"}`, 0)
	cfg := projectBoxConfig(t, true)
	cfg.SetEgress("filtered")
	a := &app{cfg: cfg}
	var code int
	var err error
	out := captureTerminal(t, func() { code, err = a.cmdUpdateCheck() })
	if code != 0 || err != nil || a.rtSet || strings.Contains(out, "No local build record") ||
		!strings.Contains(out, "--check does not inspect Docker") {
		t.Fatalf("filtered dry run reported an unrelated ordinary image: %d / %v\n%s", code, err, out)
	}
}

// The test process is the old updater; exec replaces it with the newly installed fixture.
// Nothing touches the user's binary or container runtime.
func TestUpdateActuallyExecutesTheInstalledRelease(t *testing.T) {
	if os.Getenv("COOP_TEST_UPDATE_EXEC_CHILD") == "1" {
		Version = "2.7.2"
		githubLatestURL = os.Getenv("COOP_TEST_UPDATE_API")
		releaseRoot := os.Getenv("COOP_TEST_UPDATE_RELEASE")
		releaseFileURLFor = func(tag, name string) string { return releaseRoot + "/" + tag + "/" + name }
		executablePath = func() (string, error) { return os.Getenv("COOP_TEST_UPDATE_EXECUTABLE"), nil }
		a := &app{cfg: &config.Config{BaseImage: "old definition"}}
		code, err := a.cmdUpdate(nil)
		t.Fatalf("installed executable did not replace old process: %d / %v", code, err)
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"tag_name":"v2.7.3"}`)
	}))
	defer api.Close()
	marker := filepath.Join(t.TempDir(), "continued")
	body := "#!/bin/sh\n[ \"$1\" = update ] && [ \"$2\" = --box-only ] && [ \"$#\" = 2 ] || exit 92\n" +
		"[ \"$COOP_TEST_UPDATE_CUSTODY\" = original ] || exit 93\n" +
		"printf '%s\\n%s\\n' \"$0\" \"$PWD\" > \"$COOP_TEST_UPDATE_MARKER\"\nexit 37\n"
	archive := testReleaseArchive(t, body, "coop")
	asset := fmt.Sprintf("coop_2.7.3_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	release := testReleaseServer(t, archive, releaseChecksum(asset)(archive), nil)
	defer release.Close()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "coop")
	mustWrite(t, exe, "old")
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "-test.run=^TestUpdateActuallyExecutesTheInstalledRelease$")
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "COOP_TEST_UPDATE_EXEC_CHILD=1", "COOP_TEST_UPDATE_API="+api.URL,
		"COOP_TEST_UPDATE_RELEASE="+release.URL, "COOP_TEST_UPDATE_EXECUTABLE="+exe,
		"COOP_TEST_UPDATE_CUSTODY=original", "COOP_TEST_UPDATE_MARKER="+marker)
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 37 {
		t.Fatalf("new executable's exit was not propagated: %v\n%s", err, output)
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != exe+"\n"+cwd+"\n" {
		t.Fatalf("installed executable/cwd changed: %q / %v", data, err)
	}
}
