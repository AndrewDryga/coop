package acpctl

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/AndrewDryga/coop/internal/acpproxy"
	agents "github.com/AndrewDryga/coop/internal/agent"
)

func TestThreadBindingMigrationHonorsScopedMappingsAndDeletions(t *testing.T) {
	legacy := OpenThreadBindings(filepath.Join(t.TempDir(), "legacy"))
	old := acpproxy.SessionBinding{Provider: "codex", AdapterID: "old-native"}
	legacy.Save("editor", old)
	plan, err := LegacyThreadBindings(legacy.dir, func(b acpproxy.SessionBinding) bool { return b == old })
	if err != nil || len(plan.Files) != 1 {
		t.Fatal("owned legacy mapping unavailable", err)
	}
	if foreign, err := LegacyThreadBindings(legacy.dir, func(acpproxy.SessionBinding) bool { return false }); err != nil || len(foreign.Files) != 0 {
		t.Fatal("foreign binding crossed repository", err)
	}
	scoped := OpenThreadBindings(filepath.Join(t.TempDir(), "scoped"))
	current := acpproxy.SessionBinding{Provider: "claude", AdapterID: "new-native"}
	scoped.Save("editor", current)
	assertSuppressed := func() {
		t.Helper()
		if err := scoped.Migrate(context.Background(), plan, func(filtered agents.NativeHistoryPlan) error {
			if len(filtered.Files) != 0 {
				t.Fatal("superseded or forgotten binding was republished")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	assertSuppressed()
	if got, ok := scoped.Lookup("editor"); !ok || got != current {
		t.Fatal("legacy mapping replaced current thread")
	}
	scoped.Forget("editor")
	assertSuppressed() // also covers deletion before a deferred source becomes eligible
	// A crash after tombstone publication but before unlink remains a miss.
	data, err := os.ReadFile(legacy.path("editor"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scoped.path("editor"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, ok := scoped.Lookup("editor"); ok {
		t.Fatal("interrupted deletion revived a binding")
	}
	assertSuppressed()
	scoped.Save("editor", current)
	if got, ok := scoped.Lookup("editor"); !ok || got != current {
		t.Fatal("explicit new binding did not clear deletion")
	}
}
