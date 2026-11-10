package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNPMAuditMakeTargetFailsClosed(t *testing.T) {
	makeBin, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make not installed")
	}
	makefile, err := filepath.Abs("Makefile")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	version := makeVariable(t, "NPM_VERSION")
	script := `#!/bin/sh
if [ "$1" = --version ]; then printf '%s\n' "$TEST_NPM_VERSION"; exit 0; fi
printf '%s\n' "$@" > "$TEST_NPM_ARGS"
exit "$TEST_NPM_EXIT"
`
	if err := os.WriteFile(filepath.Join(dir, "npm"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TEST_NPM_VERSION", version)
	t.Setenv("npm_config_offline", "true")
	t.Setenv("npm_config_omit", "peer")
	t.Setenv("TEST_NPM_ARGS", filepath.Join(dir, "args"))
	for _, code := range []string{"0", "1", "2"} {
		t.Setenv("TEST_NPM_EXIT", code)
		out, err := exec.Command(makeBin, "--no-print-directory", "-f", makefile, "npm-audit").CombinedOutput()
		if code == "0" && err != nil || code != "0" && err == nil {
			t.Fatalf("audit exit %s: %v\n%s", code, err, out)
		}
	}
	args, err := os.ReadFile(filepath.Join(dir, "args"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"audit\n", "--package-lock-only", "--ignore-scripts", "--offline=false", "--include=prod", "--include=dev", "--include=optional", "--include=peer", "--audit-level=high", "--registry=https://registry.npmjs.org", "--userconfig=/dev/null", "--globalconfig="} {
		if !strings.Contains(string(args), want) {
			t.Errorf("missing audit constraint %q", want)
		}
	}
	t.Setenv("TEST_NPM_VERSION", "wrong")
	t.Setenv("TEST_NPM_EXIT", "0")
	if out, err := exec.Command(makeBin, "--no-print-directory", "-f", makefile, "npm-audit").CombinedOutput(); err == nil {
		t.Fatalf("wrong audit tool passed: %s", out)
	}
}
