package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/session"
)

func TestParseSessionPoliciesIsStrictAndPinsOneCredentialPerTarget(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	companion, companionGit := gitRepo(t)
	companionGit("commit", "-q", "--allow-empty", "-m", "companion base")
	repo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	companion, err = filepath.EvalSymlinks(companion)
	if err != nil {
		t.Fatal(err)
	}
	configRoot := t.TempDir()
	profile := filepath.Join(configRoot, "codex", "profiles", "work")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ConfigDir: configRoot}
	valid := []byte("version: 1\npolicies:\n  responder:\n    repository: " + repo +
		"\n    remote: origin\n    branch: main" +
		"\n    companions:\n      - name: application\n        repository: " + companion +
		"\n        remote: upstream\n        branch: master" +
		"\n    target: codex:model/high@work\n    max_turns: 100\n    max_queued_turns: 20\n    max_queued_bytes: 1048576\n    max_patch_bytes: 1048576\n    turn_timeout: 1h\n    warm_idle_timeout: 15m\n")
	policies, err := parseSessionPolicies(valid, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := policies["responder"]; sessionTargetList(got.Targets) != "codex:model/high@work" ||
		got.Repository != repo || got.Remote != "origin" || got.Branch != "main" ||
		got.TurnTimeout != time.Hour ||
		got.WarmIdleTimeout != 15*time.Minute ||
		len(got.Companions) != 1 ||
		got.Companions[0] != (SessionCompanionPolicy{
			Name: "application", Repository: companion,
			Remote: "upstream", Branch: "master",
		}) {
		t.Fatalf("parsed policy = %+v", got)
	}
	for name, body := range map[string]string{
		"unknown":  string(valid) + "    typo: true\n",
		"preset":   string(valid[:len(valid)-1]) + "    target: codex@work,other\n",
		"relative": "version: 1\npolicies:\n  p:\n    repository: repo\n    target: codex@work\n    max_turns: 1\n    max_queued_turns: 1\n    max_queued_bytes: 1\n    max_patch_bytes: 1\n    turn_timeout: 1s\n",
	} {
		if _, err := parseSessionPolicies([]byte(body), cfg); err == nil {
			t.Fatalf("%s policy unexpectedly accepted", name)
		}
	}
	tooLong := strings.Replace(string(valid), "warm_idle_timeout: 15m", "warm_idle_timeout: 61m", 1)
	if _, err := parseSessionPolicies([]byte(tooLong), cfg); err == nil ||
		!strings.Contains(err.Error(), "warm_idle_timeout") {
		t.Fatalf("oversized warm idle timeout error = %v", err)
	}
	for name, source := range map[string]string{
		"remote only":    "    remote: origin\n",
		"branch only":    "    branch: main\n",
		"unsafe remote":  "    remote: ../origin\n    branch: main\n",
		"invalid branch": "    remote: origin\n    branch: bad..branch\n",
	} {
		body := "version: 1\npolicies:\n  responder:\n    repository: " + repo + "\n" + source +
			"    target: codex@work\n    max_turns: 1\n    max_queued_turns: 1\n" +
			"    max_queued_bytes: 1\n    max_patch_bytes: 1\n    turn_timeout: 1s\n"
		if _, err := parseSessionPolicies([]byte(body), nil); err == nil {
			t.Fatalf("%s source unexpectedly accepted", name)
		}
	}
}

func TestParseSessionPoliciesAcceptsATargetLadder(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	repo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	configRoot := t.TempDir()
	signIn := func(agent, credential, marker string) {
		dir := filepath.Join(configRoot, agent, "profiles", credential)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, marker), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	signIn("codex", "oncall", "auth.json")
	signIn("codex", "default", "auth.json")
	signIn("claude", "oncall", ".credentials.json")
	cfg := &config.Config{ConfigDir: configRoot}
	policy := func(target string) []byte {
		return []byte("version: 1\npolicies:\n  responder:\n    repository: " + repo +
			"\n    target: " + target +
			"\n    max_turns: 1\n    max_queued_turns: 1\n    max_queued_bytes: 1\n" +
			"    max_patch_bytes: 1\n    turn_timeout: 1s\n")
	}

	policies, err := parseSessionPolicies(policy("[codex:gpt-5.6-sol/xhigh@oncall, claude@oncall]"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := sessionTargetList(policies["responder"].Targets); got != "codex:gpt-5.6-sol/xhigh@oncall claude@oncall" {
		t.Fatalf("cross-provider ladder = %q", got)
	}

	// A rung with no @credential resolves to that provider's default, exactly as a scalar does.
	policies, err = parseSessionPolicies(policy("[claude@oncall, codex]"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := sessionTargetList(policies["responder"].Targets); got != "claude@oncall codex@default" {
		t.Fatalf("default-credential rung = %q", got)
	}

	for target, want := range map[string]string{
		"[]":                            "target is an empty list",
		"{provider: codex}":             "target must be a target",
		"[[codex@oncall]]":              "target[0] must be a target",
		"[codex@oncall, codex@oncall]":  `target[1] "codex@oncall" is repeated`,
		"[codex, codex@default]":        `target[1] "codex@default" is repeated`,
		`["codex@oncall,default"]`:      "target[0] must name zero or one credential",
		"[codex@oncall, nosuch@oncall]": `target[1] unknown provider "nosuch"`,
		"[codex@oncall, claude@oncall, codex:a@oncall, codex:b@oncall, codex:c@oncall]": "limited to 4 rungs",
	} {
		_, err := parseSessionPolicies(policy(target), cfg)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("target %s error = %v, want %q", target, err, want)
		}
	}

	// The rung a diagnostic names is the one an operator has to go fix — the whole point of
	// checking every rung rather than only the one a session starts on.
	_, err = parseSessionPolicies(policy("[codex@oncall, claude@absent]"), cfg)
	if err == nil || !strings.Contains(err.Error(), "target[1]") ||
		!strings.Contains(err.Error(), "not authenticated") {
		t.Fatalf("unauthenticated rung error = %v", err)
	}
	// A scalar target is not a one-rung ladder to the operator, so it is not indexed.
	_, err = parseSessionPolicies(policy("codex@absent"), cfg)
	if err == nil || strings.Contains(err.Error(), "target[") ||
		!strings.Contains(err.Error(), "not authenticated") {
		t.Fatalf("scalar target error = %v", err)
	}
}

func TestWarmIdleTimeoutIsBoundIntoPolicyDigest(t *testing.T) {
	policy := SessionPolicy{Name: "conversation", Repository: "/repo", Targets: mustTargets("codex@work"), TurnTimeout: time.Hour}
	cold := resolvedSessionPolicyDigest(policy)
	if want := "0f7066c5d36ac4cfd709ce3908092be4f92f8ee2b93d4d6983cea527e8bc2ddb"; cold != want {
		t.Fatalf("cold policy digest = %q, want backward-compatible %q", cold, want)
	}
	policy.WarmIdleTimeout = 15 * time.Minute
	if warm := resolvedSessionPolicyDigest(policy); warm == cold {
		t.Fatal("warm idle timeout did not change the immutable policy digest")
	}
	policy.WarmIdleTimeout = 0
	policy.Remote, policy.Branch = "origin", "main"
	if remote := resolvedSessionPolicyDigest(policy); remote == cold {
		t.Fatal("remote repository source did not change the immutable policy digest")
	}
}

func TestParseSessionPoliciesRejectsUnsafeCompanions(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	repo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	prefix := "version: 1\npolicies:\n  responder:\n    repository: " + repo + "\n"
	suffix := "    target: codex@work\n    max_turns: 1\n    max_queued_turns: 1\n" +
		"    max_queued_bytes: 1\n    max_patch_bytes: 1\n    turn_timeout: 1s\n"
	for name, companions := range map[string]string{
		"primary alias":    "    companions:\n      - name: primary\n        repository: " + repo + "\n",
		"uppercase alias":  "    companions:\n      - name: Application\n        repository: " + repo + "\n",
		"duplicate source": "    companions:\n      - name: application\n        repository: " + repo + "\n",
	} {
		if _, err := parseSessionPolicies(
			[]byte(prefix+companions+suffix), nil,
		); err == nil {
			t.Fatalf("%s companion unexpectedly accepted", name)
		}
	}
}

func TestParseSessionPoliciesBoundsCompanionCount(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	repo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	var companions string
	for index := 0; index <= sessionPolicyMaxCompanions; index++ {
		companion, companionGit := gitRepo(t)
		companionGit("commit", "-q", "--allow-empty", "-m", "base")
		companion, err = filepath.EvalSymlinks(companion)
		if err != nil {
			t.Fatal(err)
		}
		companions += fmt.Sprintf(
			"      - name: repo%d\n        repository: %s\n",
			index, companion,
		)
	}
	body := "version: 1\npolicies:\n  responder:\n    repository: " + repo +
		"\n    companions:\n" + companions +
		"    target: codex@work\n    max_turns: 1\n    max_queued_turns: 1\n" +
		"    max_queued_bytes: 1\n    max_patch_bytes: 1\n    turn_timeout: 1s\n"
	if _, err := parseSessionPolicies([]byte(body), nil); err == nil ||
		!strings.Contains(err.Error(), "limited to 32") {
		t.Fatalf("oversized companion set error = %v", err)
	}
}

func TestLoadSessionPoliciesRejectsUnsafeFileAndAncestry(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	repo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	body := "version: 1\npolicies:\n  responder:\n    repository: " + repo + "\n    target: codex@work\n    max_turns: 1\n    max_queued_turns: 1\n    max_queued_bytes: 1\n    max_patch_bytes: 1\n    turn_timeout: 1s\n"
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "session-policies.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSessionPolicies(path, nil); err != nil {
		t.Fatalf("normal policy file rejected: %v", err)
	}
	symlink := filepath.Join(root, "policy-link.yaml")
	if err := os.Symlink(path, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSessionPolicies(symlink, nil); err == nil {
		t.Fatal("policy symlink was accepted")
	}
	if err := os.Chmod(path, 0o622); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSessionPolicies(path, nil); err == nil {
		t.Fatal("group/world-writable policy file was accepted")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	unsafeDir := filepath.Join(root, "unsafe")
	if err := os.Mkdir(unsafeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	unsafePath := filepath.Join(unsafeDir, "session-policies.yaml")
	if err := os.WriteFile(unsafePath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unsafeDir, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSessionPolicies(unsafePath, nil); err == nil {
		t.Fatal("group/world-writable policy ancestry was accepted")
	}
}

func TestSessionServiceCreateReplayUsesPersistedIntentAndWorkspaceBase(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	base := gitOut(repo, "rev-parse", "HEAD")
	root := t.TempDir()
	policies := testSessionPolicies(repo)
	service := newTestSessionService(t, filepath.Join(root, "state"), policies, nil)
	defer service.Stop()
	request := CreateRemoteSessionRequest{Policy: "responder", Task: "task-1"}
	op, replay, err := service.Store().ReserveOperation(context.Background(), "CreateRemoteSession", "create-1", request)
	if err != nil || replay {
		t.Fatalf("reserve create = %+v, replay=%v, err=%v", op, replay, err)
	}
	intent := sessionCreateIntent{
		OperationID: op.ID, Policy: policies["responder"], Task: request.Task,
		SessionID: deterministicSessionID(op.ID), ForkName: deterministicForkName(op.ID), BaseCommit: base,
	}
	intentBytes, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Store().MarkOperationRunning(context.Background(), op.ID, intentBytes); err != nil {
		t.Fatal(err)
	}
	git("commit", "-q", "--allow-empty", "-m", "parent advanced")
	sess, err := service.CreateRemoteSession(context.Background(), "create-1", request)
	if err != nil {
		t.Fatal(err)
	}
	policy := policies["responder"]
	if sess.BaseCommit != base || sess.ID != intent.SessionID || sess.ForkName != intent.ForkName ||
		sess.PolicyDigest != resolvedSessionPolicyDigest(policy) || sess.TurnTimeout != policy.TurnTimeout || sess.MaxPatchBytes != policy.MaxPatchBytes {
		t.Fatalf("replayed session = %+v, intent=%+v", sess, intent)
	}
	if got := gitOut(sess.Workspace, "rev-parse", "HEAD"); got != base {
		t.Fatalf("workspace HEAD = %s, want persisted base %s", got, base)
	}
	replayed, err := service.CreateRemoteSession(context.Background(), "create-1", request)
	if err != nil || replayed.ID != sess.ID || replayed.Workspace != sess.Workspace {
		t.Fatalf("create replay = %+v, err=%v", replayed, err)
	}
}

func TestSessionServicePinsConfiguredRemoteWithoutChangingLocalCheckout(t *testing.T) {
	seed, seedGit := gitRepo(t)
	if err := os.WriteFile(filepath.Join(seed, "version.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedGit("add", "version.txt")
	seedGit("commit", "-qm", "v1")
	seedGit("branch", "-M", "main")
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGitTest(t, "", "init", "-q", "--bare", remote)
	seedGit("remote", "add", "origin", remote)
	seedGit("push", "-q", "-u", "origin", "main")

	checkout := filepath.Join(t.TempDir(), "checkout")
	runGitTest(t, "", "clone", "-q", remote, checkout)
	runGitTest(t, checkout, "config", "user.email", "t@t")
	runGitTest(t, checkout, "config", "user.name", "T")
	localMain := gitOut(checkout, "rev-parse", "HEAD")
	runGitTest(t, checkout, "checkout", "-qb", "local-feature")
	runGitTest(t, checkout, "commit", "-q", "--allow-empty", "-m", "local feature")
	localHead := gitOut(checkout, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(checkout, "local-only.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	statusBefore := gitOut(checkout, "status", "--porcelain=v1")

	if err := os.WriteFile(filepath.Join(seed, "version.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedGit("commit", "-qam", "v2")
	seedGit("push", "-q", "origin", "main")
	remoteHead := gitOut(seed, "rev-parse", "HEAD")
	if remoteHead == localMain {
		t.Fatal("remote did not advance")
	}

	companionSeed, companionSeedGit := gitRepo(t)
	if err := os.WriteFile(filepath.Join(companionSeed, "topology.txt"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	companionSeedGit("add", "topology.txt")
	companionSeedGit("commit", "-qm", "old topology")
	companionSeedGit("branch", "-M", "master")
	companionRemote := filepath.Join(t.TempDir(), "companion.git")
	runGitTest(t, "", "init", "-q", "--bare", companionRemote)
	companionSeedGit("remote", "add", "origin", companionRemote)
	companionSeedGit("push", "-q", "-u", "origin", "master")
	companionCheckout := filepath.Join(t.TempDir(), "companion-checkout")
	runGitTest(t, "", "clone", "-q", "-b", "master", companionRemote, companionCheckout)
	companionLocalHead := gitOut(companionCheckout, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(companionSeed, "topology.txt"), []byte("current\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	companionSeedGit("commit", "-qam", "current topology")
	companionSeedGit("push", "-q", "origin", "master")
	companionRemoteHead := gitOut(companionSeed, "rev-parse", "HEAD")

	policies := testSessionPolicies(checkout)
	policy := policies["responder"]
	policy.Remote, policy.Branch = "origin", "main"
	policy.Companions = []SessionCompanionPolicy{{
		Name: "topology", Repository: companionCheckout,
		Remote: "origin", Branch: "master",
	}}
	policies["responder"] = policy
	service := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), policies, nil)
	defer service.Stop()
	sess, err := service.CreateRemoteSession(
		context.Background(), "remote-source-create",
		CreateRemoteSessionRequest{Policy: "responder", Task: "fresh remote snapshot"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if sess.BaseCommit != remoteHead || gitOut(sess.Workspace, "rev-parse", "HEAD") != remoteHead {
		t.Fatalf("session base = %s, workspace HEAD = %s, want remote %s", sess.BaseCommit, gitOut(sess.Workspace, "rev-parse", "HEAD"), remoteHead)
	}
	if got := readFile(t, filepath.Join(sess.Workspace, "version.txt")); got != "v2\n" {
		t.Fatalf("session version = %q, want fresh remote v2", got)
	}
	if len(sess.Companions) != 1 || sess.Companions[0].BaseCommit != companionRemoteHead {
		t.Fatalf("session companion = %+v, want remote %s", sess.Companions, companionRemoteHead)
	}
	if got := readFile(t, filepath.Join(sess.Companions[0].Workspace, "topology.txt")); got != "current\n" {
		t.Fatalf("companion topology = %q, want current remote snapshot", got)
	}
	if got := gitOut(checkout, "rev-parse", "HEAD"); got != localHead {
		t.Fatalf("local HEAD moved to %s, want %s", got, localHead)
	}
	if got := gitOut(checkout, "symbolic-ref", "--short", "HEAD"); got != "local-feature" {
		t.Fatalf("local branch = %q, want local-feature", got)
	}
	if got := gitOut(checkout, "status", "--porcelain=v1"); got != statusBefore {
		t.Fatalf("local status changed from %q to %q", statusBefore, got)
	}
	if got := gitOut(checkout, "rev-parse", "refs/remotes/origin/main"); got != localMain {
		t.Fatalf("tracking ref moved to %s, want unchanged %s", got, localMain)
	}
	if got := gitOut(companionCheckout, "rev-parse", "HEAD"); got != companionLocalHead {
		t.Fatalf("companion local HEAD moved to %s, want %s", got, companionLocalHead)
	}
	if got := gitOut(companionCheckout, "rev-parse", "refs/remotes/origin/master"); got != companionLocalHead {
		t.Fatalf("companion tracking ref moved to %s, want %s", got, companionLocalHead)
	}
	changes, err := service.GetChanges(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if changes.ParentHead != remoteHead || changes.ParentDivergence.Ahead != 0 ||
		changes.ParentDivergence.Behind != 0 || changes.ParentDivergence.Diverged {
		t.Fatalf("remote-backed changes = %+v, want clean comparison with %s", changes, remoteHead)
	}
	closed, err := service.Close(
		context.Background(), "remote-source-close",
		session.CloseSessionRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision},
	)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.PlanDiscard(
		context.Background(), "remote-source-plan-discard",
		PlanDiscardRequest{SessionID: sess.ID, ExpectedRevision: closed.Revision},
	)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Plan.Workspace.ParentHead != remoteHead || plan.Plan.Workspace.Unmerged {
		t.Fatalf("remote-backed discard plan = %+v, want merged into %s", plan.Plan.Workspace, remoteHead)
	}
	if _, err := service.Discard(
		context.Background(), "remote-source-discard",
		DiscardRequest{PlanOperationID: plan.OperationID},
	); err != nil {
		t.Fatal(err)
	}
}

func TestSessionServiceConfiguredRemoteFailureDoesNotFallBackToLocalHead(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "local base")
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGitTest(t, "", "init", "-q", "--bare", remote)
	git("remote", "add", "origin", remote)
	policies := testSessionPolicies(repo)
	policy := policies["responder"]
	policy.Remote, policy.Branch = "origin", "main"
	policies["responder"] = policy
	service := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), policies, nil)
	defer service.Stop()
	_, err := service.CreateRemoteSession(
		context.Background(), "missing-remote-branch",
		CreateRemoteSessionRequest{Policy: "responder", Task: "must not use stale head"},
	)
	if err == nil || !strings.Contains(err.Error(), "check the remote, branch, network, and Git credentials") {
		t.Fatalf("remote failure = %v", err)
	}
	sessions, listErr := service.ListSessions(context.Background(), 10)
	if listErr != nil || len(sessions) != 0 {
		t.Fatalf("sessions after failed refresh = %+v, err=%v", sessions, listErr)
	}
}

func TestSessionServiceConcurrentCreatesPinTheSameRemoteCommit(t *testing.T) {
	seed, seedGit := gitRepo(t)
	seedGit("commit", "-q", "--allow-empty", "-m", "base")
	seedGit("branch", "-M", "main")
	checkout := filepath.Join(t.TempDir(), "checkout")
	runGitTest(t, "", "clone", "-q", seed, checkout)
	seedGit("commit", "-q", "--allow-empty", "-m", "remote advance")
	remoteHead := gitOut(seed, "rev-parse", "HEAD")

	policies := testSessionPolicies(checkout)
	policy := policies["responder"]
	policy.Remote, policy.Branch = "origin", "main"
	policies["responder"] = policy
	service := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), policies, nil)
	defer service.Stop()

	type result struct {
		session session.Session
		err     error
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for _, key := range []string{"concurrent-a", "concurrent-b"} {
		key := key
		wg.Add(1)
		go func() {
			defer wg.Done()
			sess, err := service.CreateRemoteSession(
				context.Background(), key,
				CreateRemoteSessionRequest{Policy: "responder", Task: key},
			)
			results <- result{session: sess, err: err}
		}()
	}
	wg.Wait()
	close(results)
	for got := range results {
		if got.err != nil || got.session.BaseCommit != remoteHead {
			t.Fatalf("concurrent create = %+v, err=%v, want base %s", got.session, got.err, remoteHead)
		}
	}
}

func runGitTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestSessionServicePinsPersistsAndDiscardsCompanionRepositories(t *testing.T) {
	primary, primaryGit := gitRepo(t)
	primaryGit("commit", "-q", "--allow-empty", "-m", "primary base")
	companion, companionGit := gitRepo(t)
	if err := os.WriteFile(
		filepath.Join(companion, "topology.txt"), []byte("v1\n"), 0o644,
	); err != nil {
		t.Fatal(err)
	}
	companionGit("add", "topology.txt")
	companionGit("commit", "-qm", "companion base")
	companionBase := gitOut(companion, "rev-parse", "HEAD")
	policies := testSessionPolicies(primary)
	policy := policies["responder"]
	policy.Companions = []SessionCompanionPolicy{{
		Name: "topology", Repository: companion,
	}}
	policies["responder"] = policy
	service := newTestSessionService(
		t, filepath.Join(t.TempDir(), "state"), policies, nil,
	)
	defer service.Stop()

	created, err := service.CreateRemoteSession(
		context.Background(), "create-companion",
		CreateRemoteSessionRequest{Policy: "responder", Task: "multi-repo"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(created.Companions) != 1 ||
		created.Companions[0].Name != "topology" ||
		created.Companions[0].BaseCommit != companionBase ||
		created.Companions[0].Workspace == companion {
		t.Fatalf("created companion binding = %+v", created.Companions)
	}
	if branch := gitOut(created.Companions[0].Workspace, "rev-parse", "--abbrev-ref", "HEAD"); branch != "HEAD" {
		t.Fatalf("companion snapshot branch = %q, want detached HEAD", branch)
	}
	if got := readFile(
		t, filepath.Join(created.Companions[0].Workspace, "topology.txt"),
	); got != "v1\n" {
		t.Fatalf("companion snapshot = %q", got)
	}
	if err := os.WriteFile(
		filepath.Join(companion, "topology.txt"), []byte("v2\n"), 0o644,
	); err != nil {
		t.Fatal(err)
	}
	companionGit("commit", "-qam", "advance companion")
	if got := readFile(
		t, filepath.Join(created.Companions[0].Workspace, "topology.txt"),
	); got != "v1\n" {
		t.Fatalf("pinned companion changed with source = %q", got)
	}
	persisted, err := service.GetSession(context.Background(), created.ID)
	if err != nil || len(persisted.Companions) != 1 ||
		persisted.Companions[0] != created.Companions[0] {
		t.Fatalf("persisted companion = %+v, %v", persisted.Companions, err)
	}
	public := publicSession(persisted)
	if len(public.Companions) != 1 ||
		public.Companions[0].Path != "/coop/repositories/topology" ||
		public.Companions[0].BaseCommit != companionBase {
		t.Fatalf("public companion = %+v", public.Companions)
	}

	closed, err := service.Close(
		context.Background(), "close-companion",
		session.CloseSessionRequest{
			SessionID: created.ID, ExpectedRevision: created.Revision,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.PlanDiscard(
		context.Background(), "plan-companion",
		PlanDiscardRequest{
			SessionID: created.ID, ExpectedRevision: closed.Revision,
		},
	)
	if err != nil || len(plan.Plan.Companions) != 1 {
		t.Fatalf("companion discard plan = %+v, %v", plan, err)
	}
	discarded, err := service.Discard(
		context.Background(), "discard-companion",
		DiscardRequest{PlanOperationID: plan.OperationID},
	)
	if err != nil || discarded.State != session.SessionDiscarded ||
		pathExists(created.Companions[0].Workspace) {
		t.Fatalf("companion discard = %+v, %v", discarded, err)
	}
}

func TestSessionServiceCreateRollsBackPartialMultiRepositoryWorkspace(t *testing.T) {
	primary, primaryGit := gitRepo(t)
	primaryGit("commit", "-q", "--allow-empty", "-m", "primary base")
	first, firstGit := gitRepo(t)
	firstGit("commit", "-q", "--allow-empty", "-m", "first base")
	blocked, blockedGit := gitRepo(t)
	blockedGit("commit", "-q", "--allow-empty", "-m", "blocked base")
	policies := testSessionPolicies(primary)
	policy := policies["responder"]
	policy.Companions = []SessionCompanionPolicy{
		{Name: "first", Repository: first},
		{Name: "blocked", Repository: blocked},
	}
	policies["responder"] = policy
	service := newTestSessionService(
		t, filepath.Join(t.TempDir(), "state"), policies, nil,
	)
	defer service.Stop()
	ctx := context.Background()
	request := CreateRemoteSessionRequest{
		Policy: "responder", Task: "partial multi-repository create",
	}
	op, replay, err := service.Store().ReserveOperation(
		ctx, "CreateRemoteSession", "create-partial-cleanup", request,
	)
	if err != nil || replay {
		t.Fatalf("reserve create = %+v, replay=%t, err=%v", op, replay, err)
	}
	sessionID := deterministicSessionID(op.ID)
	blockedPath, err := sessionCompanionWorkspace(
		service.Store().Root(), sessionID, "blocked",
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(blockedPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blockedPath, []byte("operator-owned\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = service.CreateRemoteSession(ctx, "create-partial-cleanup", request)
	if err == nil || !strings.Contains(err.Error(), `ensure companion "blocked"`) {
		t.Fatalf("create error = %v", err)
	}
	primaryPath := forkWorkspace(primary, deterministicForkName(op.ID))
	firstPath, err := sessionCompanionWorkspace(
		service.Store().Root(), sessionID, "first",
	)
	if err != nil {
		t.Fatal(err)
	}
	if pathExists(primaryPath) || pathExists(firstPath) {
		t.Fatalf(
			"partial workspaces survived: primary=%t companion=%t",
			pathExists(primaryPath), pathExists(firstPath),
		)
	}
	if got := readFile(t, blockedPath); got != "operator-owned\n" {
		t.Fatalf("ambiguous blocked path was changed: %q", got)
	}
}

func TestSessionServiceFIFOOneWorkerAndCancel(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	var mu sync.Mutex
	var prompts []string
	var running, maxRunning atomic.Int32
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var fakeStore *session.Store
	runner := SessionRunnerFunc(func(ctx context.Context, bound session.Session, turn session.Turn) (session.Turn, error) {
		n := running.Add(1)
		for {
			old := maxRunning.Load()
			if n <= old || maxRunning.CompareAndSwap(old, n) {
				break
			}
		}
		mu.Lock()
		prompts = append(prompts, turn.Prompt)
		mu.Unlock()
		if turn.Prompt == "cancel" {
			started <- struct{}{}
			<-ctx.Done()
		} else if turn.Prompt == "first" {
			started <- struct{}{}
			<-release
		}
		defer running.Add(-1)
		if err := ctx.Err(); err != nil {
			return turn, err
		}
		if _, err := fakeStore.MarkTurnSendIntent(context.Background(), bound.ID, turn.ID); err != nil {
			return turn, err
		}
		if _, err := fakeStore.MarkTurnSent(context.Background(), bound.ID, turn.ID); err != nil {
			return turn, err
		}
		return fakeStore.CompleteTurn(context.Background(), session.CompleteTurnRequest{SessionID: bound.ID, TurnID: turn.ID, Message: turn.Prompt})
	})
	service := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), testSessionPolicies(repo), func(store *session.Store) SessionRunner {
		fakeStore = store
		return runner
	})
	defer service.Stop()
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	sess, err := service.CreateRemoteSession(context.Background(), "create", CreateRemoteSessionRequest{Policy: "responder", Task: "fifo"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.SubmitTurn(context.Background(), "turn-1", session.SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "first"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.SubmitTurn(context.Background(), "turn-2", session.SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "second"})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if got := service.Store(); got == nil {
		t.Fatal("service lost its store")
	}
	close(release)
	waitForSessionTest(t, func() bool {
		got, err := service.GetTurn(context.Background(), sess.ID, second.ID)
		return err == nil && got.State == session.TurnCompleted
	})
	if maxRunning.Load() != 1 {
		t.Fatalf("maximum concurrent workers = %d", maxRunning.Load())
	}
	mu.Lock()
	if len(prompts) != 2 || prompts[0] != "first" || prompts[1] != "second" {
		t.Fatalf("FIFO prompts = %v", prompts)
	}
	mu.Unlock()

	third, err := service.SubmitTurn(context.Background(), "turn-3", session.SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "cancel"})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	cancelled, err := service.CancelTurn(context.Background(), "cancel-3", session.CancelTurnRequest{SessionID: sess.ID, TurnID: third.ID, ExpectedRevision: sess.Revision})
	if err != nil || cancelled.State != session.TurnCancelled {
		t.Fatalf("active cancellation = %+v, err=%v", cancelled, err)
	}
	replayedCancel, err := service.CancelTurn(context.Background(), "cancel-3", session.CancelTurnRequest{SessionID: sess.ID, TurnID: third.ID, ExpectedRevision: sess.Revision})
	if err != nil || replayedCancel.ID != cancelled.ID || replayedCancel.State != session.TurnCancelled {
		t.Fatalf("active cancellation replay = %+v, err=%v", replayedCancel, err)
	}
}

func TestSessionServiceQueuedCancelReplaysAfterRevisionChange(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	service := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), testSessionPolicies(repo), nil)
	defer service.Stop()
	sess, err := service.CreateRemoteSession(context.Background(), "create", CreateRemoteSessionRequest{Policy: "responder", Task: "queued-cancel"})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := service.SubmitTurn(context.Background(), "turn", session.SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "queued"})
	if err != nil {
		t.Fatal(err)
	}
	req := session.CancelTurnRequest{SessionID: sess.ID, TurnID: turn.ID, ExpectedRevision: sess.Revision}
	cancelled, err := service.CancelTurn(context.Background(), "cancel-queued", req)
	if err != nil || cancelled.State != session.TurnCancelled {
		t.Fatalf("queued cancellation = %+v, err=%v", cancelled, err)
	}
	replayed, err := service.CancelTurn(context.Background(), "cancel-queued", req)
	if err != nil || replayed.ID != cancelled.ID || replayed.State != session.TurnCancelled {
		t.Fatalf("queued cancellation replay = %+v, err=%v", replayed, err)
	}
}

func TestSessionServiceCancelNaturalCompletionReplaysObservedTerminalTurn(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	started := make(chan struct{})
	release := make(chan struct{})
	var fakeStore *session.Store
	runner := SessionRunnerFunc(func(_ context.Context, bound session.Session, turn session.Turn) (session.Turn, error) {
		if _, err := fakeStore.MarkTurnSendIntent(context.Background(), bound.ID, turn.ID); err != nil {
			return turn, err
		}
		if _, err := fakeStore.MarkTurnSent(context.Background(), bound.ID, turn.ID); err != nil {
			return turn, err
		}
		close(started)
		<-release
		return fakeStore.CompleteTurn(context.Background(), session.CompleteTurnRequest{SessionID: bound.ID, TurnID: turn.ID, Message: "natural"})
	})
	service := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), testSessionPolicies(repo), func(store *session.Store) SessionRunner {
		fakeStore = store
		return runner
	})
	defer service.Stop()
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	sess, err := service.CreateRemoteSession(context.Background(), "create", CreateRemoteSessionRequest{Policy: "responder", Task: "natural-race"})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := service.SubmitTurn(context.Background(), "turn", session.SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "natural"})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	req := session.CancelTurnRequest{SessionID: sess.ID, TurnID: turn.ID, ExpectedRevision: sess.Revision}
	result := make(chan struct {
		turn session.Turn
		err  error
	}, 1)
	go func() {
		cancelled, err := service.CancelTurn(context.Background(), "cancel-natural", req)
		result <- struct {
			turn session.Turn
			err  error
		}{cancelled, err}
	}()
	waitForSessionTest(t, func() bool {
		service.mu.Lock()
		defer service.mu.Unlock()
		active := service.active[turn.ID]
		return active != nil && active.requested
	})
	close(release)
	response := <-result
	if response.err != nil || response.turn.State != session.TurnCompleted || response.turn.AssistantMessage != "natural" {
		t.Fatalf("natural completion won cancellation = %+v, err=%v", response.turn, response.err)
	}
	replayed, err := service.CancelTurn(context.Background(), "cancel-natural", req)
	if err != nil || replayed.ID != response.turn.ID || replayed.State != session.TurnCompleted || replayed.AssistantMessage != "natural" {
		t.Fatalf("natural completion cancellation replay = %+v, err=%v", replayed, err)
	}
}

func TestSessionServiceStopWaitsForWorkerBeforeClosingStore(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	started := make(chan struct{})
	release := make(chan struct{})
	runner := SessionRunnerFunc(func(ctx context.Context, _ session.Session, turn session.Turn) (session.Turn, error) {
		close(started)
		<-ctx.Done()
		<-release
		return turn, ctx.Err()
	})
	service, err := NewSessionService(SessionServiceConfig{
		StateRoot: filepath.Join(t.TempDir(), "state"), Policies: testSessionPolicies(repo),
		Runner: runner, StopTimeout: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := service.CreateRemoteSession(context.Background(), "create", CreateRemoteSessionRequest{Policy: "responder", Task: "stop"})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := service.SubmitTurn(context.Background(), "turn", session.SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "stop"}); err != nil {
		t.Fatal(err)
	}
	<-started
	stopped := make(chan error, 1)
	go func() { stopped <- service.Stop() }()
	select {
	case err := <-stopped:
		t.Fatalf("Stop returned while worker was still blocked: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if len(service.workers) != 0 || len(service.active) != 0 {
		t.Fatalf("stopped service retained workers=%d active=%d", len(service.workers), len(service.active))
	}
}

type startupCleaningRunner struct {
	cleaned []string
	reaped  []string
	err     error
	reapErr error
}

func (*startupCleaningRunner) Run(_ context.Context, _ session.Session, turn session.Turn) (session.Turn, error) {
	return turn, nil
}

func (r *startupCleaningRunner) CleanupSession(_ context.Context, sess session.Session) error {
	r.cleaned = append(r.cleaned, sess.ID)
	return r.err
}

func (r *startupCleaningRunner) ReapInterruptedTurn(_ context.Context, _ session.Session, turn session.Turn) error {
	r.reaped = append(r.reaped, turn.ID)
	return r.reapErr
}

func TestSessionServiceRunsStartupCleanupBeforeWorkers(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	runner := &startupCleaningRunner{}
	service, err := NewSessionService(SessionServiceConfig{
		StateRoot: filepath.Join(t.TempDir(), "state"),
		Policies:  testSessionPolicies(repo),
		Runner:    runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Stop()
	var newest string
	for i := 0; i < 105; i++ {
		sess, err := service.Store().CreateSession(context.Background(), fmt.Sprintf("create-%d", i), session.CreateSessionRequest{Target: "codex@work"})
		if err != nil {
			t.Fatal(err)
		}
		newest = sess.ID
	}
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runner.cleaned) != 105 || runner.cleaned[len(runner.cleaned)-1] != newest {
		t.Fatalf("startup cleanup count = %d newest=%q, want 105 and %q", len(runner.cleaned), runner.cleaned[len(runner.cleaned)-1], newest)
	}
}

func TestSessionServiceCleanupFailureDoesNotBrickStartup(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	runner := &startupCleaningRunner{err: errors.New("old provider is unavailable")}
	service, err := NewSessionService(SessionServiceConfig{
		StateRoot: filepath.Join(t.TempDir(), "state"),
		Policies:  testSessionPolicies(repo),
		Runner:    runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Stop()
	if _, err := service.Store().CreateSession(context.Background(), "create", session.CreateSessionRequest{Target: "removed-provider@old"}); err != nil {
		t.Fatal(err)
	}
	if err := service.Start(context.Background()); err != nil {
		t.Fatalf("startup failed because one historical cleanup failed: %v", err)
	}
}

type closedCleaningRunner struct {
	startupCalls atomic.Int32
	closedCalls  atomic.Int32
}

func (*closedCleaningRunner) Run(_ context.Context, _ session.Session, turn session.Turn) (session.Turn, error) {
	return turn, nil
}

func (r *closedCleaningRunner) CleanupSession(_ context.Context, _ session.Session) error {
	r.startupCalls.Add(1)
	return nil
}

func (r *closedCleaningRunner) CleanupClosedSession(_ context.Context, _ session.Session) error {
	r.closedCalls.Add(1)
	return nil
}

func TestSessionServiceCloseUsesKnownRuntimeCleanupWithoutStartupScan(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	runner := &closedCleaningRunner{}
	service, err := NewSessionService(SessionServiceConfig{
		StateRoot: filepath.Join(t.TempDir(), "state"),
		Policies:  testSessionPolicies(repo),
		Runner:    runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Stop()
	sess, err := service.CreateRemoteSession(context.Background(), "create-close-cleanup", CreateRemoteSessionRequest{
		Policy: "responder", Task: "close cleanup",
	})
	if err != nil {
		t.Fatal(err)
	}
	req := session.CloseSessionRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision}
	closed, err := service.Close(context.Background(), "close-cleanup", req)
	if err != nil || closed.State != session.SessionClosed {
		t.Fatalf("close = %+v, err=%v", closed, err)
	}
	if _, err := service.Close(context.Background(), "close-cleanup", req); err != nil {
		t.Fatalf("close replay = %v", err)
	}
	if got := runner.closedCalls.Load(); got != 1 {
		t.Fatalf("closed cleanup calls = %d, want 1", got)
	}
	if got := runner.startupCalls.Load(); got != 0 {
		t.Fatalf("startup cleanup calls during close = %d, want 0", got)
	}
}

type periodicCleanupRunner struct {
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
}

func (r *periodicCleanupRunner) Run(_ context.Context, _ session.Session, turn session.Turn) (session.Turn, error) {
	close(r.started)
	<-r.release
	return turn, errors.New("test turn complete")
}

func (r *periodicCleanupRunner) CleanupSession(_ context.Context, _ session.Session) error {
	r.calls.Add(1)
	return nil
}

func TestSessionServiceRetriesParkedCleanupWithoutRacingActiveTurn(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	runner := &periodicCleanupRunner{started: make(chan struct{}), release: make(chan struct{})}
	service, err := NewSessionService(SessionServiceConfig{
		StateRoot:       filepath.Join(t.TempDir(), "state"),
		Policies:        testSessionPolicies(repo),
		Runner:          runner,
		CleanupInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Stop()
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	sess, err := service.CreateRemoteSession(context.Background(), "create-periodic-cleanup", CreateRemoteSessionRequest{Policy: "responder", Task: "periodic cleanup"})
	if err != nil {
		t.Fatal(err)
	}
	waitForSessionTest(t, func() bool { return runner.calls.Load() > 0 })

	turn, err := service.SubmitTurn(context.Background(), "run-during-cleanup", session.SubmitTurnRequest{
		SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "run",
	})
	if err != nil {
		t.Fatal(err)
	}
	<-runner.started
	during := runner.calls.Load()
	time.Sleep(40 * time.Millisecond)
	if got := runner.calls.Load(); got != during {
		t.Fatalf("parked cleanup raced active turn: calls changed from %d to %d", during, got)
	}
	close(runner.release)
	waitForSessionTest(t, func() bool {
		got, err := service.GetTurn(context.Background(), sess.ID, turn.ID)
		return err == nil && got.State == session.TurnFailed
	})
	waitForSessionTest(t, func() bool { return runner.calls.Load() > during })
}

func TestSessionServiceWorkerRetiresParkedSessionAndStartsAgain(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	var fakeStore *session.Store
	var calls atomic.Int32
	secondStarted := make(chan struct{})
	releaseSecond := make(chan struct{})
	runner := SessionRunnerFunc(func(_ context.Context, bound session.Session, turn session.Turn) (session.Turn, error) {
		calls.Add(1)
		if _, err := fakeStore.MarkTurnSendIntent(context.Background(), bound.ID, turn.ID); err != nil {
			return turn, err
		}
		if _, err := fakeStore.MarkTurnSent(context.Background(), bound.ID, turn.ID); err != nil {
			return turn, err
		}
		if turn.Prompt == "second" {
			close(secondStarted)
			<-releaseSecond
		}
		return fakeStore.CompleteTurn(context.Background(), session.CompleteTurnRequest{SessionID: bound.ID, TurnID: turn.ID, Message: turn.Prompt})
	})
	service := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), testSessionPolicies(repo), func(store *session.Store) SessionRunner {
		fakeStore = store
		return runner
	})
	defer service.Stop()
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	sess, err := service.CreateRemoteSession(context.Background(), "create-worker-lifecycle", CreateRemoteSessionRequest{Policy: "responder", Task: "worker-lifecycle"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.SubmitTurn(context.Background(), "worker-first", session.SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "first"})
	if err != nil {
		t.Fatal(err)
	}
	waitForSessionTest(t, func() bool {
		got, err := service.GetTurn(context.Background(), sess.ID, first.ID)
		return err == nil && got.State == session.TurnCompleted
	})
	waitForSessionTest(t, func() bool {
		service.mu.Lock()
		defer service.mu.Unlock()
		return len(service.workers) == 0
	})
	current, err := service.GetSession(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.SubmitTurn(context.Background(), "worker-second", session.SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: current.Revision, Prompt: "second"})
	if err != nil {
		t.Fatal(err)
	}
	<-secondStarted
	service.mu.Lock()
	workerCount := len(service.workers)
	service.mu.Unlock()
	if workerCount != 1 {
		t.Fatalf("later turn worker count = %d, want a new worker", workerCount)
	}
	close(releaseSecond)
	waitForSessionTest(t, func() bool {
		got, err := service.GetTurn(context.Background(), sess.ID, second.ID)
		return err == nil && got.State == session.TurnCompleted
	})
	waitForSessionTest(t, func() bool {
		service.mu.Lock()
		defer service.mu.Unlock()
		return len(service.workers) == 0
	})
	if calls.Load() != 2 {
		t.Fatalf("runner calls = %d, want 2", calls.Load())
	}
}

func TestSessionServiceRecoveryCleanupFailureLeavesTurnActiveForRetry(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	runner := &startupCleaningRunner{reapErr: errors.New("runtime unavailable")}
	service, err := NewSessionService(SessionServiceConfig{
		StateRoot: filepath.Join(t.TempDir(), "state"), Policies: testSessionPolicies(repo), Runner: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Stop()
	sess, err := service.Store().CreateSession(context.Background(), "create-recovery", session.CreateSessionRequest{Target: "codex@work"})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := service.Store().SubmitTurn(context.Background(), "turn-recovery", session.SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "recover"})
	if err != nil {
		t.Fatal(err)
	}
	leased, ok, err := service.Store().LeaseNextTurn(context.Background(), sess.ID)
	if err != nil || !ok {
		t.Fatalf("lease recovery turn = %+v, ok=%v, err=%v", leased, ok, err)
	}
	if _, err := service.Store().MarkTurnSendIntent(context.Background(), sess.ID, turn.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.Start(context.Background()); err == nil {
		t.Fatal("startup succeeded despite interrupted runtime cleanup failure")
	}
	active, err := service.Store().GetTurn(context.Background(), sess.ID, turn.ID)
	if err != nil || active.State != session.TurnStarting {
		t.Fatalf("turn after failed recovery = %+v, err=%v", active, err)
	}
	runner.reapErr = nil
	if err := service.Start(context.Background()); err != nil {
		t.Fatalf("startup retry = %v", err)
	}
	recovered, err := service.Store().GetTurn(context.Background(), sess.ID, turn.ID)
	if err != nil || recovered.State != session.TurnInterrupted {
		t.Fatalf("recovered sent-intent turn = %+v, err=%v", recovered, err)
	}
	if len(runner.reaped) != 2 || runner.reaped[0] != turn.ID || runner.reaped[1] != turn.ID {
		t.Fatalf("reaped turns = %v, want two retries for %s", runner.reaped, turn.ID)
	}
}

func TestSessionServiceDiscardPlanAndReplay(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	service := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), testSessionPolicies(repo), nil)
	defer service.Stop()
	sess, err := service.CreateRemoteSession(context.Background(), "create", CreateRemoteSessionRequest{Policy: "responder", Task: "discard"})
	if err != nil {
		t.Fatal(err)
	}
	closed, err := service.Close(context.Background(), "close", session.CloseSessionRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.PlanDiscard(context.Background(), "plan", PlanDiscardRequest{SessionID: sess.ID, ExpectedRevision: closed.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if plan.OperationID == "" || plan.Plan.Workspace.Head == "" {
		t.Fatalf("discard plan = %+v", plan)
	}
	if _, err := service.PlanDiscard(context.Background(), "plan", PlanDiscardRequest{SessionID: sess.ID, ExpectedRevision: closed.Revision}); err != nil {
		t.Fatalf("plan replay = %v", err)
	}
	sessionWorkspaceGit(t, plan.Plan.Workspace.Workspace, "commit", "--allow-empty", "-qm", "head changed")
	if _, err := service.Discard(context.Background(), "stale-discard", DiscardRequest{PlanOperationID: plan.OperationID}); session.CodeOf(err) != session.CodeDiscardPlanStale {
		t.Fatalf("stale discard error = %v", err)
	}
	plan, err = service.PlanDiscard(context.Background(), "plan-2", PlanDiscardRequest{SessionID: sess.ID, ExpectedRevision: closed.Revision, AcceptUnmerged: true})
	if err != nil {
		t.Fatal(err)
	}
	discarded, err := service.Discard(context.Background(), "discard", DiscardRequest{PlanOperationID: plan.OperationID})
	if err != nil || discarded.State != session.SessionDiscarded {
		t.Fatalf("discard = %+v, err=%v", discarded, err)
	}
	replayed, err := service.Discard(context.Background(), "discard", DiscardRequest{PlanOperationID: plan.OperationID})
	if err != nil || replayed.ID != discarded.ID || pathExists(plan.Plan.Workspace.Workspace) {
		t.Fatalf("discard replay = %+v, err=%v", replayed, err)
	}
}

func TestSessionServiceDiscardReplayAfterWorkspaceRemovalBeforeTombstone(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	service := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), testSessionPolicies(repo), nil)
	defer service.Stop()
	sess, err := service.CreateRemoteSession(context.Background(), "create", CreateRemoteSessionRequest{Policy: "responder", Task: "discard-crash"})
	if err != nil {
		t.Fatal(err)
	}
	closed, err := service.Close(context.Background(), "close", session.CloseSessionRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.PlanDiscard(context.Background(), "plan", PlanDiscardRequest{SessionID: sess.ID, ExpectedRevision: closed.Revision})
	if err != nil {
		t.Fatal(err)
	}
	discardRequest := DiscardRequest{PlanOperationID: plan.OperationID}
	op, replay, err := service.Store().ReserveOperation(context.Background(), "Discard", "discard-crash", discardRequest)
	if err != nil || replay {
		t.Fatalf("reserve discard = %+v, replay=%v, err=%v", op, replay, err)
	}
	intent, err := json.Marshal(discardIntent{Plan: plan})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Store().MarkOperationRunning(context.Background(), op.ID, intent); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(plan.Plan.Workspace.Workspace); err != nil {
		t.Fatal(err)
	}
	discarded, err := service.Discard(context.Background(), "discard-crash", discardRequest)
	if err != nil || discarded.State != session.SessionDiscarded {
		t.Fatalf("replayed missing-workspace discard = %+v, err=%v", discarded, err)
	}
	replayed, err := service.Discard(context.Background(), "discard-crash", discardRequest)
	if err != nil || replayed.State != session.SessionDiscarded {
		t.Fatalf("discard tombstone replay = %+v, err=%v", replayed, err)
	}
}

func TestSessionServiceDiscardReplaysAfterPostDeleteCleanupFailure(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	stateRoot := filepath.Join(t.TempDir(), "state")
	service := newTestSessionService(t, stateRoot, testSessionPolicies(repo), nil)
	defer service.Stop()
	sess, err := service.CreateRemoteSession(context.Background(), "create-post-delete", CreateRemoteSessionRequest{Policy: "responder", Task: "post-delete"})
	if err != nil {
		t.Fatal(err)
	}
	closed, err := service.Close(context.Background(), "close-post-delete", session.CloseSessionRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.PlanDiscard(context.Background(), "plan-post-delete", PlanDiscardRequest{SessionID: sess.ID, ExpectedRevision: closed.Revision})
	if err != nil {
		t.Fatal(err)
	}
	privateRoot := filepath.Join(stateRoot, "acp")
	if err := os.MkdirAll(privateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	privateState := filepath.Join(privateRoot, sess.ID)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.Symlink(outside, privateState); err != nil {
		t.Fatal(err)
	}
	request := DiscardRequest{PlanOperationID: plan.OperationID}
	if _, err := service.Discard(context.Background(), "discard-post-delete", request); !errors.Is(err, session.ErrOperationUncertain) {
		t.Fatalf("post-delete cleanup error = %v, want operation_uncertain", err)
	}
	op, err := service.Store().GetOperation(context.Background(), "discard-post-delete")
	if err != nil || op.State != session.OperationRunning {
		t.Fatalf("post-delete operation = %+v, err=%v, want running", op, err)
	}
	if pathExists(plan.Plan.Workspace.Workspace) {
		t.Fatal("workspace survived before cleanup repair")
	}
	if err := os.Remove(privateState); err != nil {
		t.Fatal(err)
	}
	discarded, err := service.Discard(context.Background(), "discard-post-delete", request)
	if err != nil || discarded.State != session.SessionDiscarded {
		t.Fatalf("repaired discard replay = %+v, err=%v", discarded, err)
	}
	op, err = service.Store().GetOperation(context.Background(), "discard-post-delete")
	if err != nil || op.State != session.OperationSucceeded {
		t.Fatalf("repaired discard operation = %+v, err=%v", op, err)
	}
}

// mustTargets builds a policy's target ladder from wire-form literals.
func mustTargets(values ...string) []agents.Target {
	ladder := make([]agents.Target, len(values))
	for i, value := range values {
		target, err := agents.ParseTarget(value)
		if err != nil {
			panic(err)
		}
		ladder[i] = target
	}
	return ladder
}

func testSessionPolicies(repo string) map[string]SessionPolicy {
	return map[string]SessionPolicy{"responder": {
		Name: "responder", Repository: repo, Targets: mustTargets("codex@work"), MaxTurns: 10,
		MaxQueuedTurns: 5, MaxQueuedBytes: 1 << 20, MaxPatchBytes: 1 << 20, TurnTimeout: time.Second,
	}}
}

func newTestSessionService(t *testing.T, stateRoot string, policies map[string]SessionPolicy, factory SessionRunnerFactory) *SessionService {
	t.Helper()
	cfg := SessionServiceConfig{StateRoot: stateRoot, Policies: policies, RunnerFactory: factory}
	if factory == nil {
		cfg.Runner = SessionRunnerFunc(func(_ context.Context, _ session.Session, turn session.Turn) (session.Turn, error) {
			return turn, nil
		})
	}
	service, err := NewSessionService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func waitForSessionTest(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("session condition did not become true")
}

// Editing a policy must not orphan the sessions created under its previous
// shape. The digest guard stops a drifted policy from steering a running
// session; teardown steers nothing, and refusing it leaves workspaces nobody
// can ever reclaim — cleanup retried into permanent failure while forks leaked.
func TestSessionServiceDiscardsSessionsWhosePolicyDrifted(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	repo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(t.TempDir(), "state")

	before := newTestSessionService(t, stateRoot, testSessionPolicies(repo), nil)
	clean, err := before.CreateRemoteSession(
		context.Background(), "drift-create-clean",
		CreateRemoteSessionRequest{Policy: "responder", Task: "clean drift"},
	)
	if err != nil {
		t.Fatal(err)
	}
	unmerged, err := before.CreateRemoteSession(
		context.Background(), "drift-create-unmerged",
		CreateRemoteSessionRequest{Policy: "responder", Task: "unmerged drift"},
	)
	if err != nil {
		t.Fatal(err)
	}
	// Committed-but-unpublished work in the second fork: the safety the drift
	// fallback must not weaken.
	if err := os.WriteFile(filepath.Join(unmerged.Workspace, "wip.txt"), []byte("kept\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, unmerged.Workspace, "add", "wip.txt")
	runGitTest(t, unmerged.Workspace, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "wip")
	before.Stop()

	// The operator edited the policy target; every stored digest is now stale.
	edited := testSessionPolicies(repo)
	policy := edited["responder"]
	policy.Targets = mustTargets("codex:another-model@work")
	edited["responder"] = policy
	service := newTestSessionService(t, stateRoot, edited, nil)
	defer service.Stop()

	closed, err := service.Close(
		context.Background(), "drift-close-clean",
		session.CloseSessionRequest{SessionID: clean.ID, ExpectedRevision: clean.Revision},
	)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.PlanDiscard(
		context.Background(), "drift-plan-clean",
		PlanDiscardRequest{SessionID: clean.ID, ExpectedRevision: closed.Revision},
	)
	if err != nil {
		t.Fatalf("drifted session could not be planned for discard: %v", err)
	}
	discarded, err := service.Discard(
		context.Background(), "drift-discard-clean",
		DiscardRequest{PlanOperationID: plan.OperationID},
	)
	if err != nil || discarded.State != session.SessionDiscarded || pathExists(clean.Workspace) {
		t.Fatalf("drifted discard = %+v, %v (workspace present=%t)", discarded, err, pathExists(clean.Workspace))
	}

	closedUnmerged, err := service.Close(
		context.Background(), "drift-close-unmerged",
		session.CloseSessionRequest{SessionID: unmerged.ID, ExpectedRevision: unmerged.Revision},
	)
	if err != nil {
		t.Fatal(err)
	}
	guarded, err := service.PlanDiscard(
		context.Background(), "drift-plan-unmerged",
		PlanDiscardRequest{SessionID: unmerged.ID, ExpectedRevision: closedUnmerged.Revision},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !guarded.Plan.Workspace.Unmerged {
		t.Fatal("drift fallback lost the unmerged guard; committed work would be destroyed silently")
	}
}

// The ladder has to reach the runner through the real turn path, for the real
// sessions a deployment holds — including ones that survived a policy edit.
// The rotation logic had unit tests; what production hit was the wiring above
// it: a digest-strict guard that silently withheld the ladder from every
// surviving session, leaving them pinned to a rate-limited rung.
func TestSessionServiceHandsTheLadderToTurnsAcrossPolicyEdits(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	repo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(t.TempDir(), "state")
	withLadder := func(policies map[string]SessionPolicy, targets ...string) map[string]SessionPolicy {
		policy := policies["responder"]
		policy.Targets = mustTargets(targets...)
		policies["responder"] = policy
		return policies
	}
	var mu sync.Mutex
	ladders := map[string]int{} // prompt -> rungs seen by the runner
	factory := func(store *session.Store) SessionRunner {
		return SessionRunnerFunc(func(ctx context.Context, bound session.Session, turn session.Turn) (session.Turn, error) {
			ladder, _ := ctx.Value(sessionTargetLadderContextKey{}).([]agents.Target)
			mu.Lock()
			ladders[turn.Prompt] = len(ladder)
			mu.Unlock()
			if _, err := store.MarkTurnSendIntent(context.Background(), bound.ID, turn.ID); err != nil {
				return turn, err
			}
			if _, err := store.MarkTurnSent(context.Background(), bound.ID, turn.ID); err != nil {
				return turn, err
			}
			return store.CompleteTurn(context.Background(), session.CompleteTurnRequest{
				SessionID: bound.ID, TurnID: turn.ID, Message: turn.Prompt,
			})
		})
	}
	runTurn := func(service *SessionService, sess session.Session, key, prompt string) {
		t.Helper()
		turn, err := service.SubmitTurn(context.Background(), key, session.SubmitTurnRequest{
			SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: prompt,
		})
		if err != nil {
			t.Fatal(err)
		}
		waitForSessionTest(t, func() bool {
			got, err := service.GetTurn(context.Background(), sess.ID, turn.ID)
			return err == nil && got.State == session.TurnCompleted
		})
	}

	first, err := NewSessionService(SessionServiceConfig{
		StateRoot:     stateRoot,
		Policies:      withLadder(testSessionPolicies(repo), "codex@work", "codex:fallback-model@work"),
		RunnerFactory: factory,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	sess, err := first.CreateRemoteSession(
		context.Background(), "ladder-create",
		CreateRemoteSessionRequest{Policy: "responder", Task: "ladder plumbing"},
	)
	if err != nil {
		t.Fatal(err)
	}
	runTurn(first, sess, "ladder-turn-1", "current policy")
	first.Stop()

	// The operator raises max_turns: digest drifts, but the session's target is
	// still rung 0 of the current ladder — the blitz outage shape.
	drifted := withLadder(testSessionPolicies(repo), "codex@work", "codex:fallback-model@work")
	policy := drifted["responder"]
	policy.MaxTurns = policy.MaxTurns + 1
	drifted["responder"] = policy
	second, err := NewSessionService(SessionServiceConfig{StateRoot: stateRoot, Policies: drifted, RunnerFactory: factory})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	sess, err = second.GetSession(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	runTurn(second, sess, "ladder-turn-2", "drifted digest, target still a rung")
	second.Stop()

	// The operator replaces the ladder entirely: the session's target is on no
	// current rung, so rotation has nowhere legitimate to move it.
	replaced := withLadder(testSessionPolicies(repo), "codex:new-primary@work", "codex:new-fallback@work")
	third, err := NewSessionService(SessionServiceConfig{StateRoot: stateRoot, Policies: replaced, RunnerFactory: factory})
	if err != nil {
		t.Fatal(err)
	}
	if err := third.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	sess, err = third.GetSession(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	runTurn(third, sess, "ladder-turn-3", "target removed from the ladder")
	third.Stop()

	mu.Lock()
	defer mu.Unlock()
	if ladders["current policy"] != 2 {
		t.Fatalf("current-policy turn saw %d rungs, want 2", ladders["current policy"])
	}
	if ladders["drifted digest, target still a rung"] != 2 {
		t.Fatalf("drifted-digest turn saw %d rungs, want 2 — surviving sessions lost their fallback", ladders["drifted digest, target still a rung"])
	}
	if ladders["target removed from the ladder"] != 0 {
		t.Fatalf("off-ladder turn saw %d rungs, want 0 — rotation could steer onto rungs the session never held", ladders["target removed from the ladder"])
	}
}

// A session whose workspace has already vanished — crashed teardown, manual
// removal — must still be discardable through the normal plan/confirm flow.
// Refusing to plan was the gap that made such records permanent: cleanup
// retried into internal_error forever while nothing existed to reclaim.
func TestSessionServiceDiscardsASessionWhoseWorkspaceVanished(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	repo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	companion, companionGit := gitRepo(t)
	companionGit("commit", "-q", "--allow-empty", "-m", "companion base")
	companion, err = filepath.EvalSymlinks(companion)
	if err != nil {
		t.Fatal(err)
	}
	policies := testSessionPolicies(repo)
	policy := policies["responder"]
	policy.Companions = []SessionCompanionPolicy{{Name: "sidecar", Repository: companion}}
	policies["responder"] = policy
	service := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), policies, nil)
	defer service.Stop()

	ghost, err := service.CreateRemoteSession(
		context.Background(), "vanish-create",
		CreateRemoteSessionRequest{Policy: "responder", Task: "vanish"},
	)
	if err != nil {
		t.Fatal(err)
	}
	closed, err := service.Close(
		context.Background(), "vanish-close",
		session.CloseSessionRequest{SessionID: ghost.ID, ExpectedRevision: ghost.Revision},
	)
	if err != nil {
		t.Fatal(err)
	}
	// The whole point: fork and companion snapshot removed out of band.
	if err := os.RemoveAll(ghost.Workspace); err != nil {
		t.Fatal(err)
	}
	if len(ghost.Companions) != 1 {
		t.Fatalf("companions = %+v, want one", ghost.Companions)
	}
	if err := os.RemoveAll(ghost.Companions[0].Workspace); err != nil {
		t.Fatal(err)
	}
	plan, err := service.PlanDiscard(
		context.Background(), "vanish-plan",
		PlanDiscardRequest{SessionID: ghost.ID, ExpectedRevision: closed.Revision},
	)
	if err != nil {
		t.Fatalf("vanished workspace could not be planned: %v", err)
	}
	if plan.Plan.Workspace.Dirty || plan.Plan.Workspace.Unmerged {
		t.Fatalf("vanished workspace plan invented work to protect: %+v", plan.Plan.Workspace)
	}
	discarded, err := service.Discard(
		context.Background(), "vanish-discard",
		DiscardRequest{PlanOperationID: plan.OperationID},
	)
	if err != nil || discarded.State != session.SessionDiscarded {
		t.Fatalf("vanished discard = %+v, %v", discarded, err)
	}

	// Absence is the ONLY shortcut. A workspace that exists but fails
	// inspection may hold work, so it must keep failing loudly.
	broken, err := service.CreateRemoteSession(
		context.Background(), "broken-create",
		CreateRemoteSessionRequest{Policy: "responder", Task: "broken"},
	)
	if err != nil {
		t.Fatal(err)
	}
	closedBroken, err := service.Close(
		context.Background(), "broken-close",
		session.CloseSessionRequest{SessionID: broken.ID, ExpectedRevision: broken.Revision},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(broken.Workspace); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(broken.Workspace, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.PlanDiscard(
		context.Background(), "broken-plan",
		PlanDiscardRequest{SessionID: broken.ID, ExpectedRevision: closedBroken.Revision},
	); err == nil {
		t.Fatal("a corrupted-but-present workspace planned as if absent")
	}
}
