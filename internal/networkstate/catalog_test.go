package networkstate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/egress"
)

type catalogFixture struct {
	store  *Store
	dir    string
	repo   string
	record LaunchCatalog
	id     string
}

func newCatalogFixture(t *testing.T) *catalogFixture {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	store, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	repo := t.TempDir()
	mode := egress.Open
	policy, err := store.Admit(repo, Admission{InvocationMode: &mode})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	record := LaunchCatalog{
		Version:     1,
		Project:     repo,
		Fingerprint: policy.Fingerprint,
		Members: []CatalogMember{{
			Key:           "work",
			MCPProjection: "none",
			Prototype:     json.RawMessage(`{"command":"fixed"}`),
		}},
	}
	id, err := store.RecordCatalog(record)
	if err != nil {
		t.Fatalf("RecordCatalog: %v", err)
	}
	return &catalogFixture{store: store, dir: dir, repo: repo, record: record, id: id}
}

func (f *catalogFixture) path() string {
	return filepath.Join(f.dir, "catalog-"+f.id+".json")
}

func TestOpenExistingRestoresCatalogWithoutCreatingAuthority(t *testing.T) {
	f := newCatalogFixture(t)
	s, err := OpenExisting(f.dir, []string{f.repo})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Catalog(f.id); err != nil {
		t.Fatal(err)
	}
	for _, missing := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "missing")
		if !missing {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		if opened, err := OpenExisting(path, nil); err == nil || opened != nil {
			t.Fatal("replay created new authority")
		}
		if _, err := os.Stat(filepath.Join(path, "owner.key")); !os.IsNotExist(err) {
			t.Fatal("replay created a key", err)
		}
		if missing {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("replay created a directory", err)
			}
		}
	}
	if opened, err := OpenExisting(f.dir, []string{f.dir}); err == nil || opened != nil {
		t.Fatal("replay accepted exposed authority")
	}
	if err := os.Remove(filepath.Join(f.dir, "owner.key")); err != nil {
		t.Fatal(err)
	}
	if opened, err := OpenExisting(f.dir, nil); err == nil || opened != nil {
		t.Fatal("replay regenerated a lost key")
	}
}

func (f *catalogFixture) rewrite(t *testing.T, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.Remove(f.path()); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := os.WriteFile(f.path(), data, mode); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func (f *catalogFixture) stored(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile(f.path())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	return raw
}

func catalogWithin(t *testing.T, s *Store, id string) error {
	t.Helper()
	type result struct{ err error }
	done := make(chan result, 1)
	go func() {
		_, err := s.Catalog(id)
		done <- result{err}
	}()
	select {
	case r := <-done:
		return r.err
	case <-time.After(60 * time.Second):
		t.Fatal("Catalog blocked instead of failing quickly")
		return nil
	}
}

func TestRecordCatalogIsImmutableAndStable(t *testing.T) {
	f := newCatalogFixture(t)
	if len(f.id) != 64 || strings.ContainsAny(f.id, "ABCDEF") {
		t.Fatalf("unexpected catalog identity shape (length %d)", len(f.id))
	}
	again, err := f.store.RecordCatalog(f.record)
	if err != nil {
		t.Fatalf("second RecordCatalog: %v", err)
	}
	if again != f.id {
		t.Fatal("identical catalog produced a different identity")
	}
}

func TestCatalogRoundTripReturnsDetachedBytes(t *testing.T) {
	f := newCatalogFixture(t)
	got, err := f.store.Catalog(f.id)
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	if got.ID != f.id || got.Version != 1 || got.Project != f.repo || got.Fingerprint != f.record.Fingerprint {
		t.Fatal("round-tripped catalog header does not match the recorded catalog")
	}
	if len(got.Members) != 1 || got.Members[0].Key != "work" || got.Members[0].MCPProjection != "none" {
		t.Fatal("round-tripped catalog members do not match the recorded catalog")
	}
	if string(got.Members[0].Prototype) != `{"command":"fixed"}` {
		t.Fatalf("prototype mismatch: %s", got.Members[0].Prototype)
	}
	got.Members[0].Prototype[0] = 'X'
	fresh, err := f.store.Catalog(f.id)
	if err != nil {
		t.Fatalf("second Catalog: %v", err)
	}
	if string(fresh.Members[0].Prototype) != `{"command":"fixed"}` {
		t.Fatal("mutating returned bytes affected stored catalog state")
	}
}

func TestRecordCatalogPrototypeChangeChangesIdentity(t *testing.T) {
	f := newCatalogFixture(t)
	other := f.record
	other.Members = []CatalogMember{{
		Key:           "work",
		MCPProjection: "none",
		Prototype:     json.RawMessage(`{"command":"other"}`),
	}}
	id, err := f.store.RecordCatalog(other)
	if err != nil {
		t.Fatalf("RecordCatalog: %v", err)
	}
	if id == f.id {
		t.Fatal("changed prototype reused the previous catalog identity")
	}
}

func TestRecordCatalogRefusesInvalidCatalogs(t *testing.T) {
	f := newCatalogFixture(t)
	dup := f.record
	dup.Members = []CatalogMember{f.record.Members[0], f.record.Members[0]}
	noProto := f.record
	noProto.Members = []CatalogMember{{Key: "work", MCPProjection: "none"}}
	selfID := f.record
	selfID.ID = f.id
	inputs := f.record
	inputs.Members = []CatalogMember{{
		Key:           "work",
		InputsID:      strings.Repeat("a", 64),
		MCPProjection: "none",
		Prototype:     json.RawMessage(`{"command":"fixed"}`),
	}}
	bad := f.record
	bad.Version = 2
	cases := map[string]LaunchCatalog{
		"zero":            {},
		"version":         bad,
		"duplicate keys":  dup,
		"no prototype":    noProto,
		"self-supplied":   selfID,
		"ordinary inputs": inputs,
	}
	for name, catalog := range cases {
		if _, err := f.store.RecordCatalog(catalog); err == nil {
			t.Fatalf("%s: RecordCatalog accepted an invalid catalog", name)
		}
	}
}

func TestCatalogRejectsTamperedContent(t *testing.T) {
	f := newCatalogFixture(t)
	catalog, err := f.store.Catalog(f.id)
	if err != nil {
		t.Fatal(err)
	}
	catalog.Project = f.repo + "-other"
	data, _ := json.Marshal(catalog)
	f.rewrite(t, data, 0o600)
	if err := catalogWithin(t, f.store, f.id); err == nil {
		t.Fatal("Catalog accepted tampered content")
	}
}

func TestCatalogRejectsUnknownField(t *testing.T) {
	f := newCatalogFixture(t)
	raw := f.stored(t)
	raw["surprise"] = true
	data, _ := json.Marshal(raw)
	f.rewrite(t, data, 0o600)
	if err := catalogWithin(t, f.store, f.id); err == nil {
		t.Fatal("Catalog accepted an unknown field")
	}
}

func TestCatalogRejectsNonCanonicalEncoding(t *testing.T) {
	f := newCatalogFixture(t)
	raw := f.stored(t)
	data, _ := json.MarshalIndent(raw, "", "  ")
	f.rewrite(t, data, 0o600)
	if err := catalogWithin(t, f.store, f.id); err == nil {
		t.Fatal("Catalog accepted a non-canonical encoding")
	}
}

func TestCatalogRejectsSymlink(t *testing.T) {
	f := newCatalogFixture(t)
	data, err := os.ReadFile(f.path())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(target, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Remove(f.path()); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := os.Symlink(target, f.path()); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if err := catalogWithin(t, f.store, f.id); err == nil {
		t.Fatal("Catalog followed a symlinked catalog file")
	}
}

func TestCatalogRejectsFIFOWithoutBlocking(t *testing.T) {
	f := newCatalogFixture(t)
	if err := os.Remove(f.path()); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := syscall.Mkfifo(f.path(), 0o600); err != nil {
		t.Fatalf("Mkfifo: %v", err)
	}
	if err := catalogWithin(t, f.store, f.id); err == nil {
		t.Fatal("Catalog accepted a FIFO in place of a regular file")
	}
}

func TestCatalogRejectsOversizeFile(t *testing.T) {
	f := newCatalogFixture(t)
	blob := make([]byte, maxCatalogBytes+1024)
	for i := range blob {
		blob[i] = ' '
	}
	f.rewrite(t, blob, 0o600)
	if err := catalogWithin(t, f.store, f.id); err == nil {
		t.Fatal("Catalog accepted an oversize file")
	}
}

func TestCatalogRejectsWorldReadablePermissions(t *testing.T) {
	f := newCatalogFixture(t)
	data, err := os.ReadFile(f.path())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	f.rewrite(t, data, 0o644)
	if err := os.Chmod(f.path(), 0o644); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	if err := catalogWithin(t, f.store, f.id); err == nil {
		t.Fatal("Catalog accepted a world-readable catalog file")
	}
}
