package taskmcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/tasks"
)

func TestCompletionEvidenceHandoff(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	for _, assigned := range []string{"t1", ""} {
		t.Run("assigned="+assigned, func(t *testing.T) {
			for _, retained := range []bool{false, true} {
				root := queue(t, map[string]string{"t1": tasks.StateInProgress})
				dir := filepath.Join(root, tasks.StateInProgress, "t1")
				for _, name := range []string{"tmp", "artifacts"} {
					if err := os.Mkdir(filepath.Join(dir, name), 0o700); err != nil {
						t.Fatal(err)
					}
				}
				const log = "check returned 0; retained output is not attestation\n"
				if err := os.WriteFile(filepath.Join(dir, "tmp", "gate.log"), []byte(log), 0o600); err != nil {
					t.Fatal(err)
				}
				if retained {
					if err := os.WriteFile(filepath.Join(dir, "artifacts", "gate.log"), []byte(log), 0o600); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Symlink("../tmp/gate.log", filepath.Join(dir, "artifacts", "dangling.log")); err != nil {
					t.Fatal(err)
				}
				sess := newSession(t, newServer(t, root, assigned))
				// Refused completion does not consume evidence or issue a success receipt.
				refused := sess.mustRefuse("tasks_complete", map[string]any{"id": "t1"})
				if strings.Contains(refused, completionEvidenceHandoff) {
					t.Fatal("refusal emitted successful handoff")
				}
				if _, err := os.Stat(filepath.Join(dir, "tmp", "gate.log")); err != nil {
					t.Fatalf("refusal lost scratch: %v", err)
				}
				finishChecklist(t, root, "t1")
				for call := 0; call < 2; call++ {
					text := sess.mustCall("tasks_complete", map[string]any{"id": "t1"})
					for _, want := range []string{"Host finalization removes tmp/", "never cite tmp/ as retained evidence", "or say the full log was not retained", "not independent proof", "write nothing more"} {
						if !strings.Contains(text, want) {
							t.Fatalf("handoff missing %q: %s", want, text)
						}
					}
					if strings.Contains(text, "gate.log") || strings.Contains(text, "dangling.log") {
						t.Fatal("receipt invented an artifact retention claim")
					}
				}
				item, ok, err := tasks.CurrentTask(root, "t1")
				if err != nil || !ok {
					t.Fatalf("done task missing: %v", err)
				}
				if assigned != "" {
					if _, err := os.Stat(filepath.Join(item.Dir, "tmp", "gate.log")); err != nil {
						t.Fatalf("assigned move removed scratch before host finalization: %v", err)
					}
				}
				if err := tasks.CompleteTrustedTask(root, item); err != nil {
					t.Fatalf("host finalization: %v", err)
				}
				done := filepath.Join(root, tasks.StateDone, "t1")
				if _, err := os.Stat(filepath.Join(done, "tmp")); !os.IsNotExist(err) {
					t.Fatalf("scratch survived finalization: %v", err)
				}
				body, err := os.ReadFile(filepath.Join(done, "artifacts", "gate.log"))
				if retained && (err != nil || string(body) != log) || !retained && !os.IsNotExist(err) {
					t.Fatalf("wrong retained-artifact result: %q, %v", body, err)
				}
				if !retained {
					if _, err := os.Stat(filepath.Join(done, "artifacts", "dangling.log")); !os.IsNotExist(err) {
						t.Fatalf("tmp link survived as usable evidence: %v", err)
					}
				}
			}
		})
	}
}
