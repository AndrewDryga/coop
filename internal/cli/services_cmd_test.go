package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
)

// The refusal names the folder Coop would not use, and tells the person to change TMPDIR only when
// TMPDIR chose that folder.
func TestComposeRefusalNamesTheExposedFolder(t *testing.T) {
	for _, tc := range []struct {
		fromTMPDIR bool
		want, not  string
	}{
		{true, "The temporary directory /work/tmp is inside a folder exposed to an agent.", "Keep that folder"},
		{false, "Coop's private folder /work/run is inside a folder exposed to an agent.", "TMPDIR"},
	} {
		dir := "/work/run"
		if tc.fromTMPDIR {
			dir = "/work/tmp"
		}
		err := &box.ExposedTempDirError{Dir: dir, FromTMPDIR: tc.fromTMPDIR}
		out := captureStderr(t, func() { reportExit(-1, composeRefusal("Could not start services", "compose.yml", err, "coop up")) })
		if !strings.Contains(out, tc.want) || strings.Contains(out, tc.not) {
			t.Errorf("TMPDIR=%v refusal:\n%s\nwant %q and no %q", tc.fromTMPDIR, out, tc.want, tc.not)
		}
	}
}

// coop up names every setting it refuses on a line of its own, with the service and line, and runs
// nothing.
func TestUpNamesEveryRefusedComposeSetting(t *testing.T) {
	t.Setenv(box.ServiceStateRootEnv, t.TempDir())
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "services:\n  app:\n    image: alpine\n    privileged: true\n    cap_add: [NET_ADMIN]\n"
	if err := os.WriteFile(filepath.Join(repo, ".agent", "compose.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{RepoOverride: repo}, rt: composeShim{services: []string{"app"}}.build(t), rtSet: true}
	var code int
	out := captureTerminal(t, func() { code, _ = a.cmdUp(nil) })
	want := "✗ Could not start services from .agent/compose.yml\n\n" +
		"      Service \"app\": privileged is not allowed (line 4).\n" +
		"      Service \"app\": cap_add is not allowed (line 5).\n\n" +
		"  Fix .agent/compose.yml, then run coop up again.\n"
	if code != 1 || !strings.Contains(out, want) || strings.Contains(out, "unmarshal") || strings.Contains(out, "[Compose output]") {
		t.Fatalf("refused settings = %d:\n%s\nwant:\n%s", code, out, want)
	}
}

// Progress for a wrapper with a hashed name says nothing a person can use; its failure still shows.
func TestDistinctLinesHidesHashedContainerProgress(t *testing.T) {
	var out bytes.Buffer
	d := &distinctLines{w: &out}
	_, _ = d.Write([]byte(" Container coop-fs-438b3f018f2558a35aab-service Running \n" +
		" Container coop-fs-438b3f018f2558a35aab-service Error dependency failed\n" +
		" Container web-1 Running \n"))
	want := " Container coop-fs-438b3f018f2558a35aab-service Error dependency failed\n Container web-1 Running \n"
	if out.String() != want {
		t.Errorf("shown = %q", out.String())
	}
}
