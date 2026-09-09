package networkstate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestQualificationCannotRerunItsSmokeOrReplaceObservations(t *testing.T) {
	smoke, spec := qualificationFixture(t)
	if got, err := smoke.CreateExecution(context.Background(), spec); err == nil || got.ID != "" {
		t.Fatal("a preflight cherry-picked a repeated smoke attempt")
	}
	if _, err := smoke.RecordEvidence(smoke.runID, []byte(`{"changed":true}`)); err == nil {
		t.Fatal("completed observations replaced")
	}
	if err := smoke.store.root.Remove("qualification-evidence-" + smoke.runID + ".json"); err != nil {
		t.Fatal(err)
	}
	if got, err := smoke.Complete(smokeDomain); err == nil || got.ID != "" {
		t.Fatal("absent observation artifact qualified")
	}
}

func TestQualificationLossBlocksStartsNotInspectionOrCleanup(t *testing.T) {
	for _, lost := range []string{"qualification", "contract"} {
		t.Run(lost, func(t *testing.T) {
			smoke, spec := qualificationFixture(t)
			q, err := smoke.Complete(smokeDomain)
			if err != nil {
				t.Fatal(err)
			}
			spec.QualificationID = q.ID
			r, err := smoke.store.CreateExecution(context.Background(), spec)
			if err != nil {
				t.Fatal(err)
			}
			r = prepareFixtureLaunch(t, smoke.store, r)
			switch lost {
			case "qualification":
				err = smoke.store.root.Remove("qualification-" + q.ID + ".json")
			case "contract":
				r.QualificationContract = "historical-contract"
				data, _ := json.Marshal(r)
				err = smoke.store.publish("execution-"+r.ID+".json", data, true)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := smoke.store.BeginResourceCreation(context.Background(), r.ID, r.Revision, "controller"); err == nil {
				t.Fatal("missing qualification authorized new runtime work")
			}
			evidence, err := OpenEvidence(smoke.store.Path(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer evidence.Close()
			if _, err := evidence.Execution(r.ID); err != nil {
				t.Fatal("authority loss hid exact custody", err)
			}
			resource := r.Resources[0]
			if _, err := evidence.ConfirmResourceGone(context.Background(), r.ID, r.Revision, r.DaemonID, resource.Role, resource.Name, resource.ID); err != nil {
				t.Fatal("authority loss blocked cleanup", err)
			}
		})
	}
}

func TestExecutionInspectionRetainsGoodRecordsAndDoesNotInventPartialCursor(t *testing.T) {
	s, r := executionFixture(t)
	for _, name := range []string{"execution-invalid.json", "execution-" + strings.Repeat("f", 32) + ".json"} {
		if err := s.publish(name, []byte(`{broken`), false); err != nil {
			t.Fatal(err)
		}
	}
	evidence, err := OpenEvidence(s.Path(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer evidence.Close()
	page, err := evidence.Executions("")
	if err != nil || !page.Incomplete || page.Unreadable != 2 || len(page.Executions) != 1 || page.Executions[0].ID != r.ID {
		t.Fatal("one bad record hid good custody", page, err)
	}
	// Cheap filler entries exercise the inventory bound, not 10,000 executions.
	for i := range 10001 {
		file, err := s.root.OpenFile(fmt.Sprintf("inventory-fixture-%05d", i), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	page, err = evidence.Executions("")
	if err != nil || !page.Incomplete || page.Next != "" {
		t.Fatal("partial directory inventory invented a lexical cursor", page, err)
	}
}

func TestExecutionAmbiguousIntentCannotAuthorizeAnotherSubmission(t *testing.T) {
	s, r := executionFixture(t)
	r = prepareFixtureLaunch(t, s, r)
	r, err := s.BeginResourceCreation(context.Background(), r.ID, r.Revision, "controller")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginResourceCreation(context.Background(), r.ID, r.Revision, "controller"); err == nil {
		t.Fatal("unknown create outcome authorized a repeated external submission")
	}
}
