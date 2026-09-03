package box

import (
	"os"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The Docker E2E is deliberately outside `make check`; pin its blocking CI home so an otherwise
// harmless workflow cleanup cannot leave the review write boundary covered only by argv tests.
func TestReviewWritesE2EIsWiredInCI(t *testing.T) {
	b, err := os.ReadFile("../../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(b)
	for _, want := range []string{
		"permissions:\n  contents: read",
		"  review-writes:\n    runs-on: ubuntu-latest\n    timeout-minutes: 10",
		"run: make review-writes-e2e",
		"persist-credentials: false",
	} {
		if !strings.Contains(workflow, want) {
			t.Errorf("ci.yml no longer pins review-write E2E contract %q", want)
		}
	}
	if strings.Contains(workflow, "pull_request_target:") {
		t.Error("review-write E2E must not run untrusted pull requests with a privileged pull_request_target token")
	}
}

// Runtime-bound checks remain separate CI jobs, but their executable recipes still belong in
// Make. Pin that boundary so the doctor job cannot grow a second build-and-run recipe again.
func TestDoctorE2EIsWiredThroughMake(t *testing.T) {
	workflowBytes, err := os.ReadFile("../../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	type workflowStep struct {
		Run string            `yaml:"run"`
		Env map[string]string `yaml:"env"`
	}
	type workflowJob struct {
		Strategy struct {
			Matrix map[string][]string `yaml:"matrix"`
		} `yaml:"strategy"`
		Steps []workflowStep `yaml:"steps"`
	}
	var workflow struct {
		Jobs map[string]workflowJob `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(workflowBytes, &workflow); err != nil {
		t.Fatalf("decode ci.yml: %v", err)
	}
	doctorJob, ok := workflow.Jobs["doctor"]
	if !ok {
		t.Fatal("ci.yml has no doctor job")
	}
	if runtimes := doctorJob.Strategy.Matrix["runtime"]; !slices.Equal(runtimes, []string{"docker", "podman"}) {
		t.Errorf("ci doctor runtime matrix = %v, want [docker podman]", runtimes)
	}
	doctorRuns, initRuns := 0, 0
	for _, step := range doctorJob.Steps {
		run := strings.TrimSpace(step.Run)
		switch run {
		case "make doctor":
			doctorRuns++
			if step.Env["COOP_RUNTIME"] != "${{ matrix.runtime }}" {
				t.Errorf("make doctor COOP_RUNTIME = %q, want matrix.runtime", step.Env["COOP_RUNTIME"])
			}
		case "make box-runtime-e2e":
			initRuns++
		}
		for _, duplicate := range []string{"go build -o coop", "./coop doctor"} {
			if strings.Contains(run, duplicate) {
				t.Errorf("ci doctor job duplicates Make recipe %q", duplicate)
			}
		}
	}
	if doctorRuns != 1 || initRuns != 1 {
		t.Errorf("ci doctor commands = make doctor %d, make box-runtime-e2e %d; want 1 each", doctorRuns, initRuns)
	}

	makeBytes, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	makefile := string(makeBytes)
	if !strings.Contains(makefile, "doctor: build ##") || !strings.Contains(makefile, "\n\t@./coop doctor\n") {
		t.Error("Makefile doctor target must build and run the repository binary")
	}
}
