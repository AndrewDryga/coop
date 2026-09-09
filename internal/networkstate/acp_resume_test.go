package networkstate

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCatalogHandoffTypedCustodyCannotBeConfused(t *testing.T) {
	for _, kind := range []string{HandoffForkWorker, HandoffACPChild, HandoffACPReload} {
		t.Run(kind, func(t *testing.T) {
			f, h := handoffFixture(t)
			p := h.ForkWorker.Child
			h.Kind = kind
			switch kind {
			case HandoffACPChild:
				h.ForkWorker = nil
				h.ACPChild = &ACPChildHandoff{SupervisorID: strings.Repeat("a", 16), MemberKey: "work", Supervisor: p, Child: p, Group: p}
			case HandoffACPReload:
				h.ForkWorker = nil
				id, err := f.store.RecordACPResume([]byte(`{"proxy":{},"ctrl":{}}`))
				if err != nil {
					t.Fatal(err)
				}
				h.ACPReload = &ACPReloadHandoff{SupervisorID: strings.Repeat("a", 16), Supervisor: p, ResumeID: id}
			}
			if err := f.store.RecordCatalogHandoff(h); err != nil {
				t.Fatal(err)
			}
			for _, mutation := range []string{"version", "kind", "mixed", "missing"} {
				changed := h
				changed.Locator = strings.Repeat("d", 32)
				switch mutation {
				case "version":
					changed.Version = 1
				case "kind":
					changed.Kind = "invented"
				case "mixed":
					if kind == HandoffACPChild {
						changed.ForkWorker = &ForkWorkerHandoff{}
					} else {
						changed.ACPChild = &ACPChildHandoff{}
					}
				case "missing":
					changed.ForkWorker, changed.ACPChild, changed.ACPReload = nil, nil, nil
				}
				if err := f.store.RecordCatalogHandoff(changed); err == nil {
					t.Fatalf("%s admitted %s custody", kind, mutation)
				}
			}
		})
	}
}

func TestACPResumeIsSeparatelyBoundedAuthenticatedAndConsumedOnce(t *testing.T) {
	f, h := handoffFixture(t)
	// A valid resume may be larger than the small owner configuration frame.
	data, err := json.Marshal(map[string]string{"sessions": strings.Repeat("x", 2<<20)})
	if err != nil {
		t.Fatal(err)
	}
	id, err := f.store.RecordACPResume(data)
	if err != nil {
		t.Fatal(err)
	}
	if same, err := f.store.RecordACPResume(data); err != nil || same != id {
		t.Fatal("resume publication lost idempotence", err)
	}
	read, err := f.store.ACPResume(id)
	if err != nil || !bytes.Equal(read, data) {
		t.Fatal("resume round trip changed bytes", err)
	}
	h.Kind, h.ACPReload = HandoffACPReload, &ACPReloadHandoff{SupervisorID: strings.Repeat("a", 16), Supervisor: h.ForkWorker.Child, ResumeID: id}
	h.ForkWorker = nil
	if err := f.store.RecordCatalogHandoff(h); err != nil {
		t.Fatal(err)
	}
	h, err = f.store.CatalogHandoff(h.Locator)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.ConsumeACPReload(h.Locator, strings.Repeat("f", 64)); err == nil {
		t.Fatal("different identity consumed reload")
	}
	if err := f.store.ConsumeACPReload(h.Locator, h.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ConsumeACPReload(h.Locator, h.ID); err == nil {
		t.Fatal("same process replayed consumed reload")
	}
	path := filepath.Join(f.dir, "acp-resume-"+id+".json")
	if err := os.WriteFile(path, []byte(`{"changed":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ACPResume(id); err == nil {
		t.Fatal("tampered resume accepted")
	}
	if _, err := f.store.ACPResume("../escape"); err == nil {
		t.Fatal("invalid resume reference accepted")
	}
}

func TestACPReloadAmbiguousConsumptionCannotReplay(t *testing.T) {
	f, h := handoffFixture(t)
	id, err := f.store.RecordACPResume([]byte(`{"proxy":{},"ctrl":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	h.Kind, h.ACPReload = HandoffACPReload, &ACPReloadHandoff{SupervisorID: strings.Repeat("a", 16), Supervisor: h.ForkWorker.Child, ResumeID: id}
	h.ForkWorker = nil
	if err := f.store.RecordCatalogHandoff(h); err != nil {
		t.Fatal(err)
	}
	h, err = f.store.CatalogHandoff(h.Locator)
	if err != nil {
		t.Fatal(err)
	}
	fault := errors.New("injected reload consumption sync failure")
	f.store.syncDir = func(*os.File) error { return fault }
	if err := f.store.ConsumeACPReload(h.Locator, h.ID); !errors.Is(err, fault) {
		t.Fatalf("consumption error = %v, want injected sync failure", err)
	}
	f.store.syncDir = nil
	if err := f.store.ConsumeACPReload(h.Locator, h.ID); err == nil || !strings.Contains(err.Error(), "already consumed") {
		t.Fatalf("ambiguous consumption retry = %v, want replay refusal", err)
	}
}
