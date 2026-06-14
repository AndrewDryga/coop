package cli

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/ui"
)

// doctorProbe runs inside the box against the fixture and reports, line by line,
// whether each secret is shadowed and each non-secret stays visible.
const doctorProbe = `#!/bin/sh
cd /workspace 2>/dev/null || { echo "RESULT FAIL workspace was not mounted"; exit 1; }
empty() { [ -f "$1" ] && [ ! -s "$1" ]; }
empty .env               && echo "RESULT PASS .env is shadowed (empty in the VM)"     || echo "RESULT FAIL .env is READABLE in the VM"
empty config/prod.tfvars && echo "RESULT PASS *.tfvars in a subdir is shadowed"       || echo "RESULT FAIL config/prod.tfvars is READABLE"
if [ -d secrets ] && [ -z "$(ls -A secrets 2>/dev/null)" ]; then echo "RESULT PASS secrets/ is shadowed (empty)"; else echo "RESULT FAIL secrets/ exposes files"; fi
if echo x >> .env 2>/dev/null; then echo "RESULT FAIL the .env decoy is writable"; else echo "RESULT PASS writing the .env decoy is blocked"; fi
[ -s .env.example ] && echo "RESULT PASS .env.example template stays readable" || echo "RESULT FAIL .env.example was hidden"
[ -s src/app.js ]   && echo "RESULT PASS source files stay readable"           || echo "RESULT FAIL source files were hidden"
if grep -rqs hunter2 . 2>/dev/null; then echo "RESULT FAIL secret value reachable in the tree"; else echo "RESULT PASS secret value appears nowhere the agent can read"; fi
`

type report struct{ pass, fail int }

func (r *report) ok(msg string) { r.pass++; fmt.Printf("  %s %s\n", ui.Check(), msg) }
func (r *report) no(msg string) { r.fail++; fmt.Printf("  %s %s\n", ui.Cross(), msg) }

// cmdDoctor proves isolation by attacking it: it builds a fixture repo full of
// secrets, runs the box against it, and checks that every secret is shadowed
// inside the sandbox and absent from a clone handoff.
func (a *app) cmdDoctor(args []string) (int, error) {
	if err := a.rt.EnsureDaemon(); err != nil {
		return -1, err
	}
	fixture, err := buildFixture()
	if err != nil {
		return -1, err
	}
	defer os.RemoveAll(fixture)

	// The probe lives outside the fixture: it must not appear in /workspace, or
	// its own "hunter2" grep pattern would trip the secret-value check.
	probeFile, err := os.CreateTemp("", "coop-probe-*.sh")
	if err != nil {
		return -1, err
	}
	probe := probeFile.Name()
	probeFile.Close()
	defer os.Remove(probe)
	if err := os.WriteFile(probe, []byte(doctorProbe), 0o644); err != nil {
		return -1, err
	}

	rep := &report{}
	fmt.Printf("%s  %s\n", ui.Bold("== coop doctor =="), ui.Dim("(runtime: "+a.rt.Name+")"))

	// --- inside the sandbox ---
	fmt.Printf("\n%s\n", ui.Bold("inside the sandbox"))
	var out bytes.Buffer
	_, runErr := box.Run(a.cfg, a.rt, box.RunSpec{
		Image: "alpine", Repo: fixture, Cmd: []string{"sh", "/probe.sh"},
		Batch: true, Quiet: true, Stdout: &out, Stderr: io.Discard,
		ExtraArgs: []string{"-v", probe + ":/probe.sh:ro"},
	})
	if runErr != nil || out.Len() == 0 {
		rep.no("the sandbox produced no output (the container failed to run)")
	}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		switch {
		case strings.HasPrefix(line, "RESULT PASS "):
			rep.ok(strings.TrimPrefix(line, "RESULT PASS "))
		case strings.HasPrefix(line, "RESULT FAIL "):
			rep.no(strings.TrimPrefix(line, "RESULT FAIL "))
		}
	}

	// --- on the host: the clone handoff ---
	fmt.Printf("\n%s\n", ui.Bold("on the host (the clone handoff)"))
	clone := fixture + "-clone"
	defer os.RemoveAll(clone)
	if err := exec.Command("git", "clone", "-q", fixture, clone).Run(); err != nil {
		rep.no(fmt.Sprintf("could not clone the fixture: %v", err))
	} else {
		checkAbsent(rep, filepath.Join(clone, ".env"), "gitignored .env never enters a clone", ".env leaked into the clone")
		checkAbsent(rep, filepath.Join(clone, "secrets"), "gitignored secrets/ never enters a clone", "secrets/ leaked into the clone")
		if fileExists(filepath.Join(clone, "src", "app.js")) {
			rep.ok("tracked source is present in the clone")
		} else {
			rep.no("tracked source missing")
		}
		if treeContains(clone, "hunter2") {
			rep.no("secret value leaked into the clone")
		} else {
			rep.ok("no secret value anywhere in the clone")
		}
		if origin, _ := exec.Command("git", "-C", clone, "remote", "get-url", "origin").Output(); strings.HasPrefix(strings.TrimSpace(string(origin)), "/") {
			rep.ok("clone origin is a local path — there is nowhere to push")
		} else {
			rep.no("clone origin is not a local path")
		}
	}

	fmt.Println()
	if rep.fail == 0 {
		fmt.Printf("%s — the box contains the agent.\n", ui.Bold(ui.Green(fmt.Sprintf("✓ all %d checks passed", rep.pass))))
		return 0, nil
	}
	fmt.Printf("%s\n", ui.Bold(ui.Red(fmt.Sprintf("✗ %d passed, %d failed", rep.pass, rep.fail))))
	return 1, nil
}

// buildFixture creates a throwaway git repo seeded with secrets and decoys.
func buildFixture() (string, error) {
	dir, err := os.MkdirTemp("", "coop-doctor-")
	if err != nil {
		return "", err
	}
	files := map[string]string{
		".env":               "SECRET=hunter2\n",
		".env.example":       "KEY=put-your-key-here\n",
		"config/prod.tfvars": "x = \"hunter2\"\n",
		"secrets/api-token":  "tok-hunter2\n",
		"src/app.js":         "console.log(1)\n",
		".gitignore":         ".env\n*.tfvars\nsecrets/\n",
	}
	for rel, body := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			return "", err
		}
	}
	cmds := [][]string{
		{"init", "-q"},
		{"add", "-A"},
		{"-c", "user.email=d@d", "-c", "user.name=d", "commit", "-qm", "init"},
	}
	for _, c := range cmds {
		cmd := exec.Command("git", append([]string{"-C", dir}, c...)...)
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("git %v: %w", c, err)
		}
	}
	return dir, nil
}

func checkAbsent(r *report, path, okMsg, noMsg string) {
	if !pathExists(path) {
		r.ok(okMsg)
	} else {
		r.no(noMsg)
	}
}

func treeContains(root, needle string) bool {
	found := false
	n := []byte(needle)
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if data, err := os.ReadFile(p); err == nil && bytes.Contains(data, n) {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}
