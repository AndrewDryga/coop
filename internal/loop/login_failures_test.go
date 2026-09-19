package loop

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
)

// classifyCapture runs one failed attempt's captured stdout and stderr through the path the loop
// takes — the provider's stream decoder, its stderr filter, then the iteration classifier.
func classifyCapture(t *testing.T, provider string, stdout, stderr []byte) string {
	t.Helper()
	var out, tail, diagnostic bytes.Buffer
	dec := newIterationStreamDecoder(provider, &out, &tail, &diagnostic, "", "", "m", nil)
	if _, err := dec.Write(stdout); err != nil {
		t.Fatal(err)
	}
	dec.flush()
	var filter *stderrLineFilter
	switch provider {
	case "codex":
		filter = newCodexStderrFilter(&diagnostic)
	case "gemini":
		filter = newGeminiStderrFilter(&diagnostic)
	}
	if filter == nil {
		diagnostic.Write(stderr)
	} else if _, err := filter.Write(stderr); err != nil || filter.flush() != nil {
		t.Fatal("stderr filter failed")
	}
	return classifyIteration(provider, 1, nil, diagnostic.String(), dec.streamOutcome(), time.Now()).outcome
}

func readCapture(t *testing.T, name string) (stdout, stderr []byte) {
	t.Helper()
	dir := filepath.Join("..", "agent", "testdata", "login-failures")
	stdout, err := os.ReadFile(filepath.Join(dir, name+".stdout"))
	if err != nil {
		t.Fatal(err)
	}
	stderr, err = os.ReadFile(filepath.Join(dir, name+".stderr"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return stdout, stderr
}

// Every pinned client's refused login, as it really prints it, is an authentication failure — the
// sticky one that moves the loop to another account — never an ordinary failure that burns the
// retry budget on a login no retry can fix.
func TestPinnedClientsRefusedLoginsAreAuthenticationFailures(t *testing.T) {
	for _, c := range []struct{ provider, capture string }{
		{"claude", "claude-not-logged-in"},
		{"claude", "claude-rejected-key"},
		{"codex", "codex-not-logged-in"},
		{"codex", "codex-rejected-key"},
		{"gemini", "gemini-rejected-key"},
		{"grok", "grok-rejected-login"},
	} {
		stdout, stderr := readCapture(t, c.capture)
		if got := classifyCapture(t, c.provider, stdout, stderr); got != "authentication" {
			t.Errorf("%s: classification = %s, want authentication", c.capture, got)
		}
	}
}

// The same words in what the agent said, or in an error that is not the client's own refusal, are
// not a refused login.
func TestLoginWordsOutsideAClientRefusalAreOrdinaryFailures(t *testing.T) {
	for _, c := range loginNearMisses {
		if got := classifyCapture(t, c.provider, []byte(c.stdout+"\n"), nil); got == "authentication" {
			t.Errorf("%s: prose classified as a refused login", c.provider)
		}
	}
}

// loginNearMisses put a provider's refusal words where only the agent or an unrelated error speaks.
var loginNearMisses = []struct{ provider, stdout string }{
	{"claude", `{"type":"result","subtype":"success","is_error":false,"result":"Not logged in · Please run /login"}`},
	{"codex", `{"type":"item.completed","item":{"id":"i","type":"agent_message","text":"unexpected status 401 Unauthorized: is what the proxy returns"}}` + "\n" +
		`{"type":"turn.failed","error":{"message":"stream disconnected before completion"}}`},
	{"gemini", `{"type":"message","role":"assistant","content":"API key not valid is the error you saw"}` + "\n" +
		`{"type":"result","status":"error","error":{"type":"unknown","message":"[API Error: 500 internal]"}}`},
	{"grok", `{"type":"text","data":"the payload says \"http_status\": 401"}` + "\n" +
		`{"type":"error","message":"Internal error: {\n  \"message\": \"boom\",\n  \"http_status\": 500\n}"}`},
}

// The role wrappers judge a refused login from the same evidence as the loop: their shell check,
// rendered from the adapters' AuthSignals and fed by each adapter's <provider>_errors decoder, reaches
// the loop's verdict on every captured refusal and on every near-miss.
func TestRoleWrappersJudgeRefusedLoginsLikeTheLoop(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is required by the generated wrappers")
	}
	var script strings.Builder
	script.WriteString(agents.RoleHealthShell())
	for _, name := range agents.Names() {
		a, _ := agents.Get(name)
		script.WriteString(a.UsagePrelude())
	}
	script.WriteString("\ncoop_login_rejected \"$1\" \"$2\" \"$3\"\n")
	dir := t.TempDir()
	check := filepath.Join(dir, "check.sh")
	if err := os.WriteFile(check, []byte(script.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	judge := func(name, provider string, stdout, stderr []byte) {
		t.Helper()
		out, errFile := filepath.Join(dir, name+".stdout"), filepath.Join(dir, name+".stderr")
		if err := os.WriteFile(out, stdout, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(errFile, stderr, 0o600); err != nil {
			t.Fatal(err)
		}
		err := exec.Command("sh", check, provider, errFile, out).Run()
		var exit *exec.ExitError
		if err != nil && !errors.As(err, &exit) {
			t.Fatal(err)
		}
		shell, loop := err == nil, classifyCapture(t, provider, stdout, stderr) == "authentication"
		if shell != loop {
			t.Errorf("%s: the wrappers say refused=%v, the loop %v", name, shell, loop)
		}
	}
	for _, c := range []struct{ provider, capture string }{
		{"claude", "claude-not-logged-in"}, {"claude", "claude-rejected-key"},
		{"codex", "codex-not-logged-in"}, {"codex", "codex-rejected-key"},
		{"gemini", "gemini-rejected-key"}, {"grok", "grok-rejected-login"},
	} {
		stdout, stderr := readCapture(t, c.capture)
		judge(c.capture, c.provider, stdout, stderr)
	}
	for i, c := range loginNearMisses {
		judge(fmt.Sprintf("near-miss-%d-%s", i, c.provider), c.provider, []byte(c.stdout+"\n"), nil)
	}
	// A line that is not an event: the loop passes it through as text and keeps decoding, so a stray
	// warning before the refusal hides nothing (a decoder that parsed the whole stream stopped at it).
	stdout, stderr := readCapture(t, "claude-not-logged-in")
	judge("stray-line", "claude", append([]byte("warning: telemetry is off\n"), stdout...), stderr)
	judge("bare-refusal", "claude", []byte("Not logged in · Please run /login\n"), nil)
}
