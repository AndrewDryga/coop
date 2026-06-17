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
	// The npm install and the FROM image are driven by build args so a build can pin
	// them; the packages live in the AGENT_PACKAGES default.
	for _, want := range []string{
		"ARG NODE_IMAGE=node:24", "FROM ${NODE_IMAGE}",
		`ARG AGENT_PACKAGES="@`, "npm install -g ${AGENT_PACKAGES}",
		// ~/.cache pre-created node-owned so the coop-cache volume isn't root-owned.
		"chown node:node /home/node/.asdf /home/node/.cache",
	} {
		if !strings.Contains(df, want) {
			t.Errorf("BaseDockerfile missing %q", want)
		}
	}
}
