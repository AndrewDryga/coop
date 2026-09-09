package networkstate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionCatalogOwnerPrecedesPolicyAndInputs(t *testing.T) {
	f := newCatalogFixture(t)
	owner := SessionCatalogOwner{SessionID: "session", OperationID: "operation", ForkName: "fixture",
		ForkGeneration: strings.Repeat("a", 32), AuthorityDigest: strings.Repeat("b", 64)}
	record := f.record
	record.Session = &owner
	record.ReferenceDigest = strings.Repeat("c", 64)
	inputs, err := f.store.RecordInputs(LaunchInputs{Environment: []byte("CAPTURED=value\n")})
	if err != nil {
		t.Fatal(err)
	}
	record.Members[0].InputsID = inputs
	id, err := f.store.RecordCatalog(record)
	if err != nil || id == f.id {
		t.Fatal("session identity was not bound into catalog", err)
	}
	if got, err := f.store.SessionCatalog(id, owner); err != nil || got.Session == nil || *got.Session != owner {
		t.Fatal("exact session owner could not reopen", err)
	}
	if err := os.Remove(filepath.Join(f.dir, "snapshot-"+f.record.Fingerprint+".json")); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*SessionCatalogOwner){
		func(o *SessionCatalogOwner) { o.SessionID = "other" },
		func(o *SessionCatalogOwner) { o.OperationID = "other" },
		func(o *SessionCatalogOwner) { o.ForkName = "other" },
		func(o *SessionCatalogOwner) { o.ForkGeneration = strings.Repeat("c", 32) },
		func(o *SessionCatalogOwner) { o.AuthorityDigest = strings.Repeat("c", 64) },
	} {
		changed := owner
		mutate(&changed)
		if _, err := f.store.SessionCatalog(id, changed); err == nil || err.Error() != "network catalog belongs to another session owner" {
			t.Fatal("wrong lifecycle owner reached missing private inputs", err)
		}
	}
}

func TestSessionCatalogOwnerValidation(t *testing.T) {
	base := SessionCatalogOwner{SessionID: "session", OperationID: "operation", ForkName: "fork",
		ForkGeneration: strings.Repeat("a", 32), AuthorityDigest: strings.Repeat("b", 64)}
	for _, mutate := range []func(*SessionCatalogOwner){
		func(o *SessionCatalogOwner) { o.SessionID = "../escape" },
		func(o *SessionCatalogOwner) { o.OperationID = "has space" },
		func(o *SessionCatalogOwner) { o.ForkName = "." },
		func(o *SessionCatalogOwner) { o.ForkGeneration = strings.Repeat("A", 32) },
		func(o *SessionCatalogOwner) { o.AuthorityDigest = "" },
	} {
		changed := base
		mutate(&changed)
		if err := validateSessionCatalogOwner(changed); err == nil {
			t.Fatal("invalid durable session identity accepted")
		}
	}
}
