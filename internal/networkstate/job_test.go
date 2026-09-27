package networkstate

import (
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/egress"
)

func TestControllerJobSnapshotIsPrivateAndSessionBoundWithoutLocalApproval(t *testing.T) {
	store := openStore(t)
	project := t.TempDir()
	if err := approve(store, project, egress.Filtered, nil, nil); err != nil {
		t.Fatal(err)
	}
	withdraw(t, store, project)
	if pending := pendingAfter(t, store, project, Admission{}); pending == nil || !strings.Contains(pending.Reason, "withdrawn") {
		t.Fatalf("ordinary admission before controller capture = %v, want withdrawal", pending)
	}
	if _, err := store.Admit(project, Admission{}); err == nil {
		t.Fatal("ordinary admission crossed the withdrawal barrier")
	}
	ref := JobSnapshotRef{JobDigest: strings.Repeat("a", 64), SessionID: "remote_one"}
	rules := []egress.Rule{rule("controller.example.com")}
	snapshot, err := store.CaptureJob(ref, rules, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Mode != egress.Filtered || len(snapshot.Grants) != 1 ||
		!snapshot.Domain("controller.example.com", 443).Allowed || snapshot.Domain("repository.example.com", 443).Allowed {
		t.Fatalf("captured controller reach = %+v", snapshot)
	}
	if pending := pendingAfter(t, store, project, Admission{}); pending == nil || !strings.Contains(pending.Reason, "withdrawn") {
		t.Fatalf("ordinary admission after controller capture = %v, want withdrawal", pending)
	}
	if _, err := store.Admit(project, Admission{}); err == nil {
		t.Fatal("controller capture cleared the local withdrawal barrier")
	}
	loaded, err := store.LoadJobSnapshot(ref, snapshot.Fingerprint)
	if err != nil || loaded.Fingerprint != snapshot.Fingerprint {
		t.Fatalf("reload controller snapshot = %+v, %v", loaded, err)
	}
	for _, wrong := range []JobSnapshotRef{
		{JobDigest: strings.Repeat("b", 64), SessionID: ref.SessionID},
		{JobDigest: ref.JobDigest, SessionID: "remote_other"},
	} {
		if _, err := store.LoadJobSnapshot(wrong, snapshot.Fingerprint); err == nil {
			t.Fatalf("snapshot loaded under wrong job/session %+v", wrong)
		}
	}
	if _, err := store.LoadSnapshot(t.TempDir(), snapshot.Fingerprint); err == nil {
		t.Fatal("controller snapshot loaded as locally approved project snapshot")
	}
	if _, err := store.CaptureJob(JobSnapshotRef{JobDigest: "bad", SessionID: ref.SessionID}, rules, nil, false); err == nil {
		t.Fatal("invalid controller identity was admitted")
	}
}
