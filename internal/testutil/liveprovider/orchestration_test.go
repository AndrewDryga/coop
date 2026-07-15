package liveprovider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/testutil/procharness"
)

func TestReadChildResultAcceptsEveryConsistentStatus(t *testing.T) {
	results := []ProviderResult{
		{Provider: "codex", CLIVersion: "codex-cli 1.2.3", Attempted: true, Passed: true, Status: StatusPassed},
		{Provider: "codex", CLIVersion: "codex-cli 1.2.3", Status: StatusSkipped, ReasonCode: ReasonMissingCredential},
		{Provider: "codex", Attempted: true, Status: StatusFailed, ReasonCode: ReasonPromptExit, Phase: "prompt", ErrorClass: "process"},
	}
	for _, want := range results {
		layout, err := procharness.NewLayout(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(layout.State, "result.json")
		writeChildResult(t, path, want)
		got, err := ReadChildResult(layout.Root, path, "codex")
		if err != nil || got != want {
			t.Errorf("ReadChildResult = %+v, %v; want %+v", got, err, want)
		}
	}
}

func TestReadChildResultRejectsUnsafeOrInconsistentFiles(t *testing.T) {
	valid := ProviderResult{Provider: "codex", CLIVersion: "codex-cli 1.2.3", Attempted: true, Passed: true, Status: StatusPassed}
	tests := []struct {
		name  string
		write func(t *testing.T, layout procharness.Layout, path string)
	}{
		{name: "missing", write: func(*testing.T, procharness.Layout, string) {}},
		{name: "directory", write: func(t *testing.T, _ procharness.Layout, path string) { mustMkdir(t, path) }},
		{name: "empty", write: func(t *testing.T, _ procharness.Layout, path string) { writeRawResult(t, path, nil) }},
		{name: "oversized", write: func(t *testing.T, _ procharness.Layout, path string) {
			writeRawResult(t, path, []byte(strings.Repeat(" ", int(childResultLimit)+1)))
		}},
		{name: "malformed", write: func(t *testing.T, _ procharness.Layout, path string) { writeRawResult(t, path, []byte(`{`)) }},
		{name: "unknown field", write: func(t *testing.T, _ procharness.Layout, path string) {
			writeRawResult(t, path, []byte(`{"provider":"codex","status":"skipped","reason_code":"missing_cli","unknown":true}`))
		}},
		{name: "second value", write: func(t *testing.T, _ procharness.Layout, path string) {
			writeRawResult(t, path, []byte(`{"provider":"codex","status":"skipped","reason_code":"missing_cli"}{}`))
		}},
		{name: "provider mismatch", write: func(t *testing.T, _ procharness.Layout, path string) {
			result := valid
			result.Provider = "claude"
			writeChildResult(t, path, result)
		}},
		{name: "inconsistent", write: func(t *testing.T, _ procharness.Layout, path string) {
			result := valid
			result.Attempted = false
			writeChildResult(t, path, result)
		}},
		{name: "unsafe version", write: func(t *testing.T, _ procharness.Layout, path string) {
			result := valid
			result.CLIVersion = "/private/TOKEN_CANARY 1.2.3"
			writeChildResult(t, path, result)
		}},
		{name: "symlink", write: func(t *testing.T, layout procharness.Layout, path string) {
			target := filepath.Join(layout.State, "target.json")
			writeChildResult(t, target, valid)
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "hardlink", write: func(t *testing.T, layout procharness.Layout, path string) {
			target := filepath.Join(layout.State, "target.json")
			writeChildResult(t, target, valid)
			if err := os.Link(target, path); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			layout, err := procharness.NewLayout(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(layout.State, "result.json")
			tt.write(t, layout, path)
			if result, err := ReadChildResult(layout.Root, path, "codex"); err == nil {
				t.Fatalf("unsafe result accepted: %+v", result)
			}
		})
	}
}

func TestReadChildResultAcceptsExactSizeLimitAndControlPresenceIsStrict(t *testing.T) {
	layout, err := procharness.NewLayout(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(layout.State, "result.json")
	result := ProviderResult{Provider: "codex", Status: StatusSkipped, ReasonCode: ReasonMissingCLI}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, []byte(strings.Repeat(" ", int(childResultLimit)-len(data)))...)
	writeRawResult(t, path, data)
	if _, err := ReadChildResult(layout.Root, path, "codex"); err != nil {
		t.Fatalf("exact-size child result rejected: %v", err)
	}
	if !ControlFilePresent(layout.Root, path) {
		t.Fatal("valid control file not detected")
	}
	hardlink := filepath.Join(layout.State, "attempt")
	if err := os.Link(path, hardlink); err != nil {
		t.Fatal(err)
	}
	if ControlFilePresent(layout.Root, hardlink) {
		t.Fatal("hardlinked control file was accepted")
	}
}

func TestClassifyChildProcessCoversProcessAndAttemptEdges(t *testing.T) {
	makeResult := func(t *testing.T, result ProviderResult) (procharness.Layout, string) {
		t.Helper()
		layout, err := procharness.NewLayout(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(layout.State, "result.json")
		writeChildResult(t, path, result)
		return layout, path
	}
	passed := ProviderResult{Provider: "codex", CLIVersion: "codex-cli 1.2.3", Attempted: true, Passed: true, Status: StatusPassed}
	for _, deadline := range []bool{false, true} {
		layout, path := makeResult(t, passed)
		got := ClassifyChildProcess("codex", layout.Root, path, ChildProcessObservation{
			Result: procharness.Result{ExitCode: 0}, DeadlineExceeded: deadline, Attempted: true,
		})
		if got != passed {
			t.Errorf("clean result with deadline=%v = %+v", deadline, got)
		}
	}
	for name, child := range map[string]ProviderResult{
		"missing CLI": {
			Provider: "codex", Status: StatusSkipped, ReasonCode: ReasonMissingCLI,
		},
		"preflight without attempt": {
			Provider: "codex", CLIVersion: "codex-cli 1.2.3", Status: StatusSkipped, ReasonCode: ReasonCredentialRefresh,
		},
		"version failure without attempt": {
			Provider: "codex", Status: StatusFailed, ReasonCode: ReasonVersionProbe,
			Phase: "version", ErrorClass: "process",
		},
	} {
		t.Run(name, func(t *testing.T) {
			layout, path := makeResult(t, child)
			got := ClassifyChildProcess("codex", layout.Root, path, ChildProcessObservation{
				Result: procharness.Result{ExitCode: 0},
			})
			if got != child {
				t.Errorf("classification = %+v, want %+v", got, child)
			}
		})
	}

	tests := []struct {
		name        string
		observation ChildProcessObservation
		write       *ProviderResult
		wantReason  string
		wantDetail  string
		wantPhase   string
		wantAttempt bool
		wantTimeout bool
		wantTrunc   bool
	}{
		{name: "missing result", observation: ChildProcessObservation{Result: procharness.Result{ExitCode: 0}}, wantReason: ReasonHarnessFailed, wantDetail: "child_result", wantPhase: "harness"},
		{name: "nonzero", observation: ChildProcessObservation{Result: procharness.Result{ExitCode: 9}}, wantReason: ReasonHarnessFailed, wantDetail: "child_process", wantPhase: "harness"},
		{name: "process error", observation: ChildProcessObservation{Result: procharness.Result{ExitCode: -1, Err: errors.New("TOKEN_CANARY")}}, wantReason: ReasonHarnessFailed, wantDetail: "child_process", wantPhase: "harness"},
		{name: "truncated", observation: ChildProcessObservation{Result: procharness.Result{ExitCode: 0, StdoutTruncated: true}}, wantReason: ReasonHarnessFailed, wantDetail: "child_process", wantPhase: "harness", wantTrunc: true},
		{name: "version deadline", observation: ChildProcessObservation{Result: procharness.Result{ExitCode: -1}, DeadlineExceeded: true}, wantReason: ReasonVersionProbe, wantDetail: "child_deadline", wantPhase: "version", wantTimeout: true},
		{name: "prompt deadline", observation: ChildProcessObservation{Result: procharness.Result{ExitCode: -1}, DeadlineExceeded: true, Attempted: true}, wantReason: ReasonPromptTimeout, wantDetail: "child_deadline", wantPhase: "prompt", wantAttempt: true, wantTimeout: true},
		{name: "attempt mismatch", observation: ChildProcessObservation{Result: procharness.Result{ExitCode: 0}, Attempted: true}, write: &ProviderResult{Provider: "codex", Status: StatusSkipped, ReasonCode: ReasonMissingCLI}, wantReason: ReasonHarnessFailed, wantDetail: "attempt_mismatch", wantPhase: "harness", wantAttempt: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			layout, err := procharness.NewLayout(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(layout.State, "result.json")
			if tt.write != nil {
				writeChildResult(t, path, *tt.write)
			}
			got := ClassifyChildProcess("codex", layout.Root, path, tt.observation)
			if got.ReasonCode != tt.wantReason || got.DetailCode != tt.wantDetail || got.Phase != tt.wantPhase ||
				got.Attempted != tt.wantAttempt || got.TimedOut != tt.wantTimeout || got.Truncated != tt.wantTrunc {
				t.Errorf("classification = %+v", got)
			}
		})
	}
}

func TestProcessDeadlineExceededUsesTheProcessResult(t *testing.T) {
	if ProcessDeadlineExceeded(procharness.Result{ExitCode: 9}) {
		t.Fatal("ordinary nonzero exit was classified as a timeout")
	}
	if !ProcessDeadlineExceeded(procharness.Result{ExitCode: -1, Err: context.DeadlineExceeded}) {
		t.Fatal("managed deadline was not classified as a timeout")
	}
}

func TestFinalizeResultUsesSecurityPrecedence(t *testing.T) {
	child := ProviderResult{Provider: "codex", CLIVersion: "codex-cli 1.2.3", Status: StatusFailed, ReasonCode: ReasonPromptExit, Phase: "prompt", ErrorClass: "process"}
	tests := []struct {
		name     string
		failures VerificationFailures
		want     string
	}{
		{name: "child", want: ReasonPromptExit},
		{name: "repository", failures: VerificationFailures{RepositoryChanged: true}, want: ReasonRepositoryChanged},
		{name: "source before repository", failures: VerificationFailures{SourceChanged: true, RepositoryChanged: true}, want: ReasonSourceChanged},
		{name: "cleanup before all", failures: VerificationFailures{CleanupFailed: true, SourceChanged: true, RepositoryChanged: true}, want: ReasonCleanupFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.failures.AttemptedObserved = true
			got := FinalizeResult(child, tt.failures)
			if got.ReasonCode != tt.want || !got.Attempted || got.CLIVersion != child.CLIVersion {
				t.Errorf("FinalizeResult = %+v", got)
			}
		})
	}
}

func TestPreChildSourceVerificationFailureProducesValidSummary(t *testing.T) {
	targets := []agents.Target{{Provider: "codex"}}
	result := FinalizeResult(
		ProviderResult{Provider: "codex"},
		VerificationFailures{SourceChanged: true},
	)
	if result.Attempted || result.Phase != "verification" || result.ReasonCode != ReasonSourceChanged {
		t.Fatalf("pre-child verification result = %+v", result)
	}
	if _, err := NewSummary(false, targets, []ProviderResult{result}); err != nil {
		t.Fatalf("pre-child verification result was rejected: %v", err)
	}
}

func writeChildResult(t *testing.T, path string, result ProviderResult) {
	t.Helper()
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	writeRawResult(t, path, append(data, '\n'))
}

func writeRawResult(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
