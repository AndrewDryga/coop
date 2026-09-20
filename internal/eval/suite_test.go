package eval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSuite writes a manifest plus, for every path it names, a placeholder file so the symlink and
// escape checks run against a real tree. Returns the manifest path.
func writeSuite(t *testing.T, manifest string, extra ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, rel := range extra {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "suite.yaml")
	if err := os.WriteFile(path, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const agentSuite = `version: 1
name: my-agents
runner: agent
cases:
  - id: hello
    instruction: "print hello"
    verifier: ./verifiers/hello
    timeout: 5m
`

const loopSuite = `version: 1
name: my-loops
runner: loop
loop_config: ./loop.yaml
cases:
  - id: repo-evolution
    fixture: ./fixtures/app
    tasks: ./queues/repo-evolution
    verifier: ./verifiers/repo-evolution
    timeout: 50m
`

func TestLoadAcceptsBothRunnerKinds(t *testing.T) {
	agent, err := Load(writeSuite(t, agentSuite, "verifiers/hello"))
	if err != nil {
		t.Fatalf("agent suite: %v", err)
	}
	if agent.Runner != RunnerAgent || len(agent.Cases) != 1 || agent.IsLoop() {
		t.Errorf("agent suite parsed wrong: %+v", agent)
	}
	loop, err := Load(writeSuite(t, loopSuite, "loop.yaml", "fixtures/app", "queues/repo-evolution", "verifiers/repo-evolution"))
	if err != nil {
		t.Fatalf("loop suite: %v", err)
	}
	if !loop.IsLoop() || loop.LoopConfig != "./loop.yaml" || loop.Cases[0].Timeout.String() != "50m0s" {
		t.Errorf("loop suite parsed wrong: %+v", loop)
	}
}

func TestLoadRefusesEveryMalformedManifestByName(t *testing.T) {
	cases := []struct {
		name     string
		manifest string
		extra    []string
		want     string
	}{
		{"bad version", strings.Replace(agentSuite, "version: 1", "version: 2", 1), []string{"verifiers/hello"}, "version 2 is not supported"},
		{"no name", strings.Replace(agentSuite, "name: my-agents\n", "", 1), []string{"verifiers/hello"}, "no name"},
		{"no runner", strings.Replace(agentSuite, "runner: agent\n", "", 1), []string{"verifiers/hello"}, "no runner"},
		{"bad runner", strings.Replace(agentSuite, "runner: agent", "runner: batch", 1), nil, "not supported"},
		{"unknown field", agentSuite + "extra: nope\n", []string{"verifiers/hello"}, "extra"},
		{"empty cases", "version: 1\nname: n\nrunner: agent\ncases: []\n", nil, "no cases"},
		{"duplicate id", strings.Replace(agentSuite, "cases:\n", "cases:\n  - id: hello\n    instruction: a\n    verifier: ./verifiers/hello\n    timeout: 1m\n", 1), []string{"verifiers/hello"}, "duplicate case id"},
		{"no verifier", strings.Replace(agentSuite, "    verifier: ./verifiers/hello\n", "", 1), nil, "no verifier"},
		{"no timeout", strings.Replace(agentSuite, "    timeout: 5m\n", "", 1), []string{"verifiers/hello"}, "no positive timeout"},
		{"zero timeout", strings.Replace(agentSuite, "timeout: 5m", "timeout: 0s", 1), nil, "must be positive"},
		{"bad timeout", strings.Replace(agentSuite, "timeout: 5m", "timeout: 5", 1), nil, "not a duration"},
		{"bad case id", strings.Replace(agentSuite, "id: hello", "id: Hello_World", 1), nil, "plain lower-case slug"},
		{"absolute verifier", strings.Replace(agentSuite, "./verifiers/hello", "/etc/passwd", 1), nil, "must be a path relative"},
		{"escaping verifier", strings.Replace(agentSuite, "./verifiers/hello", "../outside", 1), nil, "escapes the suite"},
		{"agent case with loop field", strings.Replace(agentSuite, "    instruction: \"print hello\"\n", "    fixture: ./fixtures/x\n", 1), nil, "loop fields"},
		{"agent case without instruction", strings.Replace(agentSuite, "    instruction: \"print hello\"\n", "", 1), []string{"verifiers/hello"}, "no instruction"},
		{"loop_config on agent suite", strings.Replace(agentSuite, "runner: agent\n", "runner: agent\nloop_config: ./loop.yaml\n", 1), []string{"verifiers/hello"}, "belongs to a loop suite"},
		{"loop suite without loop_config", strings.Replace(loopSuite, "loop_config: ./loop.yaml\n", "", 1), nil, "needs loop_config"},
		{"loop case with agent field", strings.Replace(loopSuite, "    fixture: ./fixtures/app\n", "    instruction: do it\n", 1), nil, "agent fields"},
		{"loop case without tasks", strings.Replace(loopSuite, "    tasks: ./queues/repo-evolution\n", "", 1), nil, "no tasks"},
		{"verifier is the suite dir", strings.Replace(agentSuite, "./verifiers/hello", ".", 1), nil, "suite directory itself"},
		{"verifier inside files", strings.Replace(agentSuite, "    verifier: ./verifiers/hello\n", "    files: ./work\n    verifier: ./work/grader\n", 1), nil, "overlaps its files"},
		{"files inside verifier", strings.Replace(agentSuite, "    verifier: ./verifiers/hello\n", "    files: ./v/sub\n    verifier: ./v\n", 1), nil, "overlaps its files"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeSuite(t, tc.manifest, tc.extra...))
			if err == nil {
				t.Fatalf("manifest was accepted:\n%s", tc.manifest)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err, tc.want)
			}
		})
	}
}

func TestLoadRefusesASymlinkedInput(t *testing.T) {
	dir := t.TempDir()
	// A verifier that is a symlink out of the tree must be refused, not followed.
	outside := filepath.Join(t.TempDir(), "secret-grader")
	if err := os.WriteFile(outside, []byte("answers"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "grader")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	manifest := "version: 1\nname: n\nrunner: agent\ncases:\n  - id: c\n    instruction: a\n    verifier: ./grader\n    timeout: 1m\n"
	if err := os.WriteFile(filepath.Join(dir, "suite.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(filepath.Join(dir, "suite.yaml"))
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("a symlinked verifier was not refused: %v", err)
	}
}

// A symlink at a PARENT component — not just the leaf — must be refused: `verifiers` pointing out of
// the tree with a real file named under it would otherwise slip a grader in through the parent.
func TestLoadRefusesASymlinkedParentComponent(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "sub", "grader"), []byte("answers"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "verifiers")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	manifest := "version: 1\nname: n\nrunner: agent\ncases:\n  - id: c\n    instruction: a\n    verifier: ./verifiers/sub/grader\n    timeout: 1m\n"
	if err := os.WriteFile(filepath.Join(dir, "suite.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(filepath.Join(dir, "suite.yaml"))
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("a symlinked parent component was not refused: %v", err)
	}
}

func TestLoadRefusesASymlinkedManifest(t *testing.T) {
	dir := t.TempDir()
	real := writeSuite(t, agentSuite, "verifiers/hello")
	link := filepath.Join(dir, "link.yaml")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := Load(link); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("a symlinked manifest was not refused: %v", err)
	}
}
