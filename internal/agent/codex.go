package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/mcp"
)

type codexAgent struct{}

const (
	// Float on npm's stable latest tag so `coop update` pulls new agent fixes
	// without a source edit. The profile trigger below remains the local guard
	// for openai/codex#28224.
	codexCLIPackage = "@openai/codex@latest"
	codexACPPackage = "@agentclientprotocol/codex-acp@latest"
)

func init() { register(codexAgent{}) }

func (codexAgent) Name() string { return "codex" }

// base guards against an empty COOP_CODEX_CMD override, since the exec/resume forms
// index base[0]. The resolved model rides in base as a trailing --model, which codex
// accepts on its main command and under exec/resume alike.
func (codexAgent) base(cfg *config.Config) []string {
	b := cfg.Cmd("COOP_CODEX_CMD", "codex --dangerously-bypass-approvals-and-sandbox")
	if len(b) == 0 {
		b = []string{"codex"}
	}
	return withModel(b, cfg.ModelFor("codex"))
}

func (a codexAgent) Interactive(cfg *config.Config) []string { return a.base(cfg) }

func (a codexAgent) Headless(cfg *config.Config, prompt string) []string {
	// codex runs headless via an `exec` subcommand; the prompt is positional.
	b := a.base(cfg)
	return append(append([]string{b[0], "exec"}, b[1:]...), prompt)
}

// ACP is a separate adapter binary that takes no agent flags, and codex reads no model
// env var — its model under ACP comes from its own config.toml.
func (codexAgent) ACP(*config.Config) []string { return []string{"codex-acp"} }

// ACPSessionDirs: codex stores rollouts under ~/.codex/sessions (best-effort — codex-acp's resume
// story is weaker than claude's; sharing the dir is the most we can do without a preset-id).
func (codexAgent) ACPSessionDirs() []string { return []string{"sessions"} }

// PresetSessionID is false: codex has no flag to start a session under a caller-chosen
// id (it mints its own UUIDv7), so coop allocates none and Resume scans instead.
func (codexAgent) PresetSessionID() bool { return false }

// StartSession ignores id (codex can't preset one) and just starts interactively;
// Resume finds that session afterward by scanning.
func (a codexAgent) StartSession(cfg *config.Config, _ string) []string {
	return a.Interactive(cfg)
}

func (a codexAgent) Resume(cfg *config.Config, ws, _ string) ([]string, bool) {
	// `codex resume --last` is global, so find this fork's most recent *interactive*
	// session by the cwd recorded in its session files and resume that one by id.
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

func (codexAgent) Packages() []string {
	return []string{codexCLIPackage, codexACPPackage}
}

// Models are common codex model ids. Illustrative — any id the CLI accepts works.
func (codexAgent) Models() []string {
	return []string{"gpt-5.5", "gpt-5-codex", "gpt-5", "o4-mini"}
}

// ModelEnv: codex reads no model env var (its default lives in config.toml), so the
// flag in base() is the only coop-driven path.
func (codexAgent) ModelEnv() string { return "" }

func (codexAgent) InstructionFile() string { return "AGENTS.md" }

func (codexAgent) AuthMarker() (file, envKey string) { return "auth.json", "OPENAI_API_KEY" }

// CredentialEnvKeys is Codex's only token env var.
func (codexAgent) CredentialEnvKeys() []string { return []string{"OPENAI_API_KEY"} }

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
	hardenCodexSQLiteFeedbackLog(dir)
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

// hardenCodexSQLiteFeedbackLog applies the upstream issue's local workaround to
// the mounted Codex profile. It blocks inserts into the feedback-log table only;
// sessions, auth, MCP config, and memories continue to work. Best-effort by
// design: a fresh profile has no DB yet, and custom hosts may not ship sqlite3.
func hardenCodexSQLiteFeedbackLog(dir string) {
	db := filepath.Join(dir, "logs_2.sqlite")
	if info, err := os.Stat(db); err != nil || info.IsDir() {
		return
	}
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		return
	}
	_ = exec.Command(sqlite, db, `CREATE TRIGGER IF NOT EXISTS block_log_inserts BEFORE INSERT ON logs BEGIN SELECT RAISE(IGNORE); END;`).Run()
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
				ID, Cwd, Source string
			} `json:"payload"`
		}
		if json.Unmarshal([]byte(line), &m) != nil || m.Payload.Cwd != cwd || m.Payload.ID == "" {
			return nil
		}
		if m.Payload.Source == "exec" {
			return nil // a loop/consult `codex exec` session, not the interactive one we resume
		}
		if info, err := d.Info(); err == nil && info.ModTime().After(bestTime) {
			bestTime, bestID = info.ModTime(), m.Payload.ID
		}
		return nil
	})
	return bestID
}
