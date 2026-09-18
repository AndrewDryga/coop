package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIRateLimited(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"You've hit your weekly limit.", true},
		{"You've reached your Fable 5 limit. Run /usage-credits to continue or switch models with /model.", true},
		{"You have reached your Fable 5 limit. Run /usage-credits to continue or switch models with /model.", true},
		{"Selected model is at capacity.", true},
		{`{"status":"RESOURCE_EXHAUSTED"}`, true},
		{"HTTP 429 Too Many Requests", true},
		{"1429 files scanned", false},
		{"task text mentions rate limit handling", true},
		{"failed while printing a standalone 429 item id", true},
		{"ordinary provider failure", false},
	}
	for _, tc := range cases {
		if got := CLIRateLimited(tc.text); got != tc.want {
			t.Errorf("CLIRateLimited(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}

func TestWrapperRateLimitedRejectsAmbiguousProse(t *testing.T) {
	for _, output := range []string{
		"task text mentions rate limit handling",
		"the endpoint is rate limited by design",
		"failed while printing a standalone 429 item id",
		"ordinary provider failure",
	} {
		if WrapperRateLimited(output) {
			t.Errorf("WrapperRateLimited(%q) = true, want false", output)
		}
	}
}

func TestCodexConsultPreservesStructuredFailureEvents(t *testing.T) {
	a, ok := Get("codex")
	if !ok {
		t.Fatal("codex adapter is not registered")
	}
	for name, body := range map[string]string{"fresh": a.ConsultFresh(), "resume": a.ConsultResume()} {
		if !strings.Contains(body, "codex_run codex exec") {
			t.Errorf("Codex %s consult bypasses the bounded shared result path:\n%s", name, body)
		}
	}
	prelude := a.ShellPrelude()
	for _, want := range []string{`start_capture "$codex_raw"`, `cat "$raw" >&2`} {
		if !strings.Contains(prelude, want) {
			t.Errorf("Codex consult drops bounded raw failure events needed for rate-limit classification; missing %q", want)
		}
	}
}

func TestShellRateLimitDetectorMatchesGo(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not installed")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "detect")
	body := "#!/bin/sh\nset -u\n" + ShellRateLimitDetector() +
		"coop_rate_limited \"$1\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	for i, output := range []string{
		"You've reached your Opus 4.8 limit.",
		"request failed: rate-limit exceeded",
		`{"status":"RESOURCE_EXHAUSTED"}`,
		"HTTP 429 Too Many Requests",
		"build failed after 1429 files",
		"ordinary provider failure",
		grokCreditsExhausted, grokAuthRejected,
	} {
		file := filepath.Join(dir, string(rune('a'+i)))
		if err := os.WriteFile(file, []byte(output), 0o644); err != nil {
			t.Fatal(err)
		}
		err := exec.Command(sh, script, file).Run()
		got := err == nil
		if got != WrapperRateLimited(output) {
			t.Errorf("shell detector(%q) = %v, Go = %v", output, got, WrapperRateLimited(output))
		}
	}
}

// What a Grok role prints for the pinned client's structured error payloads (captured by replay):
// a 402 is its "run out of credits" and rotates, a 401 is an authentication failure and does not.
const (
	grokCreditsExhausted = "Internal error: {\n  \"message\": \"API error (status 402 Payment Required): insufficient_credits: You have run out of credits.\",\n  \"http_status\": 402\n}"
	grokAuthRejected     = "Internal error: {\n  \"message\": \"Auth recovery succeeded but 4 authenticated inference requests were still rejected (401); giving up after 3 retries.\",\n  \"http_status\": 401\n}"
)

func TestGrokQuotaPayloadIsARoleRateLimit(t *testing.T) {
	if !WrapperRateLimited(grokCreditsExhausted) {
		t.Error("a Grok role's 402 payload did not read as a rate limit")
	}
	if WrapperRateLimited(grokAuthRejected) {
		t.Error("a Grok role's 401 payload read as a rate limit")
	}
}
