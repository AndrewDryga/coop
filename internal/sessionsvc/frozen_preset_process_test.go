//go:build providere2e

package sessionsvc

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

// Exercise the ordinary controller job and worker process; only the container
// runtime and provider are scripted. No operator account or external API is used.
func TestWorkerRunsFrozenReviewThroughACP(t *testing.T) {
	exerciseFrozenWorkerJob(t, true)
}

func TestWorkerRunsFrozenContributorThroughACP(t *testing.T) {
	exerciseFrozenWorkerJob(t, false)
}

func exerciseFrozenWorkerJob(t *testing.T, review bool) {
	t.Helper()
	tmp := t.TempDir()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(tmp, "coop")
	fixture := filepath.Join(tmp, "fixture")
	for path, pkg := range map[string]string{bin: ".", fixture: "./internal/acpproxy/testdata/acpfixture"} {
		cmd := exec.CommandContext(t.Context(), "go", "build", "-o", path, pkg)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v: %s", pkg, err, out)
		}
	}
	for _, name := range []string{"HOME", "XDG_STATE_HOME", "XDG_CONFIG_HOME", "COOP_HOME", "TMPDIR"} {
		dir := filepath.Join(tmp, name)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		t.Setenv(name, dir)
	}
	// The test daemon and compiled CLI must agree on the host-private fork anchors.
	t.Setenv(forkspace.PrivateStateRootEnv, filepath.Join(os.Getenv("HOME"), ".local", "state", "coop"))
	peerDir := filepath.Join(tmp, "scripted-peers")
	if err := os.Mkdir(peerDir, 0700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"claude": `if [ "${COOP_ACP_FIXTURE_FAIL_THIS_PEER:-}" = 1 ]; then echo "invalid model" >&2; exit 1; fi
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"Reader checked the change.","session_id":"abc123"}'`,
		"codex": `printf '%s\n' '{"type":"thread.started","thread_id":"def123"}' '{"type":"item.completed","item":{"type":"agent_message","text":"Verifier checked the test."}}' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'`,
	} {
		if err := os.WriteFile(filepath.Join(peerDir, name), []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("COOP_TEST_SESSION_PEER_BIN", peerDir)
	plan := filepath.Join(tmp, "plan.json")
	script := `{"providers":{"codex":[[
		{"method":"initialize","result":{"protocolVersion":1,"agentCapabilities":{},"authMethods":[]}},
		{"method":"session/new","result":{"sessionId":"C1"}},
		{"method":"session/prompt","consult_roles":["reader","verifier"],"failed_consult_roles":["unavailable"],"events":[{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"C1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Reviewed the requested revision."}}}}],"result":{"stopReason":"end_turn"}}
	]]}}`
	prompt, final := "Review the revision.", "Reviewed the requested revision."
	if !review {
		prompt, final = "Apply the requested change.", "Updated the requested file."
		script = strings.Replace(script, `"consult_roles":["reader","verifier"],"failed_consult_roles":["unavailable"]`, `"edit_fixture_file":true`, 1)
		script = strings.Replace(script, "Reviewed the requested revision.", final, 1)
	}
	if err := os.WriteFile(plan, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(tmp, "provider-state")
	t.Setenv("COOP_TEST_SESSION_SCRIPTED_RUNTIME", fixture)
	t.Setenv("COOP_TEST_SESSION_SCRIPTED_PLAN", plan)
	t.Setenv("COOP_TEST_SESSION_SCRIPTED_STATE", state)
	child := filepath.Join(tmp, "worker-child")
	t.Setenv("COOP_TEST_SESSION_COOP", bin)
	t.Setenv("COOP_TEST_SESSION_CHILD_LOG", filepath.Join(tmp, "child.log"))
	if err := os.WriteFile(child, []byte(`#!/bin/sh
exec "$COOP_TEST_SESSION_COOP" "$@" 2>> "$COOP_TEST_SESSION_CHILD_LOG"
`), 0700); err != nil {
		t.Fatal(err)
	}
	rt := filepath.Join(tmp, "docker")
	if err := os.WriteFile(rt, []byte(`#!/bin/sh
if [ "$1" = info ]; then exit 0; fi
for arg do
  case "$arg" in coop-box:*) export COOP_IMAGE="$arg"; break ;; esac
done
export PATH="$COOP_TEST_SESSION_PEER_BIN:$PATH"
export COOP_ACP_FIXTURE_PLAN="$COOP_TEST_SESSION_SCRIPTED_PLAN"
export COOP_ACP_FIXTURE_STATE="$COOP_TEST_SESSION_SCRIPTED_STATE"
exec "$COOP_TEST_SESSION_SCRIPTED_RUNTIME" "$@"
`), 0700); err != nil {
		t.Fatal(err)
	}
	repo, git := gitrepo.New(t)
	if err := os.WriteFile(filepath.Join(repo, "message.txt"), []byte("original message\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "message.txt")
	git("commit", "-q", "-m", "requested head")
	accounts := filepath.Join(tmp, "accounts")
	writeSessionTestCredential(t, accounts, "codex@work")
	writeSessionTestCredential(t, accounts, "claude@review")
	svc := newSessionFixture(t, Config{
		StateRoot: filepath.Join(tmp, "worker"), Runtime: runtime.Runtime{Name: rt}, Executable: child,
		SourceConfig: &config.Config{ConfigDir: accounts, ImageOverride: "fixture-image"},
	}, repo)
	defer svc.Stop()
	svc.Job.RepositoryReadOnly = review
	svc.Job.Egress.Mode = "none"
	svc.Job.Limits.TurnTimeoutMS = 30000
	svc.Job.Preset = &workerproto.JobPreset{Version: 1, Name: "review", LeadPrompt: "Check this exact head.", Roles: []workerproto.JobPresetRole{
		{Name: "reader", Mode: "consult", When: []string{}, Targets: []string{"claude@review"}, Prompt: "Read the revision."},
		{Name: "verifier", Mode: "consult", When: []string{}, Targets: []string{"codex@work"}, Prompt: "Verify the test."},
		{Name: "unavailable", Mode: "consult", When: []string{}, Targets: []string{"claude@review"}, Prompt: "Consult the assigned specialist."},
		{Name: "optional", Mode: "consult", When: []string{}, Targets: []string{"claude@review"}, Prompt: "Check if needed."},
	}}
	if !review {
		svc.Job.Preset = &workerproto.JobPreset{Version: 1, Name: "contributor", LeadPrompt: "Implement the authorized change.", Roles: []workerproto.JobPresetRole{}}
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	sess, err := svc.CreateRemoteSession(ctx, "create-review", svc.request(t, svc.Job.Preset.Name))
	if err != nil {
		t.Fatal(err)
	}
	turn, err := svc.SubmitTurn(ctx, "review-turn", session.SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: prompt})
	if err != nil {
		t.Fatal(err)
	}
	for {
		current, err := svc.Store().GetTurn(ctx, sess.ID, turn.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.State == session.TurnCompleted {
			if !strings.Contains(current.AssistantMessage, final) || (review && (!strings.Contains(current.AssistantMessage, "Reader checked the change.") || !strings.Contains(current.AssistantMessage, "Verifier checked the test."))) {
				t.Fatalf("unexpected review output: %s", current.AssistantMessage)
			}
			break
		}
		if current.State == session.TurnFailed {
			detail, _ := os.ReadFile(filepath.Join(tmp, "child.log"))
			t.Fatalf("worker turn failed: %s: %s", current.ErrorDetail, detail)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
	argv, err := os.ReadFile(filepath.Join(state, "runtime.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if review && (!strings.Contains(string(argv), "coop-consult") || !strings.Contains(string(argv), ":/workspace:ro")) {
		t.Fatalf("review mounts missing: %s", argv)
	}
	if review {
		events, err := svc.Store().ListEvents(ctx, sess.ID, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		statuses := map[string]string{}
		for _, event := range events {
			if event.TurnID != turn.ID || event.Type != "preset.peer" {
				continue
			}
			var summary struct {
				Role   string
				Status string
			}
			if err := json.Unmarshal(event.Payload, &summary); err != nil {
				t.Fatal(err)
			}
			statuses[summary.Role] = summary.Status
		}
		if statuses["reader"] != "answered" || statuses["verifier"] != "answered" || statuses["optional"] != "not_called" || statuses["unavailable"] != "failed" {
			t.Fatalf("worker-reported outcomes = %v", statuses)
		}
	}
	if !review {
		changes, err := svc.GetChanges(ctx, sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(changes.Patch, "+updated message") {
			t.Fatalf("writer patch = %q", changes.Patch)
		}
		data, err := os.ReadFile(filepath.Join(repo, "message.txt"))
		if err != nil || string(data) != "original message\n" {
			t.Fatalf("source file changed outside the fork: %q, %v", data, err)
		}
	}
}
