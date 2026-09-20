package eval

import (
	"strings"
	"testing"
	"time"
)

func loadAgent(t *testing.T) *Suite {
	t.Helper()
	s, err := Load(writeSuite(t, agentSuite, "verifiers/hello"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestBuildPlanCountsTheMatrixAndCapsJobs(t *testing.T) {
	s := loadAgent(t)
	configs := []Configuration{
		{Kind: ConfigTarget, Label: "codex"},
		{Kind: ConfigPreset, Label: "frontier"},
	}
	p, err := BuildPlan(s, configs, Options{Jobs: 8, Repeat: 3, Timeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	// 1 case x 2 configs x 3 repeats = 6 trials; 8 requested jobs cap to 6.
	if p.Trials() != 6 {
		t.Errorf("trials = %d, want 6", p.Trials())
	}
	if p.Jobs != 6 {
		t.Errorf("jobs = %d, want 6 (capped to the trial count)", p.Jobs)
	}
}

func TestBuildPlanRefusesEveryBadRequest(t *testing.T) {
	s := loadAgent(t)
	one := []Configuration{{Kind: ConfigTarget, Label: "codex"}}
	cases := []struct {
		name    string
		configs []Configuration
		opts    Options
		want    string
	}{
		{"no configs", nil, Options{Jobs: 1, Repeat: 1, Timeout: time.Hour}, "at least one target or preset"},
		{"zero repeat", one, Options{Jobs: 1, Repeat: 0, Timeout: time.Hour}, "repeat must be positive"},
		{"zero jobs", one, Options{Jobs: 0, Repeat: 1, Timeout: time.Hour}, "jobs must be positive"},
		{"no timeout", one, Options{Jobs: 1, Repeat: 1, Timeout: 0}, "positive --timeout"},
		{"timeout below a case", one, Options{Jobs: 1, Repeat: 1, Timeout: time.Minute}, "smaller than the longest case budget"},
		{"loop-config on agent suite", one, Options{Jobs: 1, Repeat: 1, Timeout: time.Hour, LoopConfigOverride: "./other.yaml"}, "only to a loop suite"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := BuildPlan(s, tc.configs, tc.opts)
			if err == nil {
				t.Fatal("bad plan request was accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err, tc.want)
			}
		})
	}
}

func TestBuildPlanTakesTheLoopConfigOverrideForALoopSuite(t *testing.T) {
	s, err := Load(writeSuite(t, loopSuite, "loop.yaml", "fixtures/app", "queues/repo-evolution", "verifiers/repo-evolution"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := BuildPlan(s, []Configuration{{Kind: ConfigPreset, Label: "frontier"}}, Options{
		Jobs: 2, Repeat: 1, Timeout: time.Hour, LoopConfigOverride: "./new-loop.yaml",
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.LoopConfig != "./new-loop.yaml" {
		t.Errorf("loop config = %q, want the override", p.LoopConfig)
	}
}
