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
}

// WriteResumeState JSON-encodes the handoff to a 0600 temp file (CreateTemp is 0600) and returns its
// path — the setup lines it carries are sensitive, so it's owner-only and removed after one read.
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
		os.Remove(f.Name()) // a flush failure on close still wrote bytes — don't leave the setup lines in /tmp
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
	return st, json.Unmarshal(data, &st)
}

// BareProviderSwitch reports whether a spawn target is a plain provider switch at default
// account/model/effort — a bare Target{Provider} — the slow, common case the warm pool covers. A
// pinned target or preset spawns cold (rare; correctness is unaffected).
func BareProviderSwitch(t agents.Target, psName string, ok bool) bool {
	return ok && psName == "" && t.Model == "" && t.Effort == "" && len(t.Accounts) == 0
}
