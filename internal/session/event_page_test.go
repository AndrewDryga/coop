package session

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// An event page used to read its whole row limit from SQLite and then drop what did not fit in
// its byte budget, reading the same rows again on the next page. The store now stops at the
// budget, so a page of 64 KiB events reads the 16 that fit in 1 MiB plus the one that did not.
func TestEventPageStopsReadingRowsAtItsByteBudget(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "state"))
	sess, err := store.CreateSession(ctx, "event-page", CreateSessionRequest{JobDocument: testJobDocument, JobDigest: testJobDigest, Target: "codex:model"})
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"text":"` + strings.Repeat("a", 64<<10-len(`{"text":""}`)) + `"}`)
	for range 100 {
		if _, err := store.AppendEvent(ctx, AppendEventRequest{SessionID: sess.ID, Type: EventModelThought, Version: 1, Payload: payload}); err != nil {
			t.Fatal(err)
		}
	}
	all, err := store.ListEvents(ctx, sess.ID, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	scanned := 0
	store.eventRowScanned = func() { scanned++ }
	page, err := store.ListEventPage(ctx, sess.ID, 0, 1000, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 16 || scanned != 17 {
		t.Fatalf("page of %d events read %d rows; want 16 events from 17 rows", len(page), scanned)
	}
	for i := range page {
		if page[i].ID != all[i].ID || string(page[i].Payload) != string(all[i].Payload) {
			t.Fatalf("page event %d changed", i)
		}
	}
	// A payload larger than the whole budget still crosses alone, so the cursor moves.
	scanned = 0
	page, err = store.ListEventPage(ctx, sess.ID, all[0].Sequence, 1000, 1)
	if err != nil || len(page) != 1 || page[0].ID != all[1].ID || scanned != 2 {
		t.Fatalf("oversized first event = %d events, %v, read %d rows", len(page), err, scanned)
	}
}
