package box

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/runtime"
)

func TestStageBaseImageFilesKeepsSystemPathsReadableUnderPrivateUmask(t *testing.T) {
	// The build runs from an operator's shell; unlike project staging, embedded-file staging
	// used to inherit umask 077 and make COPY system/ / replace /etc with mode 0700.
	previous := syscall.Umask(0o077)
	defer syscall.Umask(previous)
	dir := t.TempDir()
	files := map[string][]byte{
		"Dockerfile":                  []byte("FROM scratch\n"),
		"launchers/codex":             []byte("#!/bin/sh\n"),
		"system/etc/coop/client.conf": []byte("test\n"),
	}
	if err := stageBaseImageFiles(dir, files); err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]os.FileMode{
		".":                           0o700,
		"Dockerfile":                  0o644,
		"launchers":                   0o755,
		"launchers/codex":             0o644,
		"system":                      0o755,
		"system/etc":                  0o755,
		"system/etc/coop":             0o755,
		"system/etc/coop/client.conf": 0o644,
	} {
		info, err := os.Stat(filepath.Join(dir, rel))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %04o, want %04o", rel, got, want)
		}
	}
}

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

func TestStageBuildContextOmitsIgnoredOutputButKeepsAuthoredInputs(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	// Process-wide (not just on the fixture commands) because ignoredBuildPaths — the code under
	// test — shells out to `git ls-files --exclude-standard` with the ambient environment: a
	// developer's core.excludesFile would change which files the staged context is asserted to keep.
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	repo := t.TempDir()
	gitc := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	gitc("init", "-q")
	write(".gitignore", ".build/\n*.generated\ntracked-but-ignored.txt\n")
	write(".agent/Dockerfile", "FROM x\n")
	write("tracked-but-ignored.txt", "required input\n")
	gitc("add", ".gitignore", ".agent/Dockerfile")
	gitc("add", "-f", "tracked-but-ignored.txt")
	gitc("commit", "-qm", "fixture")
	write(".build/firmware/large.bin", "generated\n")
	write("cache.generated", "generated\n")
	write("work-in-progress.txt", "authored input\n")

	ctx, cleanup, err := stageBuildContext(repo)
	if err != nil {
		t.Fatalf("stageBuildContext: %v", err)
	}
	defer cleanup()
	exists := func(rel string) bool {
		_, err := os.Lstat(filepath.Join(ctx, filepath.FromSlash(rel)))
		return err == nil
	}
	for _, keep := range []string{".agent/Dockerfile", "tracked-but-ignored.txt", "work-in-progress.txt"} {
		if !exists(keep) {
			t.Errorf("staged context should keep tracked or intentionally authored input %q", keep)
		}
	}
	for _, omit := range []string{".build", ".build/firmware/large.bin", "cache.generated"} {
		if exists(omit) {
			t.Errorf("staged context must omit ignored output %q", omit)
		}
	}
}

// baseDockerfile is the base image's Dockerfile as this binary renders it for linux/arm64.
func baseDockerfile(t testing.TB) string {
	t.Helper()
	df, err := BaseDockerfile("arm64")
	if err != nil {
		t.Fatal(err)
	}
	return df
}

// entrypointScript is the base image's coop-entry, cut from the heredoc that installs it.
func entrypointScript(t testing.TB) string {
	t.Helper()
	const start = "COPY <<'ENTRY' /usr/local/bin/coop-entry\n"
	const end = "\nENTRY\nRUN chmod +x /usr/local/bin/coop-entry"
	_, entrypoint, ok := strings.Cut(baseDockerfile(t), start)
	if !ok {
		t.Fatal("base Dockerfile has no coop-entry heredoc")
	}
	entrypoint, _, ok = strings.Cut(entrypoint, end)
	if !ok {
		t.Fatal("base Dockerfile coop-entry heredoc is unterminated")
	}
	return entrypoint
}

// coop-entry records the PATH the box started with, overriding whatever an env file carried, so the
// consult and delegate wrappers can hand their arms the box's PATH instead of a lead client's.
func TestEntrypointRecordsTheBoxPath(t *testing.T) {
	entry := filepath.Join(t.TempDir(), "coop-entry")
	if err := os.WriteFile(entry, []byte(entrypointScript(t)), 0o755); err != nil {
		t.Fatal(err)
	}
	// An empty PATH directory keeps the entry's asdf provisioning off this host.
	boxPath := t.TempDir() + ":/opt/coop/bin"
	cmd := exec.Command("/bin/sh", entry, "/bin/sh", "-c", `printf %s "$COOP_BOX_PATH"`)
	cmd.Env = []string{"PATH=" + boxPath, "COOP_BOX_PATH=/from/an/env/file"}
	out, err := cmd.CombinedOutput()
	if err != nil || string(out) != boxPath {
		t.Fatalf("the provider saw COOP_BOX_PATH %q (%v), want the box's own %q", out, err, boxPath)
	}
}

// The base installs the qualified clients from the embedded lock — never a floating package, a
// piped installer or a package fetched at build time — with the template fully resolved.
func TestBaseDockerfileInstallsTheQualifiedClients(t *testing.T) {
	df := baseDockerfile(t)
	for _, floating := range []string{"@latest", "npm install -g", "npx ", "install.sh", "AGENT_PACKAGES"} {
		if strings.Contains(df, floating) {
			t.Errorf("BaseDockerfile installs a client outside the lock (%q)", floating)
		}
	}
	if strings.Contains(df, "%!") {
		t.Errorf("BaseDockerfile template not resolved:\n%s", df)
	}
	// The FROM image is driven by a build arg so an update can float it.
	for _, want := range []string{
		"ARG NODE_IMAGE=node:24-slim", "FROM ${NODE_IMAGE}",
		"COPY package.json package-lock.json global.npmrc /opt/coop/clients/",
		"/usr/local/bin/npm ci --prefix /opt/coop/clients --ignore-scripts",
		"| sha256sum -c -", "COPY launchers/ /opt/coop/bin/", "RUN chmod -R a-w /opt/coop/clients",
		// Every client's updater is off in the image itself, homes mounted or not.
		"COPY system/ /", "ENV DISABLE_UPDATES=1 GROK_DISABLE_AUTOUPDATER=1",
		// The launchers lead PATH, so an asdf shim cannot shadow a qualified client.
		`PATH="/opt/coop/bin:/home/node/.asdf/shims:${PATH}"`,
		// ~/.cache pre-created node-owned so the coop-cache volume isn't root-owned.
		"chown node:node /home/node/.asdf /home/node/.cache",
		// agent search/inspect tools, with fd symlinked from Debian's fdfind; shellcheck is a
		// gate tool (coop's own `make check` lints install.sh) a non-root agent can't apt-get.
		"ripgrep fd-find jq tree shellcheck", `ln -s "$(command -v fdfind)" /usr/local/bin/fd`,
		// Models can commit LFS pointers without host publication credentials.
		"git git-lfs", "git lfs install --system --skip-repo",
		// coop-consult uses a kernel-held lock that must be present in the built image.
		"inotify-tools util-linux", "command -v flock >/dev/null",
		// bare python + pip so an agent reaching for them doesn't self-debug a missing tool.
		"python3 python-is-python3 python3-pip", `ln -s "$(command -v pip3)" /usr/local/bin/pip`,
		// Playwright's Chromium system libs baked in as root (by the locked Playwright) so a
		// browser launches in the box.
		"/usr/local/bin/node /opt/coop/clients/node_modules/playwright/cli.js install-deps chromium",
		// Login shells source /etc/profile (which resets PATH); a profile.d drop-in re-adds the
		// launchers and asdf shims so go/ruby/… pinned in .tool-versions resolve there too.
		`printf 'export PATH="/opt/coop/bin:/home/node/.asdf/shims:$PATH"\n' > /etc/profile.d/asdf.sh`,
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

// Coop's own gate analyzers, and the jv validator built beside them, stay out of every user's box. A
// Go build stage once compiled Staticcheck, govulncheck and jv into both images: a golang pull and
// three compiles per build, 41 MB per image, and a Go image pin of the box's own in every host's
// network setup. A project that needs them installs them in its own .agent/Dockerfile, as this
// repository's does for its gate.
func TestBoxImagesCarryNoGateAnalyzers(t *testing.T) {
	platform := agents.ClientPlatform{OS: "linux", Architecture: "arm64", Libc: "glibc"}
	base, err := baseImageDefinition(platform)
	if err != nil {
		t.Fatal(err)
	}
	spec, _, locked, err := lockedImageDefinition(platform)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := spec.Args["GO_IMAGE"]; ok {
		t.Error("the filtered client image still takes a Go image")
	}
	for name, files := range map[string]map[string][]byte{"base": base, "filtered client": locked.Files} {
		df := string(files["Dockerfile"])
		stages := 0
		for line := range strings.Lines(df) {
			if strings.HasPrefix(line, "FROM ") {
				stages++
			}
		}
		if stages != 1 {
			t.Errorf("the %s image builds in %d stages, want one", name, stages)
		}
		for _, tool := range []string{"golang", "go install", "staticcheck", "govulncheck", "/bin/jv"} {
			if strings.Contains(strings.ToLower(df), tool) {
				t.Errorf("the %s image still mentions %q", name, tool)
			}
		}
		for file := range files {
			if strings.HasPrefix(file, "staticcheck/") {
				t.Errorf("the %s image still stages %s", name, file)
			}
		}
	}
}

// An agent can author the box Dockerfile (it defines the next box); coop flags an untracked one
// before building. Tracked → quiet; non-git → no signal.
func TestBoxDockerfileUntracked(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	// Process-wide, like the fixture above: fileUntracked — the code under test — runs
	// `git ls-files` with the ambient environment, and the fixture commits below must not meet a
	// developer's commit.gpgsign or core.hooksPath.
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
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
// --no-cache WITHOUT --pull (the base is a local tag); a standalone one gets --pull on fresh.
func TestProjectBuildArgs(t *testing.T) {
	// Inherits the base: build-arg present, no --pull even on fresh.
	got := projectBuildArgs("/ctx", ".agent/Dockerfile", "coop-app", "coop-box", true, true)
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "--build-arg COOP_BASE_IMAGE=coop-box") {
		t.Errorf("inheriting build must pass the base build-arg: %v", got)
	}
	id := boxAgentIdentity()
	for _, want := range []string{"--build-arg COOP_BOX_UID=" + fmt.Sprint(id.uid), "--build-arg COOP_BOX_GID=" + fmt.Sprint(id.gid)} {
		if !strings.Contains(joined, want) {
			t.Errorf("inheriting build missing %s: %v", want, got)
		}
	}
	if strings.Contains(joined, "--pull") {
		t.Errorf("a local base can't be pulled — no --pull on an inheriting build: %v", got)
	}
	if !strings.Contains(joined, "--no-cache") {
		t.Errorf("--fresh must still add --no-cache: %v", got)
	}
	// Standalone (external FROM): no base-image arg, but still gets the host identity.
	got = projectBuildArgs("/ctx", ".agent/Dockerfile", "coop-app", "coop-box", false, true)
	joined = strings.Join(got, " ")
	if strings.Contains(joined, "COOP_BASE_IMAGE") {
		t.Errorf("a standalone build must not pass the base arg: %v", got)
	}
	for _, want := range []string{"--build-arg COOP_BOX_UID=" + fmt.Sprint(id.uid), "--build-arg COOP_BOX_GID=" + fmt.Sprint(id.gid)} {
		if !strings.Contains(joined, want) {
			t.Errorf("standalone build missing %s: %v", want, got)
		}
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

// One manifest in every box: the base and the filtered client image render the very same client
// installation from the same closure — lock, verified native artifacts, launchers, update controls
// — for each platform, and the base's context carries every file that installation copies in.
func TestBothImagesInstallTheSameClients(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		platform := agents.ClientPlatform{OS: "linux", Architecture: arch, Libc: "glibc"}
		closure, err := agents.LockedClientClosure(platform)
		if err != nil {
			t.Fatal(err)
		}
		clients := lockedClientParts(closure)
		base, err := baseImageDefinition(platform)
		if err != nil {
			t.Fatal(err)
		}
		_, _, locked, err := lockedImageDefinition(platform)
		if err != nil {
			t.Fatal(err)
		}
		for name, files := range map[string]map[string][]byte{"base": base, "filtered": locked.Files} {
			df := string(files["Dockerfile"])
			for _, part := range []string{clients.files, clients.install, clients.browserDeps, clients.scripts} {
				if !strings.Contains(df, part) {
					t.Errorf("%s %s image does not install the shared clients:\n%s", arch, name, part)
				}
			}
			for file := range closure.Files {
				if files[file] == nil {
					t.Errorf("%s %s image context lacks %s", arch, name, file)
				}
			}
		}
	}
}

// asdf comes from a release URL into both images, so each build checks the tarball against the
// digest pinned for its own architecture before unpacking it. Pinning one architecture is not enough.
func TestImagesVerifyAsdfBeforeUnpackingIt(t *testing.T) {
	if asdfSHA256["amd64"] == asdfSHA256["arm64"] {
		t.Fatal("both architectures pin the same asdf digest")
	}
	for arch, other := range map[string]string{"amd64": "arm64", "arm64": "amd64"} {
		digest := asdfSHA256[arch]
		if raw, err := hex.DecodeString(digest); err != nil || len(raw) != sha256.Size {
			t.Fatalf("%s asdf digest %q is not a SHA-256", arch, digest)
		}
		step := asdfInstall(arch)
		url := "https://github.com/asdf-vm/asdf/releases/download/v" + asdfVersion + "/asdf-v" + asdfVersion + "-linux-" + arch + ".tar.gz"
		download := strings.Index(step, "--output /tmp/coop-asdf.tar.gz '"+url+"'")
		verify := strings.Index(step, "\n && printf '%s  %s\\n' '"+digest+"' /tmp/coop-asdf.tar.gz | sha256sum -c - \\\n")
		unpack := strings.Index(step, "\n && tar -C /usr/local/bin -xzf /tmp/coop-asdf.tar.gz asdf")
		if download < 0 || verify < download || unpack < verify {
			t.Errorf("%s asdf must be downloaded, then checked, then unpacked:\n%s", arch, step)
		}
		platform := agents.ClientPlatform{OS: "linux", Architecture: arch, Libc: "glibc"}
		base, err := baseImageDefinition(platform)
		if err != nil {
			t.Fatal(err)
		}
		_, _, locked, err := lockedImageDefinition(platform)
		if err != nil {
			t.Fatal(err)
		}
		for name, files := range map[string]map[string][]byte{"base": base, "filtered": locked.Files} {
			df := string(files["Dockerfile"])
			if !strings.Contains(df, "\n && "+step+" \\\n") {
				t.Errorf("%s %s image does not check asdf before unpacking it", arch, name)
			}
			if strings.Contains(df, asdfSHA256[other]) || strings.Contains(df, "-linux-"+other+".tar.gz") {
				t.Errorf("%s %s image fetches or checks the %s asdf", arch, name, other)
			}
			if strings.Contains(df, "| tar") {
				t.Errorf("%s %s image unpacks a download straight from the network", arch, name)
			}
		}
	}
}

// A download that does not match its pinned digest stops the build before tar runs.
func TestAsdfInstallRefusesATamperedDownload(t *testing.T) {
	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skip("sha256sum not available")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, script := range map[string]string{
		// The network answers with bytes that are not the release.
		"curl": "#!/bin/sh\nwhile [ $# -gt 0 ]; do [ \"$1\" = --output ] && { printf tampered > \"$2\"; exit 0; }; shift; done\nexit 2\n",
		"tar":  "#!/bin/sh\n: > \"$UNPACKED\"\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, arch := range []string{"amd64", "arm64"} {
		step := asdfInstall(arch)
		if !strings.Contains(step, "--output /tmp/coop-asdf.tar.gz ") {
			t.Fatalf("%s asdf step moved its download; point this test at the new path:\n%s", arch, step)
		}
		unpacked := filepath.Join(dir, arch+".unpacked")
		cmd := exec.Command("/bin/sh", "-c", strings.ReplaceAll(step, "/tmp/coop-asdf.tar.gz", filepath.Join(dir, arch+".tar.gz")))
		cmd.Env = []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "UNPACKED=" + unpacked}
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "FAILED") {
			t.Errorf("%s: the checksum did not reject a tampered download (%v):\n%s", arch, err, out)
		}
		if _, err := os.Stat(unpacked); err == nil {
			t.Errorf("%s: tar ran on a download that failed its checksum", arch)
		}
	}
}

// The base image bakes socat and the coop-entry sidecar forwarder (raw-TCP loopback) so a box can
// reach an expose'd sidecar at the same localhost:<hostport> URL the host uses (OIDC issuer match).
func TestBaseDockerfileHasSidecarForwarder(t *testing.T) {
	df := baseDockerfile(t)
	for _, want := range []string{
		"util-linux socat",
		`if [ -n "$COOP_FORWARD" ]`,
		"TCP-LISTEN:$hp,bind=127.0.0.1,fork,reuseaddr",
	} {
		if !strings.Contains(df, want) {
			t.Errorf("base Dockerfile missing sidecar-forwarder bit %q", want)
		}
	}
}

func TestBaseDockerfileSupervisesDetachedDescendantsPortably(t *testing.T) {
	df := baseDockerfile(t)
	for _, want := range []string{
		"echo \"${20}\"",
		"current=${20}",
		"quiescence_rescan=",
		"Rescan once before treating a clean provider exit as",
	} {
		if !strings.Contains(df, want) {
			t.Errorf("base Dockerfile missing descendant-supervision bit %q", want)
		}
	}
	if strings.Contains(df, "echo \"$20\"") || strings.Contains(df, "current=$20") {
		t.Errorf("base Dockerfile must brace positional field 20:\n%s", df)
	}
}

// BuildWith must send the runtime's build output to the writer it was GIVEN, never os.Stdout.
// `coop acp` speaks JSON-RPC over os.Stdout and reads the editor's requests from os.Stdin, so a
// build that grabbed either would corrupt the wire and swallow the editor's initialize — the
// reason the ACP path passes an empty reader and os.Stderr.
func TestBuildWithHonorsCallerStreams(t *testing.T) {
	repo := t.TempDir() // no .agent/Dockerfile → the shared-base path, which is what `coop acp` hits
	shim := filepath.Join(t.TempDir(), "rt")
	// Name the daemon's platform, echo a marker on stdout, then print the Dockerfile the build was
	// handed and whatever arrived on stdin, so the test can prove where each landed.
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  info) echo linux/aarch64; exit 0 ;;\n" +
		"  build) echo BUILD-STDOUT; while [ $# -gt 1 ]; do [ \"$1\" = -f ] && cat \"$2\"; shift; done; cat; exit 0 ;;\n" +
		"esac\n" +
		"exit 0\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	rt := runtime.Runtime{Name: shim}
	cfg := &config.Config{BaseImage: "coop-box", ConfigDir: t.TempDir(), BoxHome: t.TempDir()}

	var out strings.Builder
	if err := BuildWith(rt, cfg, repo, false, "vTest", strings.NewReader("CALLER-STDIN"), &out); err != nil {
		t.Fatalf("BuildWith = %v, want nil", err)
	}
	if got := out.String(); !strings.Contains(got, "BUILD-STDOUT") {
		t.Errorf("build stdout did not reach the caller's writer (an ACP build would have gone to the JSON-RPC wire):\n%s", got)
	}
	// The base builds from its staged context for the daemon's platform; it reads nothing from
	// stdin — in `coop acp` that is the editor's JSON-RPC wire.
	if got := out.String(); !strings.Contains(got, "FROM ${NODE_IMAGE}") || !strings.Contains(got, "COPY launchers/ /opt/coop/bin/") {
		t.Errorf("the staged base Dockerfile did not reach the runtime:\n%s", got)
	}
	if got := out.String(); strings.Contains(got, "CALLER-STDIN") {
		t.Errorf("the base build consumed the caller's stdin:\n%s", got)
	}
}

func TestBuildWithRejectsInvalidProjectBeforeRuntime(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, project.File), []byte("box: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	recorder := filepath.Join(t.TempDir(), "runtime-args")
	err := BuildWith(recorderRuntime(t, recorder), &config.Config{BaseImage: "coop-box"}, repo, false, "vTest", strings.NewReader(""), io.Discard)
	if err == nil || !strings.Contains(err.Error(), project.File) {
		t.Fatalf("BuildWith = %v, want policy error", err)
	}
	if _, statErr := os.Stat(recorder); !os.IsNotExist(statErr) {
		t.Fatalf("runtime was invoked before policy validation: %v", statErr)
	}
}

// A box serves one project, so its pins must hold outside the checkout too: emisar's trusted gate
// failed in the base image when a test's subprocess in a temp dir got "No version is set for
// command mix". The entry script makes the project's .tool-versions the home default, keeps a
// home-level file an image already ships, and writes nothing without project pins.
func TestProvisioningMakesTheProjectPinsTheBoxDefault(t *testing.T) {
	bin := t.TempDir()
	for name, body := range map[string]string{
		"asdf": "#!/bin/sh\nexit 0\n",       // every provisioning step succeeds and does nothing
		"node": "#!/bin/sh\necho v24.0.0\n", // a working node, so the nodejs repair stays out of it
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ name, project, home, want string }{
		{"project pins become the default", "erlang 29.0.6\nelixir 1.20.4-otp-29\n", "", "erlang 29.0.6\nelixir 1.20.4-otp-29\n"},
		{"an image's own home pins stay", "elixir 1.20.4-otp-29\n", "elixir 1.19.5-otp-28\n", "elixir 1.19.5-otp-28\n"},
		{"no project pins writes nothing", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, project := t.TempDir(), t.TempDir()
			for path, content := range map[string]string{filepath.Join(project, ".tool-versions"): tc.project, filepath.Join(home, ".tool-versions"): tc.home} {
				if content == "" {
					continue
				}
				if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command("sh", "-c", baseProvisioningScript)
			cmd.Dir = project
			cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + home, "COOP_QUIET=1"}
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("provisioning script: %v\n%s", err, out)
			}
			got, err := os.ReadFile(filepath.Join(home, ".tool-versions"))
			if tc.want == "" {
				if !os.IsNotExist(err) {
					t.Fatalf("home pins without project pins: %q, %v", got, err)
				}
				return
			}
			if err != nil || string(got) != tc.want {
				t.Fatalf("home pins = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}
