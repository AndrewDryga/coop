package box

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// stageBuildContext must OMIT shadowed secrets (and .git) from the Docker build context — so a
// .agent/Dockerfile COPY can't bake them into an image layer — while keeping every non-secret file.
func TestStageBuildContext(t *testing.T) {
	repo := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".agent/Dockerfile", "FROM x\n")
	write("main.go", "package main\n")
	write("config/app.yaml", "ok\n")
	write(".env", "SECRET=1\n")          // shadowed
	write("id_rsa", "KEY\n")             // shadowed (hard key pattern)
	write(".aws/credentials", "creds\n") // shadowed dir
	write(".git/config", "[core]\n")     // .git is never part of a build context

	ctx, cleanup, err := stageBuildContext(repo)
	if err != nil {
		t.Fatalf("stageBuildContext: %v", err)
	}
	defer cleanup()

	exists := func(rel string) bool { _, e := os.Lstat(filepath.Join(ctx, rel)); return e == nil }
	for _, keep := range []string{".agent/Dockerfile", "main.go", "config/app.yaml", "config"} {
		if !exists(keep) {
			t.Errorf("staged context should keep non-secret %q", keep)
		}
	}
	for _, omit := range []string{".env", "id_rsa", ".aws", ".aws/credentials", ".git", ".git/config"} {
		if exists(omit) {
			t.Errorf("staged context must OMIT %q (secret or .git)", omit)
		}
	}
}

// The base Dockerfile installs every agent's npm packages, assembled from the registry
// (not a hard-coded list), with the template fully resolved.
func TestBaseDockerfileInstallsAgentPackages(t *testing.T) {
	df := BaseDockerfile()
	for _, pkg := range []string{
		"@anthropic-ai/claude-code@latest", "@agentclientprotocol/claude-agent-acp@latest",
		"@openai/codex@latest", "@agentclientprotocol/codex-acp@latest", "@google/gemini-cli@latest",
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
		"ARG NODE_IMAGE=node:24-slim", "FROM ${NODE_IMAGE}",
		"ARG GO_IMAGE=golang:1.26.5-bookworm", "FROM ${GO_IMAGE} AS staticcheck-builder",
		"ARG STATICCHECK_VERSION=v0.7.0",
		"go install honnef.co/go/tools/cmd/staticcheck@${STATICCHECK_VERSION}",
		"COPY --from=staticcheck-builder /out/staticcheck /usr/local/bin/staticcheck",
		`ARG AGENT_PACKAGES="@`, "npm install -g ${AGENT_PACKAGES}",
		// ~/.cache pre-created node-owned so the coop-cache volume isn't root-owned.
		"chown node:node /home/node/.asdf /home/node/.cache",
		// agent search/inspect tools, with fd symlinked from Debian's fdfind.
		"ripgrep fd-find jq tree", `ln -s "$(command -v fdfind)" /usr/local/bin/fd`,
		// coop-consult uses a kernel-held lock that must be present in the built image.
		"inotify-tools util-linux", "command -v flock >/dev/null",
		// bare python + pip so an agent reaching for them doesn't self-debug a missing tool.
		"python3 python-is-python3 python3-pip", `ln -s "$(command -v pip3)" /usr/local/bin/pip`,
		// Playwright's Chromium system libs baked in as root so a browser launches in the box.
		"npx -y playwright install-deps chromium",
		// Login shells source /etc/profile (which resets PATH); a profile.d drop-in re-adds the
		// asdf shims so go/ruby/… pinned in .tool-versions resolve there too, not just non-login.
		`printf 'export PATH="/home/node/.asdf/shims:$PATH"\n' > /etc/profile.d/asdf.sh`,
		// The entrypoint repairs a bare `node` when an orphaned asdf nodejs shim (from a
		// prior repo, persisted in the ~/.asdf volume) shadows the image node in a repo that
		// doesn't pin nodejs — so the Node agent CLIs always have a working interpreter.
		"COOP_NO_ASDF skips provisioning, not this repair.",
		"if ! node --version >/dev/null 2>&1; then",
		`asdf set --home nodejs "$v"`,
	} {
		if !strings.Contains(df, want) {
			t.Errorf("BaseDockerfile missing %q", want)
		}
	}
	skipProvisioning := strings.Index(df, `if [ -z "$COOP_NO_ASDF" ]; then`)
	repairNode := strings.Index(df, "if ! node --version >/dev/null 2>&1; then")
	if skipProvisioning < 0 || repairNode < 0 || repairNode < skipProvisioning {
		t.Errorf("node repair should run after the COOP_NO_ASDF provisioning branch")
	}
}

// An agent can author the box Dockerfile (it defines the next box); coop flags an untracked one
// before building. Tracked → quiet; non-git → no signal.
func TestBoxDockerfileUntracked(t *testing.T) {
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
	const df = ".agent/Dockerfile"
	write := func(dir string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, ".agent"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(df)), []byte("FROM x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	repo := t.TempDir()
	gitc(repo, "init", "-q")
	write(repo)
	// Present but untracked → flagged.
	if !fileUntracked(repo, df) {
		t.Error("an untracked box Dockerfile should be flagged")
	}
	// Tracked → not flagged.
	gitc(repo, "add", df)
	gitc(repo, "commit", "-qm", "add")
	if fileUntracked(repo, df) {
		t.Error("a committed box Dockerfile should not be flagged")
	}
	// Non-git dir → no signal (false), even with the file present.
	nogit := t.TempDir()
	write(nogit)
	if fileUntracked(nogit, df) {
		t.Error("a non-git repo should not be flagged (untracked isn't meaningful there)")
	}
}

// projectBuildArgs: a base-inheriting Dockerfile gets the COOP_BASE_IMAGE build-arg and, on --fresh,
// --no-cache WITHOUT --pull (the base is a local tag); a standalone one gets --pull on fresh and no arg.
func TestProjectBuildArgs(t *testing.T) {
	// Inherits the base: build-arg present, no --pull even on fresh.
	got := projectBuildArgs("/ctx", ".agent/Dockerfile", "coop-app", "coop-box", true, true)
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "--build-arg COOP_BASE_IMAGE=coop-box") {
		t.Errorf("inheriting build must pass the base build-arg: %v", got)
	}
	if strings.Contains(joined, "--pull") {
		t.Errorf("a local base can't be pulled — no --pull on an inheriting build: %v", got)
	}
	if !strings.Contains(joined, "--no-cache") {
		t.Errorf("--fresh must still add --no-cache: %v", got)
	}
	// Standalone (external FROM): no build-arg, --pull on fresh.
	got = projectBuildArgs("/ctx", ".agent/Dockerfile", "coop-app", "coop-box", false, true)
	joined = strings.Join(got, " ")
	if strings.Contains(joined, "COOP_BASE_IMAGE") {
		t.Errorf("a standalone build must not pass the base arg: %v", got)
	}
	if !strings.Contains(joined, "--pull") || !strings.Contains(joined, "--no-cache") {
		t.Errorf("a standalone --fresh build should --pull --no-cache: %v", got)
	}
	// Non-fresh standalone: neither --pull nor --no-cache; ends with -t/-f/ctx.
	got = projectBuildArgs("/ctx", ".agent/Dockerfile", "coop-app", "coop-box", false, false)
	joined = strings.Join(got, " ")
	if strings.Contains(joined, "--pull") || strings.Contains(joined, "--no-cache") {
		t.Errorf("a plain build takes no cache flags: %v", got)
	}
	if !strings.HasSuffix(joined, "-t coop-app -f /ctx/.agent/Dockerfile /ctx") {
		t.Errorf("build must target img + resolved -f path + ctx: %v", got)
	}
}

// TestBaseDockerfileInstallLayer: grok installs via a script (curl … | bash), so the
// script-install layer carries its RUN line while the npm-only agents (claude/codex/gemini)
// contribute nothing — proving Agent.InstallScript becomes a root RUN line before USER node, the
// image seam for a non-npm agent, without editing image.go.
func TestBaseDockerfileInstallLayer(t *testing.T) {
	got := installLayer()
	if !strings.HasPrefix(got, "RUN ") || !strings.Contains(got, "curl -fsSL https://x.ai/cli/install.sh") {
		t.Errorf("grok's script install must land as a root RUN line, got:\n%s", got)
	}
	// The template resolves (both %s filled) and embeds the install layer.
	df := BaseDockerfile()
	if strings.Contains(df, "%s") || strings.Contains(df, "%!") {
		t.Errorf("install-layer %%s left unresolved:\n%s", df)
	}
	// The script-install RUN lands as root, before USER node.
	run := strings.Index(df, "RUN curl -fsSL https://x.ai/cli/install.sh")
	user := strings.LastIndex(df, "USER node")
	if run < 0 || user < 0 || run > user {
		t.Errorf("a script-install RUN must precede USER node (run@%d user@%d)", run, user)
	}
}
