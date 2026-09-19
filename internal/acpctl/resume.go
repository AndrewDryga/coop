package acpctl

import (
	"encoding/json"
	"os"

	"github.com/AndrewDryga/coop/internal/acpproxy"
	agents "github.com/AndrewDryga/coop/internal/agent"
)

// ResumeState is the whole handoff a `coop acp` supervisor carries across a SIGHUP re-exec: the
// proxy's session state + the controller's selection. JSON-serialized to a 0600 temp file whose path
// rides COOP_ACP_RESUME_STATE into the re-exec'd process. Moved from internal/cli/commands.go: it
// has no app/box coupling of its own, and its two mover test files (resume_test.go) exercise it
// directly.
type ResumeState struct {
	Proxy acpproxy.Snapshot `json:"proxy"`
	Ctrl  Snapshot          `json:"ctrl"`
	// PriorSupervisor is the previous generation's supervisor id when its pre-exec box sweep
	// failed: the next generation retries that sweep once, since the pid survives the re-exec and
	// the orphan sweep would otherwise wait for the whole supervisor to exit.
	PriorSupervisor string `json:"prior_supervisor,omitempty"`
}

// WriteResumeState JSON-encodes the handoff to a 0600 temp file (CreateTemp is 0600) and returns its
// path. The initialize, authentication, and session data are sensitive, so the file is owner-only
// and removed after one read.
func WriteResumeState(st ResumeState) (string, error) {
	data, err := json.Marshal(st)
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp("", "coop-acp-resume-*.json")
	if err != nil {
		return "", err
	}
	if _, werr := f.Write(data); werr != nil {
		f.Close()
		os.Remove(f.Name())
		return "", werr
	}
	if cerr := f.Close(); cerr != nil {
		os.Remove(f.Name()) // a flush failure on close still wrote bytes — don't leave handoff data in /tmp
		return "", cerr
	}
	return f.Name(), nil
}

// ReadResumeState reads + REMOVES the handoff file (consumed once, so a stale file can't resurrect on
// a later crash-respawn) and unsets the env var so the child boxes don't inherit it.
func ReadResumeState(path string) (ResumeState, error) {
	defer os.Remove(path)
	os.Unsetenv("COOP_ACP_RESUME_STATE")
	var st ResumeState
	data, err := os.ReadFile(path)
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, err
	}
	return st, st.Proxy.Validate()
}

// WarmSwitch reports whether a spawn target is one a warm box can serve: a plain provider target —
// no preset — at the provider's default model and effort, which is what a warm box runs. It says
// nothing of the account: a warm box serves only the account it started on, and the pool checks that
// at checkout. The editor's Provider selector always resolves an account, so this, not a bare target,
// is the common switch; a pinned model or effort, or a preset, still starts cold.
func WarmSwitch(t agents.Target, psName string, ok bool, defaultModel, defaultEffort string) bool {
	return ok && psName == "" && t.Provider != "" && len(t.Accounts) <= 1 &&
		(t.Model == "" || t.Model == defaultModel) && (t.Effort == "" || t.Effort == defaultEffort)
}
