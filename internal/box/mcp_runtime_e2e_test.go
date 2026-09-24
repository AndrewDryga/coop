//go:build boxruntimee2e

package box

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// An operator's shared MCP server is supposed to be REACHABLE by every client that can be asked, not
// merely mentioned in a config Coop wrote. The process-level test proves the file arrives at a path
// the client reads; this proves the client then launches that server and agrees a protocol with it —
// offline, because a stdio server is a child process, not a network peer.
//
// The server is the witness: `testdata/mcpprobe` logs every JSON-RPC method it receives, so the
// assertion is made from the SERVER's side. What separates a real handshake from an announced one is
// ORDER, not presence: the probe sits on its initialize RESULT for half a second and logs
// `answered initialize` only after writing it. A concurrent reader records pipelined notifications
// before that line, so they fail here (response ordering, not proof of a client's private reads).
// Presence alone would pass for a
// client that talked past the server entirely — this test's own codex driver writes initialize and
// initialized back to back, which is exactly the shape being ruled out.
//
// The configuration is rendered by Coop's own adapters (`agents.Agent.MCP`), not written by hand, so
// a change that breaks the generated shape fails here instead of in production.
//
// CLAUDE IS ABSENT ON PURPOSE — do not "fix" it by adding a row. Coop gives claude its servers with
// `--mcp-config <snapshot> --strict-mcp-config` on the main invocation, and that path cannot run
// offline: `claude mcp list` rejects `--mcp-config` outright ("unknown option"), and passing it
// before the subcommand is accepted and then ignored ("No MCP servers configured", probe never
// launched). `claude mcp list` reads the `mcpServers` key of its own user config instead — and while
// Coop does write that file (onboarding, bypass and trust keys, under CLAUDE_CONFIG_DIR), it
// deliberately never writes servers there. Claude's shared-tool call is instead required by the
// paid provider-network-live suite; its delivery is also pinned at process level.
//
// Needs the locked client image, which `coop net setup` builds; run it with `make mcp-e2e`.
func TestRuntimeSharedMCPServersAreReachedByEveryProbeableClient(t *testing.T) {
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
	imageID, _, err := docker.Image(ctx, definition.Tag)
	if err != nil {
		t.Fatalf("the locked client image %s is not on this daemon; run `coop net setup` first: %v", definition.Tag, err)
	}
	t.Logf("offline MCP clients: %s (%s), %s/%s", definition.Tag, imageID, binding.OS, binding.Architecture)
	const (
		boxHome   = "/tmp/h" // a throwaway HOME, so nothing of the operator's is in play
		boxCwd    = "/tmp/w" // ...and a working directory outside it (see skills-e2e for why)
		boxServer = boxHome + "/bin/mcpprobe"
		boxLog    = boxHome + "/probe.log"
		boxTouch  = boxHome + "/.mcpprobe-launched"
		marker    = "--- what the server saw ---"
	)

	// The witness, built for the image's platform.
	probe := filepath.Join(t.TempDir(), "mcpprobe")
	build := exec.CommandContext(ctx, "go", "build", "-o", probe, "./testdata/mcpprobe")
	build.Env = append(os.Environ(), "GOOS="+binding.OS, "GOARCH="+binding.Architecture, "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the probe server: %v\n%s", err, out)
	}

	// Two shared files: the operator's, naming that server, and an empty one for the control.
	write := func(body string) string {
		path := filepath.Join(t.TempDir(), "mcp.json")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	shared := write(`{"mcpServers":{"coop-probe":{"type":"stdio","command":"` + boxServer +
		`","args":[],"env":{"COOP_PROBE_LOG":"` + boxLog + `"}}}}`)
	noServers := write(`{"mcpServers":{}}`)

	for _, test := range []struct{ provider, check string }{
		// Codex will not connect from any `codex mcp` subcommand — `mcp list` prints the config
		// without launching anything. Its app-server's `mcpServerStatus/list` does launch them, and
		// speaks no model. Its stdin stays open until that reply lands, which is also what keeps the
		// spawned server alive long enough to finish its handshake (see codexAppServerDriver).
		{"codex", codexAppServerDriver("mcpServerStatus/list")},
		// No GEMINI_CLI_TRUST_WORKSPACE here on purpose: gemini suppresses user-level MCP servers in
		// an untrusted folder, and what keeps that from silently disabling every operator server in a
		// real box is `security.folderTrust.enabled=false` in the settings Coop generates. Reading the
		// server's log after `gemini mcp list` is therefore also the standing test of that setting —
		// flip it back to true and this row, and only this row, goes red.
		{"gemini", `timeout 60 gemini mcp list`},
		{"grok", `timeout 60 grok mcp doctor`},
	} {
		t.Run(test.provider, func(t *testing.T) {
			ag, ok := agents.Get(test.provider)
			if !ok {
				t.Fatalf("no adapter registered for %q", test.provider)
			}
			// Render from a scratch configuration, not this host's: the adapters merge the operator's
			// own agent settings and honour COOP_* keys, so a real coop.conf would both read what this
			// test must never project into a container and make the rendered file differ per machine.
			t.Setenv("COOP_CONFIG_DIR", t.TempDir())
			t.Setenv("COOP_CONF", write(""))

			// rendered is what the last run placed, kept so a failure can look at the configuration it
			// actually ran with instead of guessing.
			var rendered []agents.MCPMount
			// run renders one client's configuration from a shared file and reports what the server saw.
			run := func(t *testing.T, sharedFile string) string {
				t.Helper()
				cfg, err := config.Load()
				if err != nil {
					t.Fatal(err)
				}
				cfg.HomeInBox = boxHome
				cfg.MCPFile = sharedFile
				projection, err := ag.MCP(cfg, boxCwd)
				if err != nil {
					t.Fatalf("render %s's MCP configuration: %v", test.provider, err)
				}
				source := t.TempDir()
				if err := os.MkdirAll(filepath.Join(source, "bin"), 0o755); err != nil {
					t.Fatal(err)
				}
				body, err := os.ReadFile(probe)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(source, "bin", "mcpprobe"), body, 0o755); err != nil {
					t.Fatal(err)
				}
				rendered = projection.Mounts
				written := 0
				for _, mount := range projection.Mounts {
					rel, err := filepath.Rel(boxHome, mount.BoxPath)
					if err != nil || strings.HasPrefix(rel, "..") {
						// Never skip quietly: the row would then fail as "never launched" and blame the
						// client for a config this test declined to place.
						t.Fatalf("%s renders %s outside the probe's home, so this test cannot place it",
							test.provider, mount.BoxPath)
					}
					target := filepath.Join(source, filepath.FromSlash(rel))
					if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(target, []byte(mount.Content), 0o644); err != nil {
						t.Fatal(err)
					}
					written++
				}
				if written == 0 {
					t.Fatalf("%s renders no MCP configuration at all from a shared file", test.provider)
				}
				readableLockedClientFixture(t, source)
				script := `mkdir -p ` + boxHome + ` ` + boxCwd + ` && cp -R /src/. ` + boxHome + `/ && cd ` + boxCwd +
					` && { ` + test.check + `; } 2>&1 | tail -6; echo '` + marker + `'; ` +
					`test -e ` + boxTouch + ` && echo server-process-started; cat ` + boxLog + ` 2>/dev/null`
				out, _ := exec.CommandContext(ctx, rt.Name, "run", "--rm", "--network", "none", "-e", "HOME="+boxHome,
					"-v", source+":/src:ro", "--entrypoint", "sh", definition.Tag, "-c", script).CombinedOutput()
				report := string(out)
				// LastIndex, not Cut: if a client ever echoed the marker, splitting on the first one
				// would fold its own output into the server's.
				at := strings.LastIndex(report, marker)
				if at < 0 {
					t.Fatalf("the probe script never finished — no report from %s:\n%s", test.provider, report)
				}
				return report[at+len(marker):]
			}

			seen := run(t, shared)
			lines := strings.Fields(strings.ReplaceAll(strings.TrimSpace(seen), "answered initialize", "answered-initialize"))
			switch {
			case !slices.Contains(lines, "server-process-started"):
				t.Fatalf("%s never launched the operator's MCP server%s:\n%s",
					test.provider, folderTrustHint(rendered), seen)
			case !slices.Contains(lines, "launched"):
				t.Fatalf("%s launched the server but its configured env never reached it, so the server "+
					"could not report anything:\n%s", test.provider, seen)
			case !slices.Contains(lines, "initialize"):
				t.Fatalf("%s launched the server and never opened the handshake:\n%s", test.provider, seen)
			case !slices.Contains(lines, "notifications/initialized"):
				t.Fatalf("%s opened the handshake and never confirmed it:\n%s", test.provider, seen)
			// Checked before the order comparison below, which a MISSING line would otherwise satisfy
			// vacuously: slices.Index returns -1, and every real position sorts after it.
			case !slices.Contains(lines, "answered-initialize"):
				t.Fatalf("%s got no initialize answer from the server, so there is nothing its "+
					"confirmation could have been a response to:\n%s", test.provider, seen)
			case slices.Index(lines, "notifications/initialized") < slices.Index(lines, "answered-initialize"):
				t.Fatalf("%s confirmed the handshake BEFORE reading the server's answer, so the two never "+
					"agreed a protocol — it announced one:\n%s", test.provider, seen)
			}

			// The failure path, tested: with no server in the operator's file, nothing may start. Without
			// this, every assertion above would pass just as well against a client that launches
			// something on its own.
			if control := run(t, noServers); strings.Contains(control, "server-process-started") {
				t.Fatalf("%s started the probe server with none configured, so reaching it proves nothing "+
					"about the operator's file:\n%s", test.provider, control)
			}
		})
	}
}

// folderTrustHint explains a never-launched server when the rendered configuration is the reason.
//
// Gemini suppresses every user-level MCP server in a folder it does not trust, so Coop writes
// `security.folderTrust.enabled=false` into its settings (`disableGeminiFolderTrust`). Lose that and
// this suite goes red with a message about MCP, which sends the reader to the MCP config — the one
// place where nothing is wrong. So the failure asks the settings it actually ran with.
//
// It reads the rendered content rather than keying off the provider: only gemini's projection is
// JSON with that key, so codex's and grok's TOML simply never match, and the hint cannot outlive the
// setting it describes. Silence when the setting IS disabled is the point — a hint on every failure
// is noise, and would send the next reader down this path for an unrelated break.
func folderTrustHint(mounts []agents.MCPMount) string {
	for _, mount := range mounts {
		var settings struct {
			MCPServers map[string]any `json:"mcpServers"`
			Security   *struct {
				FolderTrust *struct {
					Enabled *bool `json:"enabled"`
				} `json:"folderTrust"`
			} `json:"security"`
		}
		if err := json.Unmarshal([]byte(mount.Content), &settings); err != nil || settings.MCPServers == nil {
			// Not the file that carries the servers: codex's and grok's projections are TOML, and
			// gemini's own extra JSON mounts have no servers in them. Keying off the servers rather
			// than the provider keeps this from firing on a healthy run.
			continue
		}
		trust := settings.Security
		if trust == nil || trust.FolderTrust == nil || trust.FolderTrust.Enabled == nil || *trust.FolderTrust.Enabled {
			return " — and its rendered settings do not turn folder trust OFF, which suppresses every" +
				" user-level MCP server in an untrusted folder; check disableGeminiFolderTrust before" +
				" looking at the MCP configuration"
		}
	}
	return ""
}
