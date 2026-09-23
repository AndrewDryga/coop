package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/eval"
)

// The denial matrix.
//
// Every other test here asserts that isolation is CONFIGURED — this one asserts it WORKS, the only
// way that can be asserted mechanically: plant a canary in the hidden material, then go looking for
// it from the candidate's side through every route it could actually take. If a case's expected
// answer ever reaches the model, the eval stops measuring capability and starts measuring who
// found the answer key, and nothing downstream would look wrong.
//
// Routes covered here are the ones a test can check without a live model: the workspace tree, the
// git repository that travels with it (history, objects, packed refs, reflog, alternates), and
// everything the candidate's container is handed — mounts, command, environment. The routes a test
// cannot reach (a model's own web search, a provider-hosted retrieval index) are handled by
// construction instead: a trial runs with MCP off and, on `--egress filtered`, with the gateway
// deciding what it may reach at all.
const canary = "CANARY-e3f19a7c-the-expected-answer-must-never-reach-the-model"

// canarySuite writes a suite whose hidden material is saturated with the canary: the verifier
// script, a reference solution beside it, and the expected output.
func canarySuite(t *testing.T) *eval.Suite {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("suite.yaml", `version: 1
name: canary-suite
runner: agent
cases:
  - id: guarded
    instruction: "do the task"
    files: ./files
    verifier: ./verifiers/guarded
    timeout: 5m
`)
	write("files/README.md", "# the candidate's starting point\n")
	write("verifiers/guarded/verify.sh", "grep -q '"+canary+"' answer.txt || exit 1\n")
	write("verifiers/guarded/reference-solution.txt", canary+"\n")
	write("verifiers/guarded/expected-output.txt", canary+"\n")

	suite, err := eval.Load(filepath.Join(dir, "suite.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return suite
}

// grepTree reports every file under root whose bytes contain the canary.
func grepTree(t *testing.T, root string) []string {
	t.Helper()
	var hits []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil // an unreadable entry cannot be a leak the candidate can read either
		}
		body, readErr := os.ReadFile(p)
		if readErr != nil {
			return nil
		}
		if strings.Contains(string(body), canary) {
			rel, _ := filepath.Rel(root, p)
			hits = append(hits, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hits
}

// The candidate's workspace — and the git repository inside it — must not contain the answer by any
// route: a working file, a commit, a loose or packed object, a ref, or the reflog. `git log -S` is
// asked directly, because a file deleted in a later commit is still in the history.
func TestCanaryNeverReachesTheCandidateWorkspace(t *testing.T) {
	suite := canarySuite(t)
	workspace := filepath.Join(t.TempDir(), "ws")
	if _, err := eval.PrepareWorkspace(context.Background(), filepath.Join(suite.Dir, "files"), workspace); err != nil {
		t.Fatal(err)
	}
	// 1. Nothing in the tree the candidate is given, including its .git.
	if hits := grepTree(t, workspace); len(hits) > 0 {
		t.Errorf("the answer is readable inside the candidate's workspace: %v", hits)
	}

	// 2. Nothing reachable through git, which a model absolutely will try.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	for _, probe := range [][]string{
		{"log", "--all", "-S" + canary, "--oneline"},
		{"log", "--all", "--format=%H %s"},
		{"rev-list", "--all", "--objects"},
		{"reflog", "--all"},
		{"stash", "list"},
	} {
		out, _ := exec.Command("git", append([]string{"-C", workspace}, probe...)...).CombinedOutput()
		if strings.Contains(string(out), canary) {
			t.Errorf("`git %s` exposes the answer:\n%s", strings.Join(probe, " "), out)
		}
	}
	// 3. No alternates file pointing at another object store (the classic way a "clean" repo still
	// reaches the author's objects).
	for _, p := range []string{".git/objects/info/alternates", ".git/commondir"} {
		if body, err := os.ReadFile(filepath.Join(workspace, p)); err == nil {
			t.Errorf("%s exists and would reach objects outside the trial: %q", p, body)
		}
	}
}

// Everything handed to the candidate's container: mounts, command, environment. A model that reads
// its own /proc/self/environ or looks at its mount table must find nothing.
func TestCanaryNeverReachesTheCandidateContainer(t *testing.T) {
	suite := canarySuite(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.ExtraRunArgs = nil // isolate the argv proof from this host's personal runtime mounts
	cfg.Homes = true
	recorder := filepath.Join(t.TempDir(), "argv.log")
	r := &trialRunner{
		app: &app{cfg: cfg, rt: recordingRuntime(t, recorder)}, suite: suite,
		image: "coop-box:test", workRoot: t.TempDir(),
	}
	r.run(context.Background(), eval.Trial{
		Case:   suite.Cases[0],
		Config: eval.FrozenConfig{Kind: eval.ConfigTarget, Label: "codex"},
	})

	data, rerr := os.ReadFile(recorder)
	if rerr != nil {
		t.Fatalf("nothing was launched: %v", rerr)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "run ") {
			continue
		}
		isGrader := strings.Contains(line, gradeVerifierDir)
		if strings.Contains(line, canary) && !isGrader {
			t.Errorf("the answer appears in a candidate launch:\n%s", line)
		}
		// The verifier directory itself must be mounted into the grader and nothing else.
		if !isGrader && strings.Contains(line, filepath.Join(suite.Dir, "verifiers")) {
			t.Errorf("the verifier directory is mounted into a non-grading box:\n%s", line)
		}
		// Any env-file or config file the launch references must also be clean.
		for _, field := range strings.Fields(line) {
			host, _, _ := strings.Cut(field, ":")
			if !strings.HasPrefix(host, "/") {
				continue
			}
			body, err := os.ReadFile(host)
			if err != nil || !strings.Contains(string(body), canary) {
				continue
			}
			if !isGrader {
				t.Errorf("a file mounted into a candidate box contains the answer: %s", host)
			}
		}
	}
}

// And the grader — the one place the answer is allowed — must actually have it, or the matrix above
// would pass just as well with the verifier never mounted anywhere.
func TestCanaryDoesReachTheGrader(t *testing.T) {
	suite := canarySuite(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: cfg}
	var mounted string
	a.gradeSnapshot(context.Background(), gradeRequest{
		Image: "img", Workspace: t.TempDir(),
		Verifier: filepath.Join(suite.Dir, suite.Cases[0].Verifier),
	}, func(spec box.RunSpec) (int, error) {
		mounted = strings.Join(spec.ExtraArgs, " ")
		return 0, nil
	})
	if !strings.Contains(mounted, "coop-verifier") {
		t.Fatalf("the grader was not given the verifier at all: %q", mounted)
	}
	hits := grepTree(t, filepath.Join(suite.Dir, suite.Cases[0].Verifier))
	if len(hits) == 0 {
		t.Error("the canary fixture is broken: the hidden material does not contain the canary, so the denial matrix proves nothing")
	}
}
