package forkctl

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/ui"
)

// configuredSettings points every box input project.yaml takes outside the names Coop knows.
const configuredSettings = "box:\n  dockerfile: deploy/box.Dockerfile\n  compose: ops/stack.yml\nreview:\n  compose: .agent/review-compose.yml\n"

var configuredInputs = []string{"deploy/box.Dockerfile", "ops/stack.yml", ".agent/review-compose.yml"}

func configuredRepo(t *testing.T) string {
	t.Helper()
	repo := initRepo(t)
	writeTaskFile(t, filepath.Join(repo, ".agent", "project.yaml"), configuredSettings)
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "configured box inputs")
	return repo
}

// A fork's change to a Dockerfile or compose file the parent's project.yaml names runs on your
// machine as .agent/compose.yml does: the merge scan names it, so the merge waits for --force.
func TestMergeScanFlagsTheBoxInputsTheParentConfigures(t *testing.T) {
	for _, path := range configuredInputs {
		t.Run(path, func(t *testing.T) {
			repo := configuredRepo(t)
			ws, err := forkspace.Setup(repo, "surface")
			if err != nil {
				t.Fatal(err)
			}
			writeTaskFile(t, filepath.Join(ws, path), "changed by the fork\n")
			git(t, ws, "add", "-A")
			git(t, ws, "commit", "-qm", "change "+path)
			if err := gitFetchInto(repo, ws, "surface"); err != nil {
				t.Fatal(err)
			}
			warns := strings.Join(mustPolicyScan(t, repo, "review/surface"), "\n")
			if !strings.Contains(warns, path+" — ") {
				t.Errorf("the merge scan does not name %s: %q", path, warns)
			}
		})
	}
}

// The settings come from the parent: a fork that adds its own project.yaml naming a file does not
// make that file a surface (its project.yaml change is flagged on its own).
func TestMergeScanReadsConfiguredInputsFromTheParent(t *testing.T) {
	repo := initRepo(t)
	ws, err := forkspace.Setup(repo, "surface")
	if err != nil {
		t.Fatal(err)
	}
	writeTaskFile(t, filepath.Join(ws, ".agent", "project.yaml"), configuredSettings)
	writeTaskFile(t, filepath.Join(ws, "ops", "stack.yml"), "services: {}\n")
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", "fork settings")
	if err := gitFetchInto(repo, ws, "surface"); err != nil {
		t.Fatal(err)
	}
	warns := strings.Join(mustPolicyScan(t, repo, "review/surface"), "\n")
	if !strings.Contains(warns, ".agent/project.yaml — ") || strings.Contains(warns, "ops/stack.yml") {
		t.Errorf("merge scan = %q; want the fork's project.yaml flagged and its named file judged by the parent's settings", warns)
	}
}

// An isolated fork's commit that changes a configured box input is refused outright.
func TestIsolatedMergeRefusesTheBoxInputsTheParentConfigures(t *testing.T) {
	for _, path := range configuredInputs {
		t.Run(path, func(t *testing.T) {
			repo, ws, identity, c := isolatedTestForkFromRepo(t, configuredRepo(t))
			isolatedTestCommit(t, ws, path, "changed by the fork\n")
			_, err := c.mergeOne(repo, "", identity.Name, false)
			var block *ui.UsageError
			if !errors.As(err, &block) {
				t.Fatalf("merge = %v, want a refusal", err)
			}
			if plain := block.Render(ui.Palette{}); !strings.Contains(plain, `The fork changes "`+path+`", which Coop will not merge`) {
				t.Errorf("the refusal does not name %s:\n%s", path, plain)
			}
		})
	}
}
