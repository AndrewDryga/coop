package cli

import (
	"os"
	"testing"

	"github.com/AndrewDryga/coop/internal/acpproxy"
)

// TestACPResumeStateRoundTrip: the SIGHUP handoff JSON-round-trips through a 0600 temp file that's
// removed after one read (the setup lines it carries are sensitive).
func TestACPResumeStateRoundTrip(t *testing.T) {
	st := acpResumeState{
		Proxy: acpproxy.Snapshot{
			Setup:    [][]byte{[]byte(`{"method":"initialize"}`)},
			Sessions: []acpproxy.SessionSnap{{EditorID: "S1", Turned: true}},
		},
		Ctrl: ctrlSnapshot{Sel: "agent:codex", Lead: "codex", Model: "m", LeadUsesSetModel: true},
	}
	path, err := writeResumeState(st)
	if err != nil {
		t.Fatal(err)
	}
	if fi, serr := os.Stat(path); serr != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("resume file perms = %v (err %v), want 0600", fi.Mode().Perm(), serr)
	}
	got, err := readResumeState(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Ctrl.Lead != "codex" || !got.Ctrl.LeadUsesSetModel || got.Ctrl.Sel != "agent:codex" {
		t.Errorf("ctrl round-trip mismatch: %+v", got.Ctrl)
	}
	if len(got.Proxy.Sessions) != 1 || got.Proxy.Sessions[0].EditorID != "S1" || !got.Proxy.Sessions[0].Turned {
		t.Errorf("proxy round-trip mismatch: %+v", got.Proxy.Sessions)
	}
	if _, serr := os.Stat(path); !os.IsNotExist(serr) {
		t.Error("resume file must be removed after one read")
	}
}
