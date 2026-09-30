package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/eval"
)

func profiledTrialSuite(t *testing.T) *eval.Suite {
	t.Helper()
	s := trialSuite(t)
	profile, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(profile, ".agent"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, ".agent", "Dockerfile"), []byte("ARG COOP_BASE_IMAGE\nFROM ${COOP_BASE_IMAGE}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.Cases[0].Runtime = &eval.CaseRuntime{Profile: profile, Workdir: "/app", AgentTimeout: eval.Duration(time.Minute), VerifierTimeout: eval.Duration(time.Minute)}
	staged, err := eval.StageSuite(context.Background(), t.TempDir(), s)
	if err != nil {
		t.Fatal(err)
	}
	staged.Cases[0].Runtime.ImageID, staged.Cases[0].Runtime.Platform = "sha256:"+strings.Repeat("a", 64), "linux/arm64"
	return staged
}

func TestProfiledTrialExecutionContract(t *testing.T) {
	suite := profiledTrialSuite(t)
	p := suite.Cases[0].Runtime
	trial := trialFor(suite)
	calls := 0
	r := newTrialRunner(t, suite, func(spec box.RunSpec) (int, error) {
		calls++
		if spec.Image != p.ImageID || spec.ExpectedImageID != p.ImageID || spec.Workdir != "/app" || spec.Cache {
			t.Fatalf("wrong execution identity: %+v", spec)
		}
		deadline, ok := spec.Ctx.Deadline()
		if !ok || time.Until(deadline) > time.Minute {
			t.Fatal("phase budget absent or reset")
		}
		if spec.Agent != "" {
			if spec.PolicyRepo != p.Profile || !spec.AgentCommand || spec.CapturedEgress == nil || len(spec.ExtraArgs) != 0 {
				t.Fatal("candidate gained hidden inputs or lost original authority")
			}
			if _, err := os.Stat(filepath.Join(spec.Repo, "expected.txt")); !os.IsNotExist(err) {
				t.Fatal("hidden verifier reached candidate")
			}
			if err := os.MkdirAll(filepath.Join(spec.Repo, ".agent"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(spec.Repo, ".agent", "project.yaml"), []byte("box:\n  egress: open\n  env:\n    PATH: /app/evil\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return 0, os.WriteFile(filepath.Join(spec.Repo, "answer.txt"), []byte("hello\n"), 0o644)
		}
		entries, err := os.ReadDir(spec.PolicyRepo)
		if err != nil || len(entries) != 0 || spec.PolicyRepo == p.Profile || !spec.GradeSnapshot || spec.Homes || spec.Network || spec.CapturedEgress != nil {
			t.Fatal("grader trusted candidate or profile execution policy")
		}
		if len(spec.ExtraArgs) != 2 || !strings.HasSuffix(spec.ExtraArgs[1], ":/coop-verifier:ro") {
			t.Fatal("hidden grader mount changed")
		}
		return 0, nil
	})
	r.profiles = evalProfileCaptures{{p.Profile, trial.Config.Label}: &box.CapturedEgress{Project: p.Profile}}
	if result := r.run(context.Background(), trial); result.Status != eval.TrialPassed || calls != 2 {
		t.Fatalf("result=%+v calls=%d", result, calls)
	}
	if entries, err := os.ReadDir(r.workRoot); err != nil || len(entries) != 0 {
		t.Fatal("passed trial retained workspace")
	}
}

func TestProfiledTrialRefusesDriftAndPhaseExpiry(t *testing.T) {
	for _, kind := range []string{"unresolved", "changed before candidate", "changed before grader", "agent timeout", "verifier timeout", "outer cancellation"} {
		t.Run(kind, func(t *testing.T) {
			suite := profiledTrialSuite(t)
			p := suite.Cases[0].Runtime
			trial := trialFor(suite)
			calls := 0
			r := newTrialRunner(t, suite, func(spec box.RunSpec) (int, error) {
				calls++
				if kind == "changed before grader" {
					return 0, os.WriteFile(filepath.Join(p.Profile, ".agent", "Dockerfile"), []byte("changed\n"), 0o644)
				}
				return 0, nil
			})
			r.profiles = evalProfileCaptures{{p.Profile, trial.Config.Label}: &box.CapturedEgress{Project: p.Profile}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want, wantCalls := eval.TrialError, 0
			switch kind {
			case "unresolved":
				r.profiles = nil
			case "changed before candidate":
				if err := os.WriteFile(filepath.Join(p.Profile, ".agent", "Dockerfile"), []byte("changed\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "changed before grader":
				wantCalls = 1
			case "agent timeout":
				p.AgentTimeout = eval.Duration(time.Nanosecond)
				want = eval.TrialTimedOut
			case "verifier timeout":
				p.VerifierTimeout = eval.Duration(time.Nanosecond)
				want, wantCalls = eval.TrialTimedOut, 1
			case "outer cancellation":
				cancel()
				want = eval.TrialTimedOut
			}
			if result := r.run(ctx, trial); result.Status != want || calls != wantCalls {
				t.Fatalf("result=%+v calls=%d want=%s/%d", result, calls, want, wantCalls)
			}
		})
	}
}

func TestEvalProfileFixedConfigurationAndProviderOnlyAuthority(t *testing.T) {
	base := &config.Config{CPUs: "8", Memory: "16g", Pids: "4096", AutoUp: true, Network: true, Cache: true, MCPFile: "operator.json", ExtraRunArgs: []string{"--privileged"}}
	cfg := evalProfileConfig(base)
	if cfg.CPUs != "1" || cfg.Memory != "2g" || cfg.Pids != "128" || !cfg.NoNewPrivileges || cfg.AutoUp || cfg.Network || cfg.Cache || cfg.MCPFile != "" || len(cfg.ExtraRunArgs) != 0 {
		t.Fatal("ambient configuration changed fixed protocol")
	}
	for _, key := range []string{"COOP_CPUS", "COOP_MEMORY", "COOP_PIDS"} {
		if !cfg.Explicit(key) {
			t.Fatal("profile cap can be overridden")
		}
	}
	if base.CPUs != "8" || base.MCPFile != "operator.json" {
		t.Fatal("operator configuration mutated")
	}
	for _, kind := range []string{"provider", "operator", "project", "feature", "other provider", "missing origin", "open"} {
		p := egress.Snapshot{Mode: egress.Filtered, Grants: []egress.Grant{{Origins: []egress.Origin{{Kind: "provider", Provider: "codex"}}}}}
		switch kind {
		case "operator", "project":
			p.Grants[0].Origins[0].Kind = kind
		case "feature":
			p.Grants[0].Origins[0].Feature = "search"
		case "other provider":
			p.Grants[0].Origins[0].Provider = "gemini"
		case "missing origin":
			p.Grants[0].Origins = nil
		case "open":
			p.Mode = egress.Open
		}
		if err := validateEvalProviderOnly(p, "codex"); (err == nil) != (kind == "provider") {
			t.Fatalf("%s: %v", kind, err)
		}
	}
}

func TestEvalComparisonDisclosesProfileAdaptationOnMismatch(t *testing.T) {
	runtime := eval.RunRuntime{Case: "ordinary-name", Protocol: eval.ProfileProtocol, ImageID: "sha256:frozen", Platform: "linux/arm64", Workdir: "/app", CPUs: 1, MemoryBytes: 2 << 30, PIDs: 128, DeclaredStorageBytes: 10 << 30}
	out := captureStdout(t, func() {
		renderEvalComparison(&eval.Comparison{Suite: "ordinary-name", Mismatch: "different workloads", Base: eval.ConfigOutcome{Runtimes: []eval.RunRuntime{runtime}}, New: eval.ConfigOutcome{Runtimes: []eval.RunRuntime{runtime}}})
	})
	normalized := strings.Join(strings.Fields(out), " ") // human output wraps at the terminal width
	if strings.Count(normalized, "storage limit unenforced and usage unmeasured") != 2 || !strings.Contains(out, eval.ProfileProtocol) {
		t.Fatalf("adaptation hidden: %s", out)
	}
}

func TestEvalProfilePolicyRequiresDedicatedExplicitBuildInputs(t *testing.T) {
	for _, kind := range []string{"valid", "missing Dockerfile", "environment", "compose", "auto up", "network", "ports", "rules"} {
		t.Run(kind, func(t *testing.T) {
			suite := profiledTrialSuite(t)
			profile := suite.Cases[0].Runtime.Profile
			var body string
			switch kind {
			case "missing Dockerfile":
				if err := os.Remove(filepath.Join(profile, ".agent", "Dockerfile")); err != nil {
					t.Fatal(err)
				}
			case "environment":
				body = "box:\n  env:\n    PATH: /evil\n"
			case "compose":
				body = "box:\n  compose: compose.yml\n"
			case "auto up":
				body = "box:\n  auto_up: true\n"
			case "network":
				body = "box:\n  network: true\n"
			case "ports":
				body = "serve:\n  ports: [8080]\n"
			case "rules":
				body = "box:\n  egress_rules:\n    - to: {domain: example.com}\n"
			}
			if body != "" {
				if err := os.WriteFile(filepath.Join(profile, ".agent", "project.yaml"), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := validateEvalProfilePolicy(profile); (err == nil) != (kind == "valid") {
				t.Fatalf("policy=%v for %s", err, kind)
			}
		})
	}
}
