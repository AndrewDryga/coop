package box

import (
	"os"
	"strings"
	"testing"
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
