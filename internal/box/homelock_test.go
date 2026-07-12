package box

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

// The home lock serializes exclusive-home agents (codex) per account: the first box holds the
// lock, a second on the SAME account fails fast with an error naming the account and the way
// out, a box on a DIFFERENT account is untouched, and releasing the first frees the account.
// Non-exclusive agents (claude) never lock.
func TestLockAgentHome(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{ConfigDir: dir}
	for _, p := range []string{"codex/profiles/personal", "codex/profiles/backup", "claude/profiles/personal"} {
		if err := os.MkdirAll(filepath.Join(dir, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	spec := RunSpec{Homes: true, Agent: "codex"}
	cfg.SetActiveProfile("codex", "personal")

	unlock, err := lockAgentHome(cfg, spec)
	if err != nil || unlock == nil {
		t.Fatalf("first codex box must take the lock (got unlock=%t err=%v)", unlock != nil, err)
	}
	// The lock file lives BESIDE the profile dir, never inside the mounted home.
	if _, serr := os.Stat(filepath.Join(dir, "codex", "profiles", ".personal.box.lock")); serr != nil {
		t.Errorf("lock file not beside the profile dir: %v", serr)
	}
	// Second box on the same account: refused, with the account and remedy named.
	if _, err := lockAgentHome(cfg, spec); err == nil {
		t.Fatal("a second codex box on the same account must be refused")
	} else if !strings.Contains(err.Error(), "personal") || !strings.Contains(err.Error(), "@account") {
		t.Errorf("the refusal should name the busy account and the way out: %v", err)
	}
	// A different account is independent.
	cfg2 := &config.Config{ConfigDir: dir}
	cfg2.SetActiveProfile("codex", "backup")
	if unlock2, err := lockAgentHome(cfg2, spec); err != nil || unlock2 == nil {
		t.Fatalf("a different account must not be blocked: %v", err)
	} else {
		unlock2()
	}
	// Releasing frees the account for the next box.
	unlock()
	if unlock3, err := lockAgentHome(cfg, spec); err != nil || unlock3 == nil {
		t.Fatalf("after release the account must be lockable again: %v", err)
	} else {
		unlock3()
	}
	// Non-exclusive agents and non-Homes runs never lock.
	if u, err := lockAgentHome(cfg, RunSpec{Homes: true, Agent: "claude"}); u != nil || err != nil {
		t.Error("claude has no exclusive home — no lock expected")
	}
	if u, err := lockAgentHome(cfg, RunSpec{Homes: false, Agent: "codex"}); u != nil || err != nil {
		t.Error("a Homes-less run mounts no agent home — no lock expected")
	}
}
