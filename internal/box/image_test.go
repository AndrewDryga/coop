package box

import (
	"os"
	"os/exec"
	"path/filepath"
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
		// agent search/inspect tools, with fd symlinked from Debian's fdfind.
		"ripgrep fd-find jq tree", `ln -s "$(command -v fdfind)" /usr/local/bin/fd`,
		// bare python + pip so an agent reaching for them doesn't self-debug a missing tool.
		"python3 python-is-python3 python3-pip", `ln -s "$(command -v pip3)" /usr/local/bin/pip`,
		// Playwright's Chromium system libs baked in as root so a browser launches in the box.
		"npx -y playwright install-deps chromium",
	} {
		if !strings.Contains(df, want) {
			t.Errorf("BaseDockerfile missing %q", want)
		}
	}
}

// An agent can author a Dockerfile.agent (it defines the next box); coop flags an untracked one
// before building. Tracked → quiet; non-git → no signal.
func TestDockerfileAgentUntracked(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	gitc := func(dir string, args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	repo := t.TempDir()
	gitc(repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "Dockerfile.agent"), []byte("FROM x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Present but untracked → flagged.
	if !dockerfileAgentUntracked(repo) {
		t.Error("an untracked Dockerfile.agent should be flagged")
	}
	// Tracked → not flagged.
	gitc(repo, "add", "Dockerfile.agent")
	gitc(repo, "commit", "-qm", "add")
	if dockerfileAgentUntracked(repo) {
		t.Error("a committed Dockerfile.agent should not be flagged")
	}
	// Non-git dir → no signal (false), even with the file present.
	nogit := t.TempDir()
	if err := os.WriteFile(filepath.Join(nogit, "Dockerfile.agent"), []byte("FROM x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if dockerfileAgentUntracked(nogit) {
		t.Error("a non-git repo should not be flagged (untracked isn't meaningful there)")
	}
}
