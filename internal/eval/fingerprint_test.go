package eval

import (
	"strings"
	"testing"
)

func TestWorkloadFingerprintChangesWithEveryWorkloadEdit(t *testing.T) {
	base := loadAgent(t)
	fp := WorkloadFingerprint(base)
	old := newHasher().text("schema", "eval.workload.v2").text("version", "1").text("runner", string(base.Runner)).text("content", string(base.ContentDigest))
	for _, c := range base.Cases {
		old.text("case.id", c.ID).text("case.timeout", c.Timeout.String()).text("case.verifier", c.Verifier).
			text("case.instruction", c.Instruction).text("case.files", c.Files).text("case.fixture", c.Fixture).text("case.tasks", c.Tasks)
	}
	if fp == old.sum() {
		t.Fatal("new web-tool protocol shares the previous workload identity")
	}

	// Stable: the same parsed suite hashes the same every time.
	if WorkloadFingerprint(base) != fp {
		t.Fatal("workload fingerprint is not stable for identical input")
	}

	// A comment-only edit is NOT a workload change: the parsed fields are identical, so pooling the
	// two runs' scores is still honest.
	commented, err := Load(writeSuite(t, "# a comment\n"+agentSuite, "verifiers/hello"))
	if err != nil {
		t.Fatal(err)
	}
	if WorkloadFingerprint(commented) != fp {
		t.Error("a comment-only edit changed the workload fingerprint — it should not")
	}

	// Each of these edits is a real workload change and must change the fingerprint.
	for _, edit := range []struct {
		name string
		from string
		to   string
	}{
		{"instruction", "print hello", "print goodbye"},
		{"timeout", "timeout: 5m", "timeout: 6m"},
		{"verifier", "./verifiers/hello", "./verifiers/other"},
		{"case id", "id: hello", "id: goodbye"},
	} {
		t.Run(edit.name, func(t *testing.T) {
			manifest := strings.Replace(agentSuite, edit.from, edit.to, 1)
			edited, err := Load(writeSuite(t, manifest, "verifiers/hello", "verifiers/other"))
			if err != nil {
				t.Fatal(err)
			}
			if WorkloadFingerprint(edited) == fp {
				t.Errorf("editing the %s did not change the workload fingerprint", edit.name)
			}
		})
	}
}

// loop_config is a configuration change, not a workload one — two runs of the same cases under
// different loop recipes must still share a workload fingerprint so their scores can be compared.
func TestWorkloadFingerprintIgnoresLoopConfig(t *testing.T) {
	a, err := Load(writeSuite(t, loopSuite, "loop.yaml", "fixtures/app", "queues/repo-evolution", "verifiers/repo-evolution"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Load(writeSuite(t, strings.Replace(loopSuite, "./loop.yaml", "./other.yaml", 1),
		"other.yaml", "fixtures/app", "queues/repo-evolution", "verifiers/repo-evolution"))
	if err != nil {
		t.Fatal(err)
	}
	if WorkloadFingerprint(a) != WorkloadFingerprint(b) {
		t.Error("a loop_config change altered the WORKLOAD fingerprint; it belongs to the configuration")
	}
}
