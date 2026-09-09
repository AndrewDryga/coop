package networkstate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/egress"
)

func TestQualificationMCPRequiresProjectionBeforeReservation(t *testing.T) {
	client := QualifiedClient{Dependency: egress.Dependency{Provider: "anthropic", Client: egress.ClientCLI, Backend: "direct", AuthMode: "api-key", Version: "2026-09-08"}, MCPProjection: "none"}
	trial, spec, _ := qualificationFixture(t, []QualifiedClient{client})
	if got, err := trial.CreateExecution(context.Background(), spec, "mcp", &client); err == nil || got.ID != "" {
		t.Fatal("MCP trial without an MCP projection accepted", err)
	}
	if _, err := trial.store.root.Stat(trial.caseFile("mcp", &client)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("invalid MCP trial consumed a case reservation", err)
	}
}

func TestQualificationCannotRetryCaseOrReplaceObservations(t *testing.T) {
	trial, spec, proofs := qualificationFixture(t, nil)
	if got, err := trial.CreateExecution(context.Background(), spec, "enforcement", nil); err == nil || got.ID != "" {
		t.Fatal("one group can cherry-pick repeated case attempts")
	}
	if _, err := trial.RecordEvidence(proofs[0].RunID, []byte(`{"changed":true}`)); err == nil {
		t.Fatal("completed observations replaced")
	}
	if err := trial.store.root.Remove("qualification-evidence-" + proofs[0].RunID + ".json"); err != nil {
		t.Fatal(err)
	}
	if got, err := trial.Complete(nil, proofs); err == nil || got.ID != "" {
		t.Fatal("absent observation artifact qualified")
	}
}

func TestQualificationCannotInventProviderPolicyOrResumeRelationship(t *testing.T) {
	client := QualifiedClient{Dependency: egress.Dependency{Provider: "anthropic", Client: egress.ClientCLI, Backend: "direct", AuthMode: "api-key", Version: "2026-09-08"}, MCPProjection: "none"}
	trial := executionTrial(t, openStore(t))
	if got, err := trial.CreateExecution(context.Background(), qualificationExecutionSpec(t, trial), "provider-start", &client); err == nil || got.ID != "" {
		t.Fatal("trial declared a provider absent from its policy")
	}
	trial, _, proofs := qualificationFixture(t, []QualifiedClient{client})
	for i := range proofs {
		if proofs[i].Case == "provider-resume" {
			proofs[i].ResumesRunID = strings.Repeat("f", 32)
		}
	}
	if got, err := trial.Complete([]QualifiedClient{client}, proofs); err == nil || got.ID != "" {
		t.Fatal("unrelated session qualified as resume")
	}
}

func TestQualificationLossBlocksStartsNotInspectionOrCleanup(t *testing.T) {
	for _, lost := range []string{"qualification", "contract"} {
		t.Run(lost, func(t *testing.T) {
			trial, spec, proofs := qualificationFixture(t, nil)
			q, err := trial.Complete(nil, proofs)
			if err != nil {
				t.Fatal(err)
			}
			spec.QualificationID = q.ID
			r, err := trial.store.CreateExecution(context.Background(), spec)
			if err != nil {
				t.Fatal(err)
			}
			r = prepareFixtureLaunch(t, trial.store, r)
			switch lost {
			case "qualification":
				err = trial.store.root.Remove("qualification-" + q.ID + ".json")
			case "contract":
				r.QualificationContract = "historical-contract"
				data, _ := json.Marshal(r)
				err = trial.store.publish("execution-"+r.ID+".json", data, true)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := trial.store.BeginResourceCreation(context.Background(), r.ID, r.Revision, "controller"); err == nil {
				t.Fatal("missing qualification authorized new runtime work")
			}
			evidence, err := OpenEvidence(trial.store.Path(), nil)
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
