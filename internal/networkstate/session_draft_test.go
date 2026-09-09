package networkstate

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/egress"
)

func sessionNetworkDraftFixture(t *testing.T) (*Store, SessionNetworkDraft) {
	t.Helper()
	f := newCatalogFixture(t)
	inputs, err := f.store.RecordInputs(LaunchInputs{
		Environment: []byte("SESSION_DRAFT_SECRET=never-in-draft\n"),
		MCP:         []byte(`{"mcpServers":{"private":{"command":"secret"}}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	return f.store, SessionNetworkDraft{
		Version: 1, Project: f.repo, PolicyFingerprint: f.record.Fingerprint, ReferenceDigest: strings.Repeat("b", 64),
		Mode: egress.Open, SessionID: "session-owner", OperationID: "create-session",
		ForkName: "session-fork", AuthorityDigest: strings.Repeat("a", 64), Exposed: []string{f.repo},
		Members: []SessionNetworkDraftMember{{Key: "claude/default", InputsID: inputs,
			Plan: json.RawMessage(`{"agent":"claude","target":"claude@default"}`)}},
	}
}

func TestSessionNetworkDraftRoundTripIsIdempotentAndDetached(t *testing.T) {
	store, draft := sessionNetworkDraftFixture(t)
	id, err := store.RecordSessionNetworkDraft(draft)
	if err != nil || !lowerHex(id, 64) {
		t.Fatalf("record draft = %q, %v", id, err)
	}
	again, err := store.RecordSessionNetworkDraft(draft)
	if err != nil || again != id {
		t.Fatalf("identical draft lost idempotence: %q, %v", again, err)
	}
	envelope, err := store.SessionNetworkDraftEnvelope(id)
	draft.ID = id
	if err != nil || !equalJSON(envelope, draft) {
		t.Fatal("draft envelope changed", err)
	}
	loaded, err := store.LoadSessionNetworkDraft(id)
	if err != nil || !equalJSON(loaded, envelope) {
		t.Fatal("loaded draft changed", err)
	}
	envelope.Members[0].Plan[0] = 'X'
	fresh, err := store.SessionNetworkDraftEnvelope(id)
	if err != nil || string(fresh.Members[0].Plan) != `{"agent":"claude","target":"claude@default"}` {
		t.Fatal("caller mutated retained draft bytes", err)
	}
	data, err := os.ReadFile(filepath.Join(store.Path(), "session-draft-"+id+".json"))
	if err != nil || bytes.Contains(data, []byte("SESSION_DRAFT_SECRET")) || bytes.Contains(data, []byte("mcpServers")) {
		t.Fatal("draft metadata exposed secret input payloads", err)
	}
}

func TestSessionNetworkDraftRequiresExactCapturedPolicyProjectAndInputs(t *testing.T) {
	store, draft := sessionNetworkDraftFixture(t)
	otherInputs, err := openStore(t).RecordInputs(LaunchInputs{Environment: []byte("FOREIGN=secret\n")})
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*SessionNetworkDraft){
		"mode":              func(v *SessionNetworkDraft) { v.Mode = egress.None },
		"export":            func(v *SessionNetworkDraft) { v.ExportDestinations = true },
		"policy":            func(v *SessionNetworkDraft) { v.PolicyFingerprint = strings.Repeat("b", 64) },
		"project":           func(v *SessionNetworkDraft) { v.Project = t.TempDir() },
		"missing inputs":    func(v *SessionNetworkDraft) { v.Members[0].InputsID = strings.Repeat("c", 64) },
		"foreign inputs":    func(v *SessionNetworkDraft) { v.Members[0].InputsID = otherInputs },
		"exposed authority": func(v *SessionNetworkDraft) { v.Exposed = append(v.Exposed, store.Path()) },
		"duplicate member":  func(v *SessionNetworkDraft) { v.Members = append(v.Members, v.Members[0]) },
		"invalid owner":     func(v *SessionNetworkDraft) { v.SessionID = "../other" },
		"oversize plan": func(v *SessionNetworkDraft) {
			v.Members[0].Plan = json.RawMessage(`"` + strings.Repeat("x", 1<<20) + `"`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := draft
			changed.Exposed = append([]string(nil), draft.Exposed...)
			changed.Members = append([]SessionNetworkDraftMember(nil), draft.Members...)
			change(&changed)
			if id, err := store.RecordSessionNetworkDraft(changed); err == nil || id != "" {
				t.Fatalf("inexact draft was recorded: %q, %v", id, err)
			}
		})
	}
}

func TestSessionNetworkDraftEnvelopeDoesNotOpenSecretInputs(t *testing.T) {
	store, draft := sessionNetworkDraftFixture(t)
	id, err := store.RecordSessionNetworkDraft(draft)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(store.Path(), "inputs-"+draft.Members[0].InputsID+".env")); err != nil {
		t.Fatal(err)
	}
	if envelope, err := store.SessionNetworkDraftEnvelope(id); err != nil || envelope.ID != id {
		t.Fatal("envelope depended on secret inputs", err)
	}
	if loaded, err := store.LoadSessionNetworkDraft(id); err == nil || loaded.ID != "" {
		t.Fatal("load accepted missing immutable inputs", err)
	}
}

func TestSessionNetworkDraftRejectsTamperingUnknownFieldsAndOversize(t *testing.T) {
	for _, kind := range []string{"content", "unknown", "oversize"} {
		t.Run(kind, func(t *testing.T) {
			store, draft := sessionNetworkDraftFixture(t)
			id, err := store.RecordSessionNetworkDraft(draft)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(store.Path(), "session-draft-"+id+".json")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "content":
				data = bytes.Replace(data, []byte(`"session-owner"`), []byte(`"other-session"`), 1)
			case "unknown":
				data = append([]byte(`{"unexpected":true,`), data[1:]...)
			case "oversize":
				data = bytes.Repeat([]byte("x"), maxSessionNetworkDraftBytes+1)
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if got, err := store.SessionNetworkDraftEnvelope(id); err == nil || got.ID != "" {
				t.Fatalf("%s draft passed envelope validation", kind)
			}
			if got, err := store.LoadSessionNetworkDraft(id); err == nil || got.ID != "" {
				t.Fatalf("%s draft reached input validation", kind)
			}
		})
	}
}
