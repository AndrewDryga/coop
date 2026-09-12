package acpctl

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/acpproxy"
)

func TestThreadBindingsRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "acp-threads")
	s := OpenThreadBindings(dir)
	if _, ok := s.Lookup("S1"); ok {
		t.Fatal("empty store reported a binding")
	}
	s.Save("S1", acpproxy.SessionBinding{Provider: "claude", AdapterID: "N1"})
	got, ok := s.Lookup("S1")
	if !ok || got != (acpproxy.SessionBinding{Provider: "claude", AdapterID: "N1"}) {
		t.Fatalf("Lookup after Save = %+v, %v", got, ok)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("store dir entries = %v, %v; want exactly one binding file", entries, err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}\.json$`).MatchString(entries[0].Name()) {
		t.Fatalf("binding file %q is not a hashed name", entries[0].Name())
	}
	if info, err := entries[0].Info(); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("binding file mode = %v, %v; want 0600", info.Mode(), err)
	}
	s.Forget("S1")
	if _, ok := s.Lookup("S1"); ok {
		t.Fatal("Forget left the binding readable")
	}
	var none *ThreadBindings
	none.Save("S1", acpproxy.SessionBinding{Provider: "claude", AdapterID: "N1"})
	none.Forget("S1")
	if _, ok := none.Lookup("S1"); ok {
		t.Fatal("nil store reported a binding")
	}
}

// An adapter mints the editor id inside the box, so it never becomes a host path.
func TestThreadBindingsHashEditorIDs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "acp-threads")
	s := OpenThreadBindings(dir)
	for _, id := range []string{"../../escape", "/abs/path", "a/b", strings.Repeat("x", 4000)} {
		s.Save(id, acpproxy.SessionBinding{Provider: "claude", AdapterID: "N1"})
		if _, ok := s.Lookup(id); !ok {
			t.Fatalf("binding for %q not readable back", id)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 4 {
		t.Fatalf("store dir entries = %d, %v; want the four bindings inside the store dir", len(entries), err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape")); !os.IsNotExist(err) {
		t.Fatal("a binding escaped the store dir")
	}
}

func TestThreadBindingsRejectUnusableEntries(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "acp-threads")
	s := OpenThreadBindings(dir)
	cases := map[string]string{
		"garbage":     "not json",
		"foreign":     `{"editor_id":"S2","provider":"claude","adapter_id":"N1"}`,
		"noProvider":  `{"editor_id":"S1","provider":"","adapter_id":"N1"}`,
		"spaceyID":    `{"editor_id":"S1","provider":"claude","adapter_id":"N1 --evil"}`,
		"oversized":   `{"editor_id":"S1","provider":"claude","adapter_id":"` + strings.Repeat("n", maxThreadBindingBytes) + `"}`,
		"controlChar": "{\"editor_id\":\"S1\",\"provider\":\"cla\\u0001ude\",\"adapter_id\":\"N1\"}",
	}
	for name, body := range cases {
		if err := os.WriteFile(s.path("S1"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, ok := s.Lookup("S1"); ok {
			t.Fatalf("%s: unusable entry was accepted as %+v", name, got)
		}
	}
	// Save refuses the same shapes instead of persisting them.
	s.Save("S1", acpproxy.SessionBinding{Provider: "claude", AdapterID: "N1\n"})
	s.Save("S1", acpproxy.SessionBinding{Provider: "", AdapterID: "N1"})
	if _, ok := s.Lookup("S1"); ok {
		t.Fatal("Save persisted an unusable binding")
	}
}

func TestThreadBindingsPruneStaleEntries(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "acp-threads")
	s := OpenThreadBindings(dir)
	s.Save("old", acpproxy.SessionBinding{Provider: "claude", AdapterID: "N1"})
	s.Save("fresh", acpproxy.SessionBinding{Provider: "claude", AdapterID: "N2"})
	stale := time.Now().Add(-threadBindingMaxAge - time.Hour)
	if err := os.Chtimes(s.path("old"), stale, stale); err != nil {
		t.Fatal(err)
	}
	reopened := OpenThreadBindings(dir)
	if _, ok := reopened.Lookup("old"); ok {
		t.Fatal("a binding older than the retention window survived reopen")
	}
	if _, ok := reopened.Lookup("fresh"); !ok {
		t.Fatal("prune removed a fresh binding")
	}
}
