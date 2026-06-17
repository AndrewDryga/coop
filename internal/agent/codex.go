package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/mcp"
)

type codexAgent struct{}

func init() { register(codexAgent{}) }

func (codexAgent) Name() string { return "codex" }

// base guards against an empty COOP_CODEX_CMD override, since the exec/resume forms
// index base[0].
func (codexAgent) base(cfg *config.Config) []string {
	b := cfg.Cmd("COOP_CODEX_CMD", "codex --dangerously-bypass-approvals-and-sandbox")
	if len(b) == 0 {
		b = []string{"codex"}
	}
	return b
}

func (a codexAgent) Interactive(cfg *config.Config) []string { return a.base(cfg) }

func (a codexAgent) Headless(cfg *config.Config, prompt string) []string {
	// codex runs headless via an `exec` subcommand; the prompt is positional.
	b := a.base(cfg)
	return append(append([]string{b[0], "exec"}, b[1:]...), prompt)
}

func (codexAgent) ACP() []string { return []string{"codex-acp"} }

func (a codexAgent) Resume(cfg *config.Config, ws string) ([]string, bool) {
	// `codex resume --last` is global, so find this fork's most recent session by the
	// cwd recorded in its session files and resume that one by id.
	if id := latestCodexSession(cfg.AgentDir("codex"), ws); id != "" {
		b := a.base(cfg)
		return append([]string{b[0], "resume", id}, b[1:]...), true
	}
	return a.Interactive(cfg), false
}

func (codexAgent) Login(*config.Config) []string {
	// Device-code flow: the box has no browser and codex's localhost OAuth redirect
	// can't reach the host, so browser login hangs. --device-auth prints a URL + code.
	return []string{"codex", "login", "--device-auth"}
}

func (codexAgent) ConsultCmd(question string) []string {
	return []string{"codex", "exec", "-s", "read-only", question}
}

func (codexAgent) InstructionFile() string { return "AGENTS.md" }

func (codexAgent) AuthMarker() (file, envKey string) { return "auth.json", "OPENAI_API_KEY" }

// MCP emits the shared servers as [mcp_servers.*] in codex's config.toml.
func (codexAgent) MCP(cfg *config.Config) ([]MCPMount, error) {
	cx, err := mcp.GenerateCodex(cfg.MCPFile, filepath.Join(cfg.AgentDir("codex"), "config.toml"))
	if err != nil {
		return nil, err
	}
	return []MCPMount{{Content: cx, BoxPath: cfg.HomeInBox + "/.codex/config.toml"}}, nil
}

// EnsureDefaults pre-trusts the workdir in codex's config.toml so a fresh box doesn't
// stop at "Do you trust this directory?". Codex records trust as
// [projects."<dir>"] trust_level = "trusted"; we append it idempotently. The box is the
// sandbox, so trusting the one mounted repo is the intended posture.
func (a codexAgent) EnsureDefaults(cfg *config.Config, workdir string) {
	if workdir == "" {
		return
	}
	dir := cfg.AgentDir(a.Name())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	path := filepath.Join(dir, "config.toml")
	data, _ := os.ReadFile(path) // missing file → empty, which is fine
	if strings.Contains(string(data), fmt.Sprintf("projects.%q", workdir)) {
		return // leave any existing entry for this dir untouched
	}
	out := string(data)
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	if out != "" {
		out += "\n"
	}
	out += fmt.Sprintf("[projects.%q]\ntrust_level = \"trusted\"\n", workdir)
	os.WriteFile(path, []byte(out), 0o644)
}

// latestCodexSession returns the id of the most recent codex session recorded for cwd,
// or "" if none. Codex stores sessions flat by date as JSONL whose first line is a
// session_meta carrying {id, cwd}.
func latestCodexSession(codexDir, cwd string) string {
	var bestID string
	var bestTime time.Time
	_ = filepath.WalkDir(filepath.Join(codexDir, "sessions"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".jsonl") {
			return nil
		}
		f, openErr := os.Open(p)
		if openErr != nil {
			return nil
		}
		defer f.Close()
		line, _ := bufio.NewReader(f).ReadString('\n')
		var m struct {
			Payload struct {
				ID, Cwd string
			} `json:"payload"`
		}
		if json.Unmarshal([]byte(line), &m) != nil || m.Payload.Cwd != cwd || m.Payload.ID == "" {
			return nil
		}
		if info, err := d.Info(); err == nil && info.ModTime().After(bestTime) {
			bestTime, bestID = info.ModTime(), m.Payload.ID
		}
		return nil
	})
	return bestID
}
