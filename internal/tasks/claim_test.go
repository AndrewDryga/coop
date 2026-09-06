package tasks

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/processidentity"
)

func fakeClaimProbe(tree map[int]int, names, tokens map[int]string) claimActorProbe {
	return claimActorProbe{
		parent:  func(pid int) int { return tree[pid] },
		command: func(pid int) string { return names[pid] },
		token:   func(pid int) string { return tokens[pid] },
	}
}

// selfActor is this test process as a claim actor: a live process with a readable identity.
func selfActor(t *testing.T, label string) ClaimActor {
	t.Helper()
	token := processidentity.StartToken(os.Getpid())
	if !processidentity.Stable(token) {
		t.Skip("no stable process identity on this platform")
	}
	return ClaimActor{Label: label, PID: os.Getpid(), StartToken: token}
}

// sleeperActor starts a child that lives until the returned stop func runs, so a test has a second
// live process — and, after stop, a provably dead one — to bind claims to.
func sleeperActor(t *testing.T, label string) (ClaimActor, func()) {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a sleeper: %v", err)
	}
	token := processidentity.StartToken(cmd.Process.Pid)
	if !processidentity.Stable(token) {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Skip("no stable process identity on this platform")
	}
	var once bool
	stop := func() {
		if once {
			return
		}
		once = true
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	t.Cleanup(stop)
	return ClaimActor{Label: label, PID: cmd.Process.Pid, StartToken: token}, stop
}

func TestCaptureClaimActorBindsPastShellsAndNeverToATerminalUser(t *testing.T) {
	tree := map[int]int{400: 300, 300: 200, 200: 100, 100: 1}
	names := map[int]string{400: "sh", 300: "-zsh", 200: "claude", 100: "launchd"}
	tokens := map[int]string{}
	for pid := range names {
		tokens[pid] = fmt.Sprintf("darwin-kinfo-v1:%d:0", pid)
	}
	probe := fakeClaimProbe(tree, names, tokens)
	agent := ClaimActor{Label: "claude", PID: 200, StartToken: tokens[200]}
	for name, tc := range map[string]struct {
		start       int
		interactive bool
		override    ClaimActor
		want        ClaimActor
	}{
		"shell parent walks to the agent":   {300, false, ClaimActor{}, agent},
		"nested shells keep walking":        {400, false, ClaimActor{}, agent},
		"non-shell parent binds directly":   {200, false, ClaimActor{}, agent},
		"--as names the actor":              {300, false, ClaimActor{Label: "codex@zed"}, ClaimActor{Label: "codex@zed", PID: 200, StartToken: tokens[200]}},
		"a terminal user binds to nothing":  {300, true, ClaimActor{}, ClaimActor{}},
		"a terminal user with --pid binds":  {300, true, ClaimActor{PID: 200}, agent},
		"an unreadable identity is dropped": {300, false, ClaimActor{PID: 999}, ClaimActor{}},
		"a bad label is dropped":            {200, false, ClaimActor{Label: "not ok"}, ClaimActor{Label: "", PID: 200, StartToken: tokens[200]}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := captureClaimActor(probe, tc.start, tc.interactive, tc.override); got != tc.want {
				t.Fatalf("captureClaimActor = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// The real walk: this test re-runs itself under `sh -c` with a trailing command, so the child's
// parent is a real shell that stays alive and its grandparent is this process.
func TestCaptureClaimActorFromARealShell(t *testing.T) {
	if want := os.Getenv("COOP_TEST_CLAIM_ACTOR_GRANDPARENT"); want != "" {
		actor := captureClaimActor(realClaimActorProbe, os.Getppid(), false, ClaimActor{})
		fmt.Printf("CLAIM_ACTOR parent=%s pid=%d label=%s stable=%t want=%s\n",
			processidentity.Command(os.Getppid()), actor.PID, actor.Label, processidentity.Stable(actor.StartToken), want)
		return
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	if !processidentity.Stable(processidentity.StartToken(os.Getpid())) {
		t.Skip("no stable process identity on this platform")
	}
	cmd := exec.Command("sh", "-c", os.Args[0]+" -test.run='^TestCaptureClaimActorFromARealShell$' -test.v; true")
	cmd.Env = append(os.Environ(), "COOP_TEST_CLAIM_ACTOR_GRANDPARENT="+strconv.Itoa(os.Getpid()))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child run: %v\n%s", err, out)
	}
	line := ""
	for _, l := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(l, "CLAIM_ACTOR ") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("child printed no actor line:\n%s", out)
	}
	want := fmt.Sprintf("pid=%d", os.Getpid())
	if !strings.Contains(line, want) || !strings.Contains(line, "stable=true") {
		t.Fatalf("child bound to %q, want the grandparent %s:\n%s", line, want, out)
	}
	parent := strings.TrimPrefix(strings.Fields(line)[1], "parent=")
	if !isShellCommand(parent) {
		t.Fatalf("the child's parent was %q, not a shell — the walk was not exercised", parent)
	}
}

func TestClaimBindsToAProcessAndRefusesACompetingLiveClaim(t *testing.T) {
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, StateTodo, "shared", "task.md"), "# Shared\n")
	me := selfActor(t, "codex")
	claim := func(actor ClaimActor, force bool) (int, error) {
		return tasksFolderMoveWith(root, []string{"shared"}, StateInProgress, "claim", "claimed", claimOptions{actor: actor, force: force})
	}
	if code, err := claim(me, false); code != 0 || err != nil {
		t.Fatalf("first claim = %d, %v", code, err)
	}
	rec, ok := taskOwned(t, root, "shared")
	if !ok || rec.Actor != "codex" || rec.ActorPID != os.Getpid() || rec.ActorStart != me.StartToken {
		t.Fatalf("owner record after a bound claim = %+v, %t", rec, ok)
	}
	if out := captureStdout(t, func() { _, _ = tasksFolderList(root, false) }); !strings.Contains(out, fmt.Sprintf("claimed by codex (pid %d)", os.Getpid())) || strings.Contains(out, "gone") {
		t.Fatalf("listing of a live bound claim:\n%s", out)
	}

	other, stop := sleeperActor(t, "claude")
	if code, err := claim(other, false); code != 1 || !errors.Is(err, errTaskClaimedByOther) {
		t.Fatalf("competing live claim = %d, %v; want a refusal", code, err)
	}
	if rec, _ := taskOwned(t, root, "shared"); rec.ActorPID != os.Getpid() {
		t.Fatalf("a refused claim must leave the owner alone, got %+v", rec)
	}
	if code, err := claim(other, true); code != 0 || err != nil {
		t.Fatalf("--force takeover = %d, %v", code, err)
	}
	if rec, _ := taskOwned(t, root, "shared"); rec.ActorPID != other.PID || rec.Actor != "claude" {
		t.Fatalf("owner after --force = %+v", rec)
	}
	if code, err := claim(other, false); code != 0 || err != nil {
		t.Fatalf("re-claim by the same process = %d, %v; want idempotent", code, err)
	}

	stop()
	if out := captureStdout(t, func() { _, _ = tasksFolderList(root, false) }); !strings.Contains(out, "owner process gone") {
		t.Fatalf("listing after the owner died must say so:\n%s", out)
	}
	if code, err := claim(me, false); code != 0 || err != nil {
		t.Fatalf("claim over a dead owner = %d, %v; want a takeover without --force", code, err)
	}
	if rec, _ := taskOwned(t, root, "shared"); rec.ActorPID != os.Getpid() {
		t.Fatalf("owner after the takeover = %+v", rec)
	}
}

func TestReleaseGoneOwnersReleasesOnlyDeadProcessBoundClaims(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"dead", "alive", "person"} {
		writeTaskFile(t, filepath.Join(root, StateTodo, id, "task.md"), "# "+id+"\n")
	}
	dead, stop := sleeperActor(t, "codex")
	for id, actor := range map[string]ClaimActor{"dead": dead, "alive": selfActor(t, "claude"), "person": {}} {
		if code, err := tasksFolderMoveWith(root, []string{id}, StateInProgress, "claim", "claimed", claimOptions{actor: actor}); code != 0 || err != nil {
			t.Fatalf("claim %s = %d, %v", id, code, err)
		}
	}
	stop()
	ids, err := ReleaseGoneOwners([]string{root})
	if err != nil || len(ids) != 1 || ids[0] != "dead" {
		t.Fatalf("ReleaseGoneOwners = %v, %v; want only the dead owner's task", ids, err)
	}
	if _, owned := taskOwned(t, root, "dead"); owned {
		t.Fatal("the dead owner's claim must be released")
	}
	for _, id := range []string{"alive", "person"} {
		if _, owned := taskOwned(t, root, id); !owned {
			t.Fatalf("%s must keep its claim", id)
		}
	}
	log, err := os.ReadFile(filepath.Join(root, StateInProgress, "dead", "log.md"))
	if err != nil || !strings.Contains(string(log), "preflight: released the claim") {
		t.Fatalf("release must be journaled in the task log, got %q, %v", log, err)
	}
	if ids, err := ReleaseGoneOwners([]string{root}); err != nil || len(ids) != 0 {
		t.Fatalf("second pass = %v, %v; want nothing left to release", ids, err)
	}
}

func TestCmdTasksFolderClaimFlags(t *testing.T) {
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, StateTodo, "flags", "task.md"), "# Flags\n")
	me := selfActor(t, "codex")
	if code, err := CmdTasksFolder("", root, []string{"claim", "flags", "--as", "codex", "--pid", strconv.Itoa(me.PID)}); code != 0 || err != nil {
		t.Fatalf("claim with flags = %d, %v", code, err)
	}
	if rec, ok := taskOwned(t, root, "flags"); !ok || rec.Actor != "codex" || rec.ActorPID != me.PID {
		t.Fatalf("owner record = %+v, %t", rec, ok)
	}
	for name, args := range map[string][]string{
		"unknown flag":    {"claim", "flags", "--bogus"},
		"bad label":       {"claim", "flags", "--as", "not ok"},
		"bad pid":         {"claim", "flags", "--pid", "zero"},
		"no id":           {"claim", "--as", "codex"},
		"two ids":         {"claim", "flags", "again"},
		"value-less --as": {"claim", "flags", "--as"},
	} {
		if code, err := CmdTasksFolder("", root, args); code != 2 || err == nil {
			t.Errorf("%s: %v = %d, %v; want a usage error", name, args, code, err)
		}
	}
	if code, err := CmdTasksFolder("", root, []string{"claim", "flags", "--pid", "999999999"}); code != 1 || err == nil || !strings.Contains(err.Error(), "999999999") {
		t.Fatalf("claim bound to a missing process = %d, %v; want a refusal naming the pid", code, err)
	}
}

func TestOwnerRecordRejectsMalformedActorIdentity(t *testing.T) {
	base := TaskOwnerRecord{
		Version: taskOwnerRecordVersion, TaskID: "x", Source: taskOwnerSourceInteractiveClaim,
		User: "alice", Host: "h", ClaimedAt: time.Now(),
	}
	legacy := base
	legacy.ActorPID = 12
	if err := validateTaskOwnerRecord(legacy, "x"); err == nil {
		t.Fatal("a legacy record must not carry an actor identity")
	}
	for name, mutate := range map[string]func(*TaskOwnerRecord){
		"pid without a token": func(r *TaskOwnerRecord) { r.ActorPID = 12 },
		"token without a pid": func(r *TaskOwnerRecord) { r.ActorStart = "darwin-kinfo-v1:1:1" },
		"unstable token":      func(r *TaskOwnerRecord) { r.ActorPID, r.ActorStart = 12, "made-up" },
		"reserved pid":        func(r *TaskOwnerRecord) { r.ActorPID, r.ActorStart = 1, "darwin-kinfo-v1:1:1" },
		"unprintable label":   func(r *TaskOwnerRecord) { r.Actor = "a b" },
	} {
		t.Run(name, func(t *testing.T) {
			r := humanOwnerRecordForTest("x")
			mutate(&r)
			if err := validateTaskOwnerRecord(r, "x"); err == nil {
				t.Fatal("malformed actor identity was accepted")
			}
		})
	}
	good := humanOwnerRecordForTest("x")
	good.Actor, good.ActorPID, good.ActorStart = "codex@zed", 12, "darwin-kinfo-v1:1:1"
	if err := validateTaskOwnerRecord(good, "x"); err != nil {
		t.Fatalf("a well-formed bound claim was rejected: %v", err)
	}
}

// humanOwnerRecordForTest is a well-formed version-2 human claim on id, ready for one mutation.
func humanOwnerRecordForTest(id string) TaskOwnerRecord {
	return TaskOwnerRecord{
		Version: taskOwnershipRecordVersion, TaskID: id, Kind: TaskOwnerHuman,
		Task: &TaskInstance{
			Ref:        TaskRef{QueueID: strings.Repeat("a", 32), TaskID: strings.Repeat("b", 32), ID: id},
			Generation: TaskGeneration{Device: 1, Inode: 2},
		},
		Source: taskOwnerSourceInteractiveClaim, User: "alice", Host: "h", ClaimedAt: time.Now(),
	}
}
