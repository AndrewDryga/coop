package workerconnector

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// A record passes the check exactly when a connector of this build accepts it. Rejected records
// are sorted by the version they declare, so a newer build's records read as a refusal and a torn
// one as damage, never the other way round.
func TestCheckJournalSortsRecordsByWhatThisBuildDoesWithThem(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "connector")
	journal, err := openJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	command := createCommand(time.Now().Add(time.Minute))
	if _, err := journal.begin(command); err != nil {
		t.Fatal(err)
	}
	if err := journal.bindEventStream(command, "coop-session-1"); err != nil {
		t.Fatal(err)
	}
	origin := createOrigin{
		Version: createOriginVersion, WorkerID: command.WorkerID, SessionRef: command.SessionRef,
		PlacementGeneration: 1, CommandID: command.CommandID, CommandDigest: repeatedDigest("a"),
		OperationKey: command.IdempotencyKey, OperationID: "op_1", State: "pending",
	}
	if err := journal.writeActivityRecord(journal.createOriginPath(origin.SessionRef), origin); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		filepath.Join(journal.dir, "newer.json"):     `{"version":2,"command_id":"from-a-newer-build"}`,
		filepath.Join(journal.dir, "never.json"):     `{"version":0}`, // never a format: damaged, not old
		filepath.Join(journal.dir, "receipt.body"):   `not a record`,
		filepath.Join(journal.streams, "torn.json"):  `{"version":1,"session_ref":`,
		filepath.Join(journal.origins, "bare.json"):  `{}`,
		filepath.Join(journal.streams, ".scan-temp"): `{"version":9}`,
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	checks, err := CheckJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []RecordCheck{
		{Noun: "worker command receipt", Version: journalVersion, Current: 1, Newer: 1,
			Damaged: []string{"never.json: worker command receipt is malformed"}},
		{Noun: "worker event stream", Version: eventStreamVersion, Current: 1,
			Damaged: []string{"torn.json: decode worker event stream: unexpected end of JSON input"}},
		{Noun: "worker create origin", Version: createOriginVersion, Current: 1,
			Damaged: []string{"bare.json: worker create origin is malformed"}},
	}
	if !slices.EqualFunc(checks, want, func(a, b RecordCheck) bool {
		return a.Noun == b.Noun && a.Version == b.Version && a.Current == b.Current &&
			a.Newer == b.Newer && a.Older == b.Older && slices.Equal(a.Damaged, b.Damaged)
	}) {
		t.Fatalf("checks = %+v\nwant %+v", checks, want)
	}
}

// A state root whose connector never ran has no journal, and the check must not create one.
func TestCheckJournalLeavesAMissingJournalMissing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "connector")
	checks, err := CheckJournal(dir)
	if err != nil || len(checks) != 3 {
		t.Fatalf("checks = %+v, %v", checks, err)
	}
	for _, check := range checks {
		if check.Current+check.Newer+check.Older+len(check.Damaged) != 0 {
			t.Fatalf("missing journal reported records: %+v", check)
		}
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the check created the journal directory: %v", err)
	}
}
