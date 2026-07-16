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
