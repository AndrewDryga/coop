package workerproto

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/teststate"
)

func TestMain(m *testing.M) { os.Exit(teststate.Isolate(m.Run)) }

// A worker must resume the recipe that was admitted even if the operator
// edits the role prompt after a review has been submitted.
func TestJobDigestBindsFrozenReviewPreset(t *testing.T) {
	base, err := json.Marshal(validJobSpec())
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(base, &doc); err != nil {
		t.Fatal(err)
	}
	first := json.RawMessage(`{"version":1,"name":"review","lead_prompt":"Read the change.","roles":[{"name":"reader","mode":"consult","targets":["claude:opus@default"],"when":["code-review"],"prompt":"Check the requested behavior."}]}`)
	decode := func(recipe json.RawMessage) (JobSpec, error) {
		doc["preset"] = recipe
		data, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		return DecodeJobSpec(data)
	}
	job, err := decode(first)
	if err != nil {
		t.Fatalf("valid frozen review preset: %v", err)
	}
	digest, err := job.Digest()
	if err != nil {
		t.Fatal(err)
	}
	var updated map[string]any
	if err := json.Unmarshal(first, &updated); err != nil {
		t.Fatal(err)
	}
	role := updated["roles"].([]any)[0].(map[string]any)
	role["prompt"] = "Check behavior and edge cases."
	changed, _ := json.Marshal(updated)
	second, err := decode(changed)
	if err != nil {
		t.Fatal(err)
	}
	other, err := second.Digest()
	if err != nil || digest == other {
		t.Fatalf("role prompt did not change job digest: %v", err)
	}
	role["prompt_path"] = ".agent/presets/review/reader.md"
	untrusted, _ := json.Marshal(updated)
	if _, err := decode(untrusted); err == nil {
		t.Fatal("worker accepted a path in the checked-out pull request")
	}
}

// A box has one active login per provider. A review role must never silently
// run as the lead or another reviewer's account when the recipe says otherwise.
func TestFrozenRolesCannotSubstituteTheirRequestedAccount(t *testing.T) {
	base := JobPreset{Version: 1, Name: "review", Roles: []JobPresetRole{
		{Name: "reviewer", Mode: "consult", Targets: []string{"claude:opus@review"}, When: []string{}},
		{Name: "second", Mode: "consult", Targets: []string{"claude:opus@other"}, When: []string{}},
	}}
	for _, leader := range []string{"codex:gpt-5@lead", "claude:opus@lead"} {
		if _, err := base.Recipe([]string{leader}, true); err == nil {
			t.Fatalf("accepted different logins for one provider with lead %s", leader)
		}
	}
	base.Roles[1].Targets = base.Roles[0].Targets
	if _, err := base.Recipe([]string{"codex:gpt-5@lead", "codex:gpt-5@fallback"}, true); err != nil {
		t.Fatalf("a lead may rotate between accounts when peer roles use a different provider: %v", err)
	}
}

func TestSharedJobPresetFixtureHasTheControllerDigest(t *testing.T) {
	data, err := os.ReadFile("testdata/job_preset_v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Job    json.RawMessage `json:"job"`
		Digest string          `json:"digest"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	job, err := DecodeJobSpec(fixture.Job)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := job.Digest()
	if err != nil || digest != fixture.Digest {
		t.Fatalf("shared preset digest = %s, want %s: %v", digest, fixture.Digest, err)
	}
}
