package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/AndrewDryga/coop/internal/acpctl"
	"github.com/AndrewDryga/coop/internal/acpproxy"
	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
)

func TestACPBindingsImportOnlyRepositoryOwnedSessions(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	repo, other := t.TempDir(), t.TempDir()
	const sharedID = "11111111-1111-4111-8111-111111111111"
	const localID = "22222222-2222-4222-8222-222222222222"
	const foreignID = "33333333-3333-4333-8333-333333333333"
	home, err := box.NativeHomePath(cfg, "codex", "", repo, true)
	if err != nil {
		t.Fatal(err)
	}
	put := func(root, id, cwd string) {
		t.Helper()
		dir := filepath.Join(root, "sessions")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]string{"id": id, "cwd": cwd}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), append(data, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
	shared := filepath.Join(cfg.ConfigDir, "codex", "acp-sessions")
	put(shared, sharedID, repo)
	put(home, sharedID, repo)
	put(home, localID, repo)
	put(shared, foreignID, other)
	if ids, err := box.ACPRepositorySessions(cfg, "claude", repo); err != nil || len(ids) != 0 {
		t.Fatalf("unused provider: %v %v", ids, err)
	}
	if _, err := os.Stat(filepath.Join(cfg.ConfigDir, "claude")); !os.IsNotExist(err) {
		t.Fatalf("discovery seeded unused provider: %v", err)
	}
	legacyDir := acpctl.ThreadBindingsDir(cfg)
	legacy := acpctl.OpenThreadBindings(legacyDir)
	records := map[string]acpproxy.SessionBinding{"shared-editor": {Provider: "codex", AdapterID: sharedID}, "local-editor": {Provider: "codex", AdapterID: localID}, "foreign-editor": {Provider: "codex", AdapterID: foreignID}}
	for editor, binding := range records {
		legacy.Save(editor, binding)
	}
	read := func(path string) string {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	for _, test := range []struct {
		repo string
		ids  map[string]bool
	}{{repo, map[string]bool{sharedID: true, localID: true}}, {other, map[string]bool{foreignID: true}}} {
		owned, err := box.ACPRepositorySessions(cfg, "codex", test.repo)
		if err != nil || len(owned) != len(test.ids) {
			t.Fatalf("owned sessions: %v %v", owned, err)
		}
		for id := range test.ids {
			if !owned[id] {
				t.Fatalf("missing session %s", id)
			}
		}
		plan, err := acpctl.LegacyThreadBindings(legacyDir, func(b acpproxy.SessionBinding) bool { return b.Provider == "codex" && owned[b.AdapterID] })
		if err != nil || len(plan.Files) != len(test.ids) {
			t.Fatalf("binding plan: %d %v", len(plan.Files), err)
		}
		before := map[string]string{}
		for _, file := range plan.Files {
			before[file.Path] = read(filepath.Join(legacyDir, file.Path))
		}
		destination, err := box.ImportACPThreadBindings(t.Context(), cfg, test.repo, agents.NativeHistoryPlan{})
		if err != nil {
			t.Fatal(err)
		}
		key, _, err := box.HistoryKey(test.repo)
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(legacyDir, key, "bindings"); destination != want {
			t.Fatalf("destination %q want %q", destination, want)
		}
		scoped := acpctl.OpenThreadBindings(destination)
		for range 2 {
			if err := scoped.Migrate(t.Context(), plan, func(filtered agents.NativeHistoryPlan) error {
				got, err := box.ImportACPThreadBindings(t.Context(), cfg, test.repo, filtered)
				if err == nil && got != destination {
					t.Fatalf("changed destination: %q", got)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		for editor, binding := range records {
			got, found := scoped.Lookup(editor)
			if found != test.ids[binding.AdapterID] || found && got != binding {
				t.Fatalf("scoped binding %q: %+v %v", editor, got, found)
			}
			if got, found := legacy.Lookup(editor); !found || got != binding {
				t.Fatalf("source binding changed: %q", editor)
			}
		}
		for path, data := range before {
			if got := read(filepath.Join(legacyDir, path)); got != data {
				t.Fatalf("source bytes changed: %q", path)
			}
		}
	}
}
