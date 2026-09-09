package networkstate

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func handoffFixture(t *testing.T) (*catalogFixture, CatalogHandoff) {
	t.Helper()
	f := newCatalogFixture(t)
	h := CatalogHandoff{Version: 2, Kind: HandoffForkWorker, Locator: strings.Repeat("a", 32), CatalogID: f.id,
		Project: f.repo, Workspace: f.repo, WorkspaceFork: &HandoffForkIdentity{Name: "fixture", Generation: strings.Repeat("b", 32)},
		ForkWorker: &ForkWorkerHandoff{ReservationDigest: strings.Repeat("c", 64), Child: Supervisor{PID: os.Getpid(), StartToken: "linux-proc-v1:fixture-token"}},
		Runtime:    "/usr/local/bin/docker", Exposed: []string{f.repo}, Owner: json.RawMessage(`{"version":1}`)}
	return f, h
}

func TestCatalogEnvelopeDoesNotOpenCapturedAuthority(t *testing.T) {
	f := newCatalogFixture(t)
	if err := os.Remove(filepath.Join(f.dir, "snapshot-"+f.record.Fingerprint+".json")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.CatalogEnvelope(f.id); err != nil {
		t.Fatal("metadata read opened policy or captured inputs", err)
	}
	if _, err := f.store.Catalog(f.id); err == nil {
		t.Fatal("full validation accepted unavailable policy")
	}
}

func TestCatalogHandoffBindsExactChildAndIsImmutable(t *testing.T) {
	f, h := handoffFixture(t)
	if err := f.store.RecordCatalogHandoff(h); err != nil {
		t.Fatal(err)
	}
	if err := f.store.RecordCatalogHandoff(h); err != nil {
		t.Fatal("identical publication lost idempotence", err)
	}
	got, err := f.store.CatalogHandoff(h.Locator)
	if err != nil || got.ForkWorker.Child != h.ForkWorker.Child || !bytes.Equal(got.Owner, h.Owner) || !lowerHex(got.ID, 64) {
		t.Fatal("handoff round trip failed", err)
	}
	got.Owner[0] = 'X'
	got.Exposed[0] = "/changed"
	again, err := f.store.CatalogHandoff(h.Locator)
	if err != nil || !bytes.Equal(again.Owner, h.Owner) || again.Exposed[0] != h.Exposed[0] {
		t.Fatal("read shared mutable state", err)
	}
	for _, change := range []func(*CatalogHandoff){
		func(h *CatalogHandoff) { h.ForkWorker.Child.PID++ },
		func(h *CatalogHandoff) { h.ForkWorker.Child.StartToken += "replacement" },
		func(h *CatalogHandoff) { h.ForkWorker.ReservationDigest = strings.Repeat("d", 64) },
		func(h *CatalogHandoff) { h.WorkspaceFork.Generation = strings.Repeat("e", 32) },
		func(h *CatalogHandoff) { h.Owner = json.RawMessage(`{"version":2}`) },
	} {
		changed := h
		worker, fork := *h.ForkWorker, *h.WorkspaceFork
		changed.ForkWorker, changed.WorkspaceFork = &worker, &fork
		change(&changed)
		if err := f.store.RecordCatalogHandoff(changed); err == nil {
			t.Fatal("locator rebound to different child or owner")
		}
	}
}

func TestCatalogHandoffRejectsTamperingAndUnboundedFrames(t *testing.T) {
	for _, kind := range []string{"changed-child", "unknown-field", "duplicate", "whitespace", "truncated", "exposure", "owner-size", "bad-locator", "nonprivate", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			f, h := handoffFixture(t)
			switch kind {
			case "exposure":
				h.Exposed = []string{f.dir}
			case "owner-size":
				h.Owner, _ = json.Marshal(strings.Repeat("x", 1<<20))
			case "bad-locator":
				h.Locator = "../escape"
			}
			err := f.store.RecordCatalogHandoff(h)
			if kind == "exposure" || kind == "owner-size" || kind == "bad-locator" {
				if err == nil {
					t.Fatal("invalid frame published")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(f.dir, "handoff-"+h.Locator+".json")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "changed-child":
				data = bytes.Replace(data, []byte("fixture-token"), []byte("changed-token"), 1)
			case "unknown-field":
				data = append([]byte(`{"unknown":true,`), data[1:]...)
			case "duplicate":
				data = append([]byte(`{"version":1,`), data[1:]...)
			case "whitespace":
				data = append(data, '\n')
			case "truncated":
				data = data[:len(data)-1]
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if kind == "nonprivate" {
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "symlink" {
				if err := os.Rename(path, path+".target"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".target", path); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.store.CatalogHandoff(h.Locator); err == nil {
				t.Fatal("invalid handoff accepted")
			}
		})
	}
}
