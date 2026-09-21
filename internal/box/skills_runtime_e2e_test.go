//go:build boxruntimee2e

package box

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// Every skills-capable client finds a skill at the path Coop's shared `.agent/skills` projection
// mounts for it — proven by that client's OWN report of what it loaded, inside the locked client
// image with networking off. The mount plan is already pinned elsewhere; this is the other half,
// that the client actually reads the directory the plan targets. A client that quietly moved its
// skills root would keep passing the mount test and silently load nothing.
//
// Two guards keep a row from passing for the wrong reason. The probe runs from a working directory
// that is NOT the home: every one of these clients ALSO discovers <cwd>/.<agent>/skills as a project
// skill and reports it with the same absolute path, so a probe run from $HOME would keep passing for
// a client that dropped the user-level root entirely — the regression this test exists to catch. And
// a decoy skill sits at ~/.<agent>/notskills, a path Coop does not mount, which no client may report.
//
// Needs the locked client image, which `coop net setup` builds; run it with `make skills-e2e`.
func TestRuntimeSharedSkillsAreDiscoveredByEveryPinnedClient(t *testing.T) {
	rt, err := runtime.Detect(os.Getenv("COOP_RUNTIME"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	docker, err := runtime.InspectDocker(ctx, rt)
	if err != nil {
		t.Fatal(err)
	}
	defer docker.Close()
	binding := networkRuntimeBinding(docker.Info(), docker.Endpoint())
	definition, _, _, err := lockedImageDefinition(agents.ClientPlatform{OS: binding.OS, Architecture: binding.Architecture, Libc: "glibc"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := docker.Image(ctx, definition.Tag); err != nil {
		t.Fatalf("the locked client image %s is not on this daemon; run `coop net setup` first: %v", definition.Tag, err)
	}
	const (
		boxHome = "/tmp/h" // the probe runs the client under a throwaway HOME inside the image
		boxCwd  = "/tmp/w" // ...from a working directory outside it, so project scope cannot answer for it
	)
	for _, test := range []struct {
		provider, check string
		// want is the file the client's report must name, under the probe's HOME.
		want string
	}{
		// Claude is the weak row, and stays labelled as such: it has no offline command that reports
		// loaded skills (`claude skills list` needs an account), so this asks its validator — pointed
		// at ~/.claude, where it picks the `skills/` subdirectory by its OWN convention rather than
		// being handed the path. Claude reports only what it OBJECTS to (a valid skill leaves
		// `contents: []`, byte-identical to an empty directory), so the signal is the frontmatter-less
		// canary beside the real skill; `--strict` turns that warning into a JSON entry. What this
		// proves is that claude's own tooling finds and parses skills at the mounted layout — NOT that
		// its runtime loads them, which only a live prompt can show.
		{"claude", `claude plugin validate "$HOME/.claude" --json --strict`,
			".claude/skills/coop-canary/SKILL.md"},
		// Codex has no CLI that lists skills — discovery is model-facing (`skills.list`). Its
		// app-server speaks the same catalog over stdio JSON-RPC with no model call; see
		// codexAppServerDriver for why the driving is shared and why it waits rather than sleeps.
		{"codex", codexAppServerDriver("skills/list"), ".codex/skills/coop-review-board/SKILL.md"},
		{"gemini", `gemini skills list`, ".gemini/skills/coop-review-board/SKILL.md"},
		{"grok", `grok inspect --json`, ".grok/skills/coop-review-board/SKILL.md"},
	} {
		t.Run(test.provider, func(t *testing.T) {
			ag, ok := agents.Get(test.provider)
			if !ok {
				t.Fatalf("no adapter registered for %q", test.provider)
			}
			if !ag.SkillsCapable() {
				t.Fatalf("%s no longer claims to discover skills, so Coop must stop mounting them; "+
					"drop this row together with the mount", test.provider)
			}
			source := t.TempDir()
			skill := func(dir, name, body string) {
				full := filepath.Join(source, dir, name)
				if err := os.MkdirAll(full, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(full, "SKILL.md"), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			const (
				valid   = "---\nname: coop-review-board\ndescription: Review a change across every hat before landing it.\n---\n\nRun the board.\n"
				flawed  = "no frontmatter here, so claude's validator has something to say\n"
				decoyed = "coop-decoy"
			)
			mounted := filepath.Join("."+test.provider, "skills")
			skill(mounted, "coop-review-board", valid)
			// The decoy sits one directory over, at a path Coop never mounts. It has to be written in
			// whatever shape THIS client would report, or the guard rules out nothing: claude reports
			// only what it objects to, so a valid decoy could never surface there even if claude did
			// scan the directory.
			decoy := valid
			if test.provider == "claude" {
				decoy = flawed
				skill(mounted, "coop-canary", flawed)
			}
			skill(filepath.Join("."+test.provider, "notskills"), decoyed, decoy)

			script := `mkdir -p ` + boxHome + ` ` + boxCwd + ` && cp -R /src/. ` + boxHome + `/ && cd ` + boxCwd + ` && ` + test.check + ` 2>&1`
			out, _ := exec.CommandContext(ctx, rt.Name, "run", "--rm", "--network", "none", "-e", "HOME="+boxHome,
				"-v", source+":/src:ro", "--entrypoint", "sh", definition.Tag, "-c", script).CombinedOutput()
			report := string(out)
			if want := boxHome + "/" + test.want; !strings.Contains(report, want) {
				t.Fatalf("%s never reported the skill at %s:\n%s", test.provider, want, report)
			}
			if strings.Contains(report, "notskills") {
				t.Fatalf("%s reports skills from outside the directory Coop mounts, so naming the mounted "+
					"one proves nothing about where it looks:\n%s", test.provider, report)
			}
		})
	}
}
