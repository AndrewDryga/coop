package box

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

// A filtered run points composition at its execution's private artifact
// directory, and its mount custody accepts ONLY descendants of that directory —
// so every generated-artifact producer has to honor the parent, not just the
// one that is easiest to wrap. Ordinary runs pass "" and keep the system temp
// dir, including their existing cleanup.
func TestRunComposesUnderExplicitParentWithoutChangingOrdinaryCleanup(t *testing.T) {
	// Canonical, like a filtered run's artifact directory: its custody check requires every mount
	// source to BE its own resolved path, so producers resolve the parent before allocating.
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	artifacts := defaultCompositionArtifactOps()
	artifacts.parent = parent
	originalWrite := artifacts.writeFile
	var generated []string
	artifacts.writeFile = func(root, content string) (string, error) {
		if root != parent {
			t.Fatal("run lost explicit composition parent")
		}
		path, err := originalWrite(root, content)
		if err == nil {
			generated = append(generated, path)
		}
		return path, err
	}
	hooks := false
	originalHooks := artifacts.gitHookDir
	artifacts.gitHookDir = func(root string) (string, error) {
		if root != parent {
			t.Fatal("Git hook composition lost its explicit parent")
		}
		hooks = true
		path, err := originalHooks(root)
		if err == nil {
			generated = append(generated, path)
		}
		return path, err
	}
	originalAgents := artifacts.assembleAgentsDir
	artifacts.assembleAgentsDir = func(root string, files []genFile) (string, error) {
		if root != parent {
			t.Fatal("agents-directory composition lost its explicit parent")
		}
		path, err := originalAgents(root, files)
		if err == nil {
			generated = append(generated, path)
		}
		return path, err
	}
	repo := t.TempDir()
	writeCopyFixture(t, filepath.Join(repo, ".agent", "project.yaml"), "box:\n  env:\n    FIXTURE_ENV: synthetic\n")
	// Shared skills and a home fallback: the two producers that USED to escape the parent. The
	// fixture had neither, which is how every filtered launch in a repository with shared skills
	// was refused ("generated network workload mount is not an owned descendant") without this
	// invariant test noticing.
	writeCopyFixture(t, filepath.Join(repo, ".agent", "skills", "probe", "SKILL.md"), "---\nname: probe\n---\n")
	writeCopyFixture(t, filepath.Join(repo, ".agent", "claude", "settings.json"), "{}\n")
	cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "none"}
	recorder := filepath.Join(t.TempDir(), "runtime-args")
	code, err := runWithCompositionArtifacts(cfg, recorderRuntime(t, recorder), RunSpec{
		Image: "fixture", Repo: repo, Cmd: []string{"true"}, Agent: "claude", Homes: true, Batch: true, Quiet: true,
	}, artifacts)
	// The agents directory is only assembled when this run generates agent files;
	// its wrapper still holds it to the parent on the runs that do.
	if err != nil || code != 0 || len(generated) < 3 || !hooks {
		t.Fatalf("run composition: code=%d generated=%d hooks=%v err=%v", code, len(generated), hooks, err)
	}
	for _, path := range generated {
		if filepath.Dir(path) != parent {
			t.Fatalf("generated artifact escaped its parent: %s", path)
		}
	}
	// The copies are not produced through the wrappers above, so check the mounts the runtime was
	// actually handed: every generated mount into the agent's home has to come from the parent.
	recorded, err := os.ReadFile(recorder)
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(recorded))
	seen := map[string]bool{}
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] != "-v" {
			continue
		}
		source, target, _ := strings.Cut(fields[i+1], ":")
		target, _, _ = strings.Cut(target, ":")
		switch target {
		case "/home/node/.claude/skills", "/home/node/.claude/settings.json":
			seen[target] = true
			if !strings.HasPrefix(source, parent+string(filepath.Separator)) {
				t.Errorf("%s mounted from %s, outside the composition parent %s", target, source, parent)
			}
		}
	}
	for _, target := range []string{"/home/node/.claude/skills", "/home/node/.claude/settings.json"} {
		if !seen[target] {
			t.Errorf("the run never mounted %s, so this test proved nothing about it:\n%s", target, recorded)
		}
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatal("ordinary run changed its generated-artifact cleanup", err)
	}
}
