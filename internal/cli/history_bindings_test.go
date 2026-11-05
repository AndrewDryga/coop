package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/AndrewDryga/coop/internal/acpctl"
	"github.com/AndrewDryga/coop/internal/acpproxy"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
)

// Thread bindings that builds before v11 kept flat in acp-threads/ are no longer imported: they stay
// on disk untouched, a reopened thread simply misses, and a repository's own bindings work.
func TestACPThreadBindingsAreRepositoryScopedAndOldOnesAreNotImported(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	repo := t.TempDir()
	flat := filepath.Join(cfg.ConfigDir, "acp-threads")
	old := acpctl.OpenThreadBindings(flat)
	old.Save("editor-thread", acpproxy.SessionBinding{Provider: "codex", AdapterID: "11111111-1111-4111-8111-111111111111"})
	entries, err := os.ReadDir(flat)
	if err != nil || len(entries) == 0 {
		t.Fatalf("flat fixture not written: %v", err)
	}
	before := len(entries)

	dir, err := box.ACPThreadBindingsDir(cfg, repo)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("bindings folder = %v, %v", info, err)
	}
	scoped := acpctl.OpenThreadBindings(dir)
	if _, found := scoped.Lookup("editor-thread"); found {
		t.Fatal("a pre-v11 flat binding was imported")
	}
	binding := acpproxy.SessionBinding{Provider: "codex", AdapterID: "22222222-2222-4222-8222-222222222222"}
	scoped.Save("new-thread", binding)
	if got, found := scoped.Lookup("new-thread"); !found || got != binding {
		t.Fatalf("scoped binding = %+v, %v", got, found)
	}
	scoped.Forget("new-thread")
	if _, found := scoped.Lookup("new-thread"); found {
		t.Fatal("forgotten binding came back")
	}
	if after, err := os.ReadDir(flat); err != nil || len(after) < before {
		t.Fatalf("flat bindings were touched: %d -> %d, %v", before, len(after), err)
	}
	if _, found := old.Lookup("editor-thread"); !found {
		t.Fatal("the flat store lost its binding")
	}
}
