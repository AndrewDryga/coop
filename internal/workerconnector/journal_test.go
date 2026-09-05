package workerconnector

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// A receipt is the worker's memory of a command; every poll decodes all of them, so one torn by a
// crash mid-write would stop polling for good. It must therefore appear whole or not at all.
func TestCommandReceiptIsPublishedWholeOrNotAtAll(t *testing.T) {
	journal, err := openJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	command := createCommand(time.Now().Add(time.Minute))
	entry, err := journal.begin(command)
	if err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(journal.dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, file := range files {
		names = append(names, file.Name())
	}
	if len(names) != 1 || !strings.HasSuffix(names[0], ".json") {
		t.Fatalf("receipt dir after begin = %v; want exactly the published receipt and no temp file", names)
	}
	entries, err := journal.pending()
	if err != nil || len(entries) != 1 || entries[0].CommandID != command.CommandID || entries[0].Result != nil {
		t.Fatalf("pending = %+v, %v; want the received receipt only", entries, err)
	}
	again, err := journal.begin(command)
	if err != nil || again.CommandDigest != entry.CommandDigest || again.State != "received" {
		t.Fatalf("second begin = %+v, %v; want the same receipt adopted", again, err)
	}
	changed := command
	changed.Payload = json.RawMessage(`{"external_ref":"other","policy":"writable","policy_digest":"` + repeatedDigest("c") + `"}`)
	if _, err := journal.begin(changed); !errors.Is(err, ErrCommandConflict) {
		t.Fatalf("changed payload under one id = %v, want ErrCommandConflict", err)
	}
	if _, err := os.Stat(journal.path(command.CommandID)); err != nil {
		t.Fatalf("published receipt: %v", err)
	}
}
