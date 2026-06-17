package box

import (
	"strings"
	"testing"
)

// The base Dockerfile installs every agent's npm packages, assembled from the registry
// (not a hard-coded list), with the template fully resolved.
func TestBaseDockerfileInstallsAgentPackages(t *testing.T) {
	df := BaseDockerfile()
	for _, pkg := range []string{
		"@anthropic-ai/claude-code", "@agentclientprotocol/claude-agent-acp",
		"@openai/codex", "@zed-industries/codex-acp", "@google/gemini-cli",
	} {
		if !strings.Contains(df, pkg) {
			t.Errorf("BaseDockerfile missing package %q", pkg)
		}
	}
	if strings.Contains(df, "%s") || strings.Contains(df, "%!") {
		t.Errorf("BaseDockerfile template not resolved:\n%s", df)
	}
	if !strings.Contains(df, "npm install -g @") {
		t.Error("npm install line not assembled")
	}
}
