package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/box"
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
	cfg.ExtraRunArgs = []string{"-v", filepath.Join(t.TempDir(), "operator-data") + ":/leak"}
	a := &app{cfg: cfg, rt: recordingRuntime(t, recorder)}
	verifier := verifierDir(t, "verify.sh", "exit 1\n", 0o644)

	// run == nil: the real box.Run assembles and launches.
	result := a.gradeSnapshot(context.Background(), gradeRequest{
		Image: "coop-box:trusted", Workspace: snapshot, Verifier: verifier, CaseID: "c",
	}, nil)

	data, rerr := os.ReadFile(recorder)
	if rerr != nil {
		t.Fatalf("the grading launch never reached the runtime: %v; result: %+v", rerr, result)
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
	if strings.Contains(argv, "/leak") || strings.Contains(argv, "operator-data") {
		t.Errorf("the grader inherited an operator runtime mount.\nargv: %s", argv)
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

// A candidate asked to produce a file that happens to be named like a secret — a .env, a key — must
// be graded on what it actually wrote. Secret shadowing replaces such files with empty decoys, which
// protects a MODEL from someone's stray credential but would silently fail the case here, because a
// grader is trusted code that has to see the truth.
func TestGradingSeesFilesNamedLikeSecrets(t *testing.T) {
	snapshot := t.TempDir()
	for name, body := range map[string]string{
		".env":         "ANSWER=42\n",
		"server.pem":   "-----BEGIN CERTIFICATE-----\n",
		"ordinary.txt": "plain\n",
	} {
		if err := os.WriteFile(filepath.Join(snapshot, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	recorder := filepath.Join(t.TempDir(), "argv.log")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.ExtraRunArgs = nil
	a := &app{cfg: cfg, rt: recordingRuntime(t, recorder)}
	verifier := verifierDir(t, "verify.sh", "exit 0\n", 0o644)
	a.gradeSnapshot(context.Background(), gradeRequest{
		Image: "coop-box:trusted", Workspace: snapshot, Verifier: verifier,
	}, nil)

	data, rerr := os.ReadFile(recorder)
	if rerr != nil {
		t.Fatalf("the grading launch never reached the runtime: %v", rerr)
	}
	argv := string(data)
	// A decoy mount targets the file's path inside the box; none may exist for the graded tree.
	for _, name := range []string{".env", "server.pem"} {
		if strings.Contains(argv, gradeWorkspaceDir+"/"+name) {
			t.Errorf("%s was shadowed with a decoy; the grader would see an empty file:\n%s", name, argv)
		}
	}
}

// The rail: the unshadowed mount must never be combined with model credentials.
func TestGradeSnapshotIsRefusedWithCredentials(t *testing.T) {
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	code, err := box.Run(cfg, recordingRuntime(t, filepath.Join(t.TempDir(), "argv.log")), box.RunSpec{
		Image: "img", Repo: t.TempDir(), Cmd: []string{"true"},
		GradeSnapshot: true, Homes: true,
	})
	if err == nil {
		t.Fatal("an unshadowed tree was allowed beside agent credentials")
	}
	if code != -1 {
		t.Errorf("exit code = %d, want -1", code)
	}
}
