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

func (codexAgent) Name() string        { return "codex" }
func (codexAgent) DisplayName() string { return "Codex" }
func (codexAgent) Stream() StreamSpec {
	return StreamSpec{Format: StreamCodexJSON, Flags: []string{"--json"}, TrailingArgs: 1}
}

// base guards against an empty COOP_CODEX_CMD override, since the exec/resume forms
// index base[0]. The resolved model rides in base as a trailing --model, which codex
// accepts on its main command and under exec/resume alike.
func (codexAgent) base(cfg *config.Config) []string {
	b := cfg.Cmd("COOP_CODEX_CMD", "codex --dangerously-bypass-approvals-and-sandbox")
	if len(b) == 0 {
		b = []string{"codex"}
	}
	return withEffort(withModel(b, cfg.ModelFor("codex")), codexAgent{}, cfg.EffortFor("codex"))
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
	return []string{
		"gpt-5.6-sol",
		"gpt-5.6-terra",
		"gpt-5.6-luna",
		"gpt-5.5",
		"gpt-5.4",
		"gpt-5.4-mini",
		"gpt-5.3-codex-spark",
	}
}

// ModelEnv: codex reads no model env var (its default lives in config.toml), so the
// flag in base() is the only coop-driven path.
func (codexAgent) ModelEnv() string { return "" }

// EffortFlag: codex takes reasoning effort as a config override, -c model_reasoning_effort=<level>
// (minimal/low/medium/high/xhigh), on its main command and exec/resume alike.
func (codexAgent) EffortFlag(level string) []string {
	return []string{"-c", "model_reasoning_effort=" + level}
}

// EffortEnv: codex reads no effort env var (its default lives in config.toml), so the flag in
// base() is the only coop-driven path — like its model.
func (codexAgent) EffortEnv() string { return "" }

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

// ACPRateLimitSignals: codex-acp surfaces a limit as codexErrorInfo=usageLimitExceeded
// (top-level or nested); the value alone is the proof, whatever key carries it.
func (codexAgent) ACPRateLimitSignals() []ACPSignal {
	return []ACPSignal{{Value: "usageLimitExceeded"}}
}

// ACPSessionConfig: codex-acp exposes no config option coop must force (yolo is
// enforced protocol-side by the controller's autoReply).
func (codexAgent) ACPSessionConfig() map[string]string { return nil }

// BoxEnv points codex's single-writer sqlite state (state_*.sqlite, logs_*, memories_*,
// goals_* — the "state runtime") at a CONTAINER-LOCAL path off the shared ~/.codex bind
// mount, via CODEX_SQLITE_HOME (honored by codex and codex-acp). codex ≥0.144 keeps that
// state in $CODEX_HOME, and two boxes sharing one account's home make the second crash
// ("failed to initialize sqlite state runtime") — sqlite's single-writer lock can't span the
// mount. Redirecting only the sqlite keeps the WHOLE home shared as before (auth + its
// in-place refresh, sessions, config), so any number of codex boxes run in parallel on one
// account, each with its own state on its own writable layer. Ephemeral by design: the
// session INDEX is rebuilt from the shared sessions/ rollouts (codex backfills), so resume
// still works; per-box goals/memories don't persist, which is inherent to parallel sessions.
func (codexAgent) BoxEnv(homeInBox string) []string {
	return []string{"CODEX_SQLITE_HOME=" + homeInBox + "/.codex-state"}
}

// codexText is the jq filter that pulls the agent's reply text out of codex's --json
// stream; the wrapper emits it once (ShellPrelude) since fresh and resume both use it.
const codexText = `codex_text() { jq -r 'select(.type=="item.completed" and .item.type=="agent_message").item.text' 2>/dev/null; }
# codex_peer_row logs this consult's token usage (the turn.completed event, read from stdin) to the
# run's peer-usage file so the loop's closing digest can tally the peer per model. Best-effort: no
# COOP_RUN_ID (not a loop), no usage event, or any write error → nothing, and it never fails the
# consult. codex reports tokens but no cost. Args: <role> <model>.
codex_peer_row() {
	[ -n "${COOP_RUN_ID:-}" ] || return 0
	u=$(jq -c 'select(.type=="turn.completed").usage' 2>/dev/null | tail -n1)
	[ -n "$u" ] || return 0
	i=$(printf '%s' "$u" | jq '.input_tokens // 0')
	o=$(printf '%s' "$u" | jq '(.output_tokens // 0) + (.reasoning_output_tokens // 0)')
	printf '{"run":"%s","role":"%s","provider":"codex","model":"%s","in":%s,"out":%s}\n' "$COOP_RUN_ID" "$1" "$2" "$i" "$o" >>".agent/runs/$COOP_RUN_ID.peers.jsonl" 2>/dev/null || true
}`

func (codexAgent) ConsultFresh() string {
	return `out=$(run codex exec -s read-only ${model:+--model "$model"} ${effort:+-c model_reasoning_effort="$effort"} --json "$prompt"); st=$?
# Only record the thread id when one was actually parsed — on a timeout/failure $out is empty,
# and writing an empty idfile would make the next --continue run "codex exec resume ''".
tid=$(printf '%s\n' "$out" | jq -r 'select(.type=="thread.started").thread_id' 2>/dev/null | head -n1)
if [ -n "$tid" ]; then printf '%s' "$tid" >"$idfile"; fi
printf '%s\n' "$out" | codex_text
printf '%s\n' "$out" | codex_peer_row "$role" "$model"
# codex_text intentionally hides protocol JSON on success. On failure, preserve the
# raw events so the wrapper can classify structured usageLimitExceeded/turn.failed data.
if [ "$st" -ne 0 ]; then printf '%s\n' "$out" >&2; fi
# Propagate codex's own exit status (timeout/error), not the codex_text pipe's 0, so a
# consult failure is observable like claude/gemini's instead of always looking successful.
exit "$st"`
}

func (codexAgent) ConsultResume() string {
	return `out=$(run codex exec resume "$id" -c sandbox_mode=read-only ${model:+--model "$model"} ${effort:+-c model_reasoning_effort="$effort"} --json "$prompt"); st=$?; printf '%s\n' "$out" | codex_text; printf '%s\n' "$out" | codex_peer_row "$role" "$model"; if [ "$st" -ne 0 ]; then printf '%s\n' "$out" >&2; fi; exit "$st"`
}

func (codexAgent) DelegateExec() string {
	return `codex exec --dangerously-bypass-approvals-and-sandbox ${model:+--model "$model"} ${effort:+-c model_reasoning_effort="$effort"} "$prompt"`
}

func (codexAgent) ShellPrelude() string  { return codexText }
func (codexAgent) InstallScript() string { return "" }
