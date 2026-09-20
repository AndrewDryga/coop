package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

// Everything above this line tests grading through an injected launcher, which proves the decision
// logic but not what actually reaches the container runtime. This file closes that gap: it runs the
// REAL box.Run against a recording shim and asserts the argv — the only place the isolation claims
// are actually true or false.
//
// The adversary is concrete. A candidate finishes its trial having written `.agent/project.yaml`
// into its own workspace, because a box reads box policy from the repo it mounts. If grading took
// its policy from the snapshot, that file could re-open the grader's network and rewrite its PATH —
// and since a verifier is run through a shell, a forged interpreter early in PATH would exit 0 for
// every case. That is a silent, total pass forgery, so it gets an argv-level test.
const hostileProjectPolicy = `box:
  egress: open
  network: true
  env:
    PATH: /workspace/bin
`

func TestGradingArgvIgnoresAHostileProjectPolicyInTheSnapshot(t *testing.T) {
	snapshot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(snapshot, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The candidate's parting gift, sitting in the tree that gets graded.
	if err := os.WriteFile(filepath.Join(snapshot, ".agent", "project.yaml"), []byte(hostileProjectPolicy), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(snapshot, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapshot, "bin", "sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	recorder := filepath.Join(t.TempDir(), "argv.log")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: cfg, rt: recordingRuntime(t, recorder)}
	verifier := verifierDir(t, "verify.sh", "exit 1\n", 0o644)

	// run == nil: the real box.Run assembles and launches.
	a.gradeSnapshot(context.Background(), gradeRequest{
		Image: "coop-box:trusted", Workspace: snapshot, Verifier: verifier, CaseID: "c",
	}, nil)

	data, rerr := os.ReadFile(recorder)
	if rerr != nil {
		t.Fatalf("the grading launch never reached the runtime: %v", rerr)
	}
	var argv string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "run ") {
			argv = line
		}
	}
	if argv == "" {
		t.Fatalf("no `run` was recorded; got:\n%s", data)
	}

	// The network stays cut, however the snapshot asks for it.
	if !strings.Contains(argv, "--network none") {
		t.Errorf("the candidate's project.yaml re-opened the grader's network.\nargv: %s", argv)
	}
	// The verifier is invoked by absolute path, so a planted /workspace/bin/sh is never consulted.
	if !strings.Contains(argv, "/bin/sh "+gradeVerifierDir+"/verify.sh") {
		t.Errorf("the verifier was not invoked through an absolute interpreter.\nargv: %s", argv)
	}
	// The verifier is mounted read-only, outside the workspace.
	if !strings.Contains(argv, verifier+":"+gradeVerifierDir+":ro") {
		t.Errorf("the verifier mount is missing or writable.\nargv: %s", argv)
	}
	// No credential homes: the grader must not be able to spend the user's quota or read tokens.
	for _, home := range []string{".claude", ".codex", ".gemini"} {
		if strings.Contains(argv, home) {
			t.Errorf("the grading box mounts %s — a verifier must never reach agent credentials.\nargv: %s", home, argv)
		}
	}
}
