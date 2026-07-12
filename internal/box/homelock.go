package box

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
)

// lockAgentHome serializes boxes on an agent whose CLI keeps SINGLE-WRITER state in its home
// (Agent.ExclusiveHome — codex ≥0.144's sqlite databases): a second box bind-mounting the same
// account's home dies at startup ("failed to initialize sqlite state runtime") or risks
// corrupting it, since sqlite locking over the macOS bind mount doesn't hold between containers.
// The lock is a host-side flock on a dotfile BESIDE the mounted dir (never inside it, so the box
// can't see or tamper with it), taken non-blocking: the second spawn fails fast with an error
// naming the busy account and the way out, instead of a cryptic crash inside the box. Held by
// this process's fd for the run's lifetime — the kernel frees it even on SIGKILL, so a torn-down
// ACP generation never wedges the account. Fusion peers that mount another agent's home aren't
// serialized here — only the lead's own run, the proven collision — and a lock-file open error
// degrades to no lock (best-effort guard, not a new way to fail a run).
func lockAgentHome(cfg *config.Config, spec RunSpec) (unlock func(), err error) {
	if !spec.Homes || spec.Agent == "" {
		return nil, nil
	}
	a, ok := agents.Get(spec.Agent)
	if !ok || !a.ExclusiveHome() {
		return nil, nil
	}
	dir := cfg.AgentDir(spec.Agent)
	lock := filepath.Join(filepath.Dir(dir), "."+filepath.Base(dir)+".box.lock")
	f, oerr := os.OpenFile(lock, os.O_CREATE|os.O_RDWR, 0o600)
	if oerr != nil {
		return nil, nil
	}
	if ferr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); ferr != nil {
		f.Close()
		return nil, fmt.Errorf("%s account %q is already in use by another coop box — %s keeps single-writer state in its home, so two boxes on one account collide; close that session, or run this one on a different account (@account)",
			spec.Agent, cfg.ActiveProfile(spec.Agent), spec.Agent)
	}
	return func() { f.Close() }, nil // closing the fd releases the flock
}
