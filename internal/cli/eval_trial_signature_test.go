package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/eval"
)

func TestTrialRunnerGradesSameSizeEditAfterNonZeroExit(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	suite := trialSuite(t)
	graded := false
	r := newTrialRunner(t, suite, func(spec box.RunSpec) (int, error) {
		if spec.Agent != "" {
			return 1, os.WriteFile(filepath.Join(spec.Repo, "README.md"), []byte("# fixed here\n"), 0o644)
		}
		graded = true
		body, err := os.ReadFile(filepath.Join(spec.Repo, "README.md"))
		if err != nil || string(body) != "# fixed here\n" {
			t.Fatalf("grader did not receive changed snapshot: %q, %v", body, err)
		}
		return 0, nil
	})
	res := r.run(context.Background(), trialFor(suite))
	if !graded || res.Status != eval.TrialPassed {
		t.Fatalf("graded=%v status=%s detail=%s", graded, res.Status, res.Detail)
	}
}
