//go:build providere2e

package cli

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/AndrewDryga/coop/internal/mcp"
	"github.com/AndrewDryga/coop/internal/tasks"
)

// An agent session in a project with a task queue gets the task channel a loop work box gets — the
// run-private volume and helper around its box, and the coop-tasks server in its client's MCP
// config — so a terminal or editor agent changes task state without the coop CLI. A project with no
// queue keeps the plain direct shape (TestProviderScriptedDirectMatrix).
func TestProviderScriptedSessionGetsTaskTools(t *testing.T) {
	suite := newDirectProcessSuite(t)
	if err := tasks.ScaffoldStateDirs(filepath.Join(suite.layout.Repo, ".agent", "tasks")); err != nil {
		t.Fatal(err)
	}
	for _, provider := range suite.providers {
		t.Run(provider, func(t *testing.T) {
			result, trace := suite.run(t, []string{provider}, processScenario(provider, nil, 0, ""))
			assertDirectSuccess(t, result, provider, "fixture-ok-"+provider)
			contract := directProviderContracts[provider]
			model, effort := contract.models[0], contract.efforts[0]
			assertDirectRunContract(t, suite, trace, provider, "personal", taskToolsArgv(provider, directExpectedArgv(contract, model, effort, nil)), model, effort, sessionTaskBox)
			run := oneProcessEvent(t, trace, "runtime", "run")
			carried := false
			for _, mount := range run.Run.Mounts {
				carried = carried || slices.Contains(mount.MCPServers, mcp.TaskToolsServer)
			}
			if !carried {
				t.Fatalf("%s's session box was never handed the %s server:\n%#v", provider, mcp.TaskToolsServer, run.Run.Mounts)
			}
		})
	}
}
