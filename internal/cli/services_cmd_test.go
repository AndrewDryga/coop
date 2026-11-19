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

// An agent can break or delete the Compose file after coop up. coop down still stops the project's
// containers, by the Compose project label coop derived from the workspace path, and runs nothing
// from the file: no Compose command at all. With --delete-volumes it deletes the reviewed volumes.
func TestDownWithoutAValidComposeFile(t *testing.T) {
	for name, tc := range map[string]struct {
		compose string // "" deletes the file
		args    []string
		want    []string
	}{
		"refused setting": {
			compose: "services:\n  app:\n    image: alpine\n    privileged: true\n",
			want:    []string{"Could not use .agent/compose.yml: Service \"app\": privileged is not allowed (line 4).", "✓ Services stopped\n  Stored data was kept."},
		},
		"deleted file": {
			want: []string{".agent/compose.yml does not exist.", "✓ Services stopped\n  Stored data was kept."},
		},
		"deleted file, delete volumes": {
			args: []string{"--delete-volumes", "--yes"},
			want: []string{".agent/compose.yml does not exist.", "✓ Services stopped; 1 volume deleted"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(box.ServiceStateRootEnv, t.TempDir())
			repo := t.TempDir()
			if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.compose != "" {
				if err := os.WriteFile(filepath.Join(repo, ".agent", "compose.yml"), []byte(tc.compose), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			project := composeProjectName(t, repo)
			log := filepath.Join(t.TempDir(), "runtime.log")
			shim := composeShim{services: []string{"app"}, running: []string{"0f1e2d3c4b5a"}, volumes: []string{project + "_data"}, log: log}
			a := &app{cfg: &config.Config{RepoOverride: repo}, rt: shim.build(t), rtSet: true}
			var code int
			var err error
			out := captureTerminal(t, func() { code, err = a.cmdDown(tc.args) })
			if code != 0 || err != nil {
				t.Fatalf("coop down = (%d, %v), want success:\n%s", code, err, out)
			}
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("output does not say %q:\n%s", want, out)
				}
			}
			data, readErr := os.ReadFile(log)
			if readErr != nil {
				t.Fatal(readErr)
			}
			calls := string(data)
			if !strings.Contains(calls, "ps -q -a --filter label=com.docker.compose.project="+project+"\nrm -f 0f1e2d3c4b5a\n") {
				t.Errorf("the project's containers were not removed by their label:\n%s", calls)
			}
			if strings.HasPrefix(calls, "compose ") || strings.Contains(calls, "\ncompose ") {
				t.Errorf("coop down ran Compose with the file it could not trust:\n%s", calls)
			}
			if deleted := strings.Contains(calls, "volume rm "+project+"_data\n"); deleted != (len(tc.args) > 0) {
				t.Errorf("volume deleted = %v, want %v:\n%s", deleted, len(tc.args) > 0, calls)
			}
		})
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
