package tasks

import (
	"context"
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
	"github.com/AndrewDryga/coop/internal/testutil/starttoken"
	"github.com/AndrewDryga/coop/internal/ui"
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
	if out := captureStdout(t, func() { _, _ = tasksFolderList(root, false) }); !strings.Contains(out, fmt.Sprintf("claimed by codex (PID %d)", os.Getpid())) || strings.Contains(out, "has stopped") {
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
	if out := captureStdout(t, func() { _, _ = tasksFolderList(root, false) }); !strings.Contains(out, "⚠ Owner process has stopped") {
		t.Fatalf("listing after the owner died must say so:\n%s", out)
	}
	if code, err := claim(me, false); code != 0 || err != nil {
		t.Fatalf("claim over a dead owner = %d, %v; want a takeover without --force", code, err)
	}
	if rec, _ := taskOwned(t, root, "shared"); rec.ActorPID != os.Getpid() {
		t.Fatalf("owner after the takeover = %+v", rec)
	}
}

// claim --force over a live claim ends the lease the displaced process holds, so the task has one
// holder, and says whose claim it took, in its output and in log.md.
func TestAForcedClaimEndsTheDisplacedLeaseAndNamesItsHolder(t *testing.T) {
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, StateTodo, "second", "task.md"), "# Second\n")
	qa, _ := sleeperActor(t, "qa")
	me := selfActor(t, "claude")
	claim := func(actor ClaimActor, force bool) (int, error) {
		return tasksFolderMoveWith(root, []string{"second"}, StateInProgress, "claim", "claimed", claimOptions{actor: actor, force: force})
	}
	if code, err := claim(qa, false); code != 0 || err != nil {
		t.Fatalf("qa's claim = %d, %v", code, err)
	}
	item, _ := mustCurrentTask(t, root, "second")
	holderOutput := startLeaseHolderProcess(t, root, "second", qa)

	out := captureStderr(t, func() {
		if code, err := claim(me, true); code != 0 || err != nil {
			t.Fatalf("forced claim = %d, %v", code, err)
		}
	})
	if want := fmt.Sprintf("  Took over from qa (PID %d) and ended its lease.\n", qa.PID); !strings.Contains(out, want) {
		t.Fatalf("forced claim output =\n%s\nwant it to contain %q", out, want)
	}
	if observed := observeTaskLease(item, time.Now()); observed.State != leaseUnleased {
		t.Fatalf("the displaced lease is still held after the forced claim: %+v", observed)
	}
	want := fmt.Sprintf("✓ Lease on second ended: this holder was stopped\n  claude (PID %d) took the task over.\n", me.PID)
	if got := holderOutput(); !strings.Contains(got, want) {
		t.Fatalf("displaced holder output =\n%s\nwant it to contain\n%s", got, want)
	}
	logWant := fmt.Sprintf("- Claimed by claude (PID %d), taking it over from qa (PID %d)\n", me.PID, qa.PID)
	if log := readFileString(filepath.Join(item.Dir, "log.md")); !strings.Contains(log, logWant) {
		t.Fatalf("log.md =\n%s\nwant it to contain %q", log, logWant)
	}
	listing := captureStdout(t, func() { _, _ = tasksFolderList(root, false) })
	if !strings.Contains(listing, fmt.Sprintf("claimed by claude (PID %d)", me.PID)) || strings.Contains(listing, "Reserved by") {
		t.Fatalf("listing after the forced claim must show one holder:\n%s", listing)
	}
}

// An agent's claim over a person's says whose it took, in its output and in log.md, as it does over
// a process's, forced or not. A person has no process, so no lease ends and nothing is marked: the
// mark an earlier forced claim left on a process stays. A person who claims again what is theirs
// takes it from nobody.
func TestAnAgentsClaimOverAPersonsClaimNamesThePerson(t *testing.T) {
	t.Setenv("USER", "alice")
	me := selfActor(t, "claude")
	qa, _ := sleeperActor(t, "qa")
	for name, force := range map[string]bool{"forced": true, "plain": false} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeTaskFile(t, filepath.Join(root, StateTodo, "x", "task.md"), "# X\n")
			claim := func(actor ClaimActor, force bool) string {
				t.Helper()
				return captureStderr(t, func() {
					if code, err := tasksFolderMoveWith(root, []string{"x"}, StateInProgress, "claim", "claimed", claimOptions{actor: actor, force: force}); code != 0 || err != nil {
						t.Fatalf("claim by %+v (force %t) = %d, %v", actor, force, code, err)
					}
				})
			}
			claim(qa, false)
			claim(ClaimActor{}, true) // alice, at a terminal, takes it from qa, which is then marked as displaced

			out := claim(me, force)
			if want := "  Took over from alice.\n"; !strings.Contains(out, want) || strings.Contains(out, "Already in progress") {
				t.Fatalf("claim output =\n%s\nwant it to say %q", out, want)
			}
			item, _ := mustCurrentTask(t, root, "x")
			want := fmt.Sprintf("- Claimed by claude (PID %d), taking it over from alice\n", me.PID)
			if log := readFileString(filepath.Join(item.Dir, "log.md")); !strings.Contains(log, want) {
				t.Fatalf("log.md =\n%s\nwant it to contain %q", log, want)
			}
			if rec, _ := taskOwned(t, root, "x"); rec.ActorPID != me.PID || rec.TakenFromPID != qa.PID {
				t.Fatalf("owner after the claim = %+v, want claude's claim with qa still marked as displaced", rec)
			}
		})
	}

	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, StateTodo, "mine", "task.md"), "# Mine\n")
	for range 2 {
		out := captureStderr(t, func() {
			if code, err := tasksFolderMoveWith(root, []string{"mine"}, StateInProgress, "claim", "claimed", claimOptions{}); code != 0 || err != nil {
				t.Fatalf("a person's claim = %d, %v", code, err)
			}
		})
		if strings.Contains(out, "Took over") {
			t.Fatalf("a person's own claim says it took the task from someone:\n%s", out)
		}
	}
}

// A forced claim takes the task from the displaced process for good: that process can no longer
// complete it, lease it or claim it back without --force, whether an agent forced the claim or a
// person at a terminal, whose claim binds to no process.
func TestAProcessAForcedClaimDisplacedCannotCompleteTheTask(t *testing.T) {
	qa, _ := sleeperActor(t, "qa")
	for name, forcer := range map[string]ClaimActor{"agent": selfActor(t, "claude"), "person": {}} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("USER", "alice")
			root := t.TempDir()
			writeTaskFile(t, filepath.Join(root, StateTodo, "x", "task.md"), "# X\n")
			claim := func(actor ClaimActor, force bool) (int, error) {
				return tasksFolderMoveWith(root, []string{"x"}, StateInProgress, "claim", "claimed", claimOptions{actor: actor, force: force})
			}
			done := func(actor ClaimActor) (int, error) {
				return tasksFolderMoveWith(root, []string{"x"}, StateDone, "done", "done", claimOptions{actor: actor})
			}
			if code, err := claim(qa, false); code != 0 || err != nil {
				t.Fatalf("qa's claim = %d, %v", code, err)
			}
			item, _ := mustCurrentTask(t, root, "x")
			if err := RewriteSubtasks(item.Dir, []Subtask{{Text: "the work", Done: true}}); err != nil {
				t.Fatal(err)
			}
			if code, err := claim(forcer, true); code != 0 || err != nil {
				t.Fatalf("forced claim = %d, %v", code, err)
			}

			holder := fmt.Sprintf("claude (PID %d)", forcer.PID)
			if forcer.PID == 0 {
				holder = "alice"
			}
			cause := fmt.Sprintf("      %s took it over from qa (PID %d).\n      It is theirs now, so stop working on it.\n", holder, qa.PID)
			for verb, try := range map[string]func() (int, error){
				"complete": func() (int, error) { return done(qa) },
				"claim":    func() (int, error) { return claim(qa, false) },
				"lease": func() (int, error) {
					return holdTaskLease(context.Background(), root, leaseRequest{id: "x", actor: qa}, time.Millisecond)
				},
			} {
				code, err := try()
				var usage *ui.UsageError
				if code == 0 || !errors.Is(err, ErrTaskTakenOver) || !errors.As(err, &usage) {
					t.Fatalf("%s by the displaced process = %d, %v; want a refusal", verb, code, err)
				}
				if want := "\n✗ Could not " + verb + " x\n\n" + cause; usage.Render(ui.Palette{}) != want {
					t.Fatalf("%s refusal:\n%s\nwant:\n%s", verb, usage.Render(ui.Palette{}), want)
				}
			}
			if current, ok := mustCurrentTask(t, root, "x"); !ok || current.State != StateInProgress {
				t.Fatalf("a refused completion moved the task: %+v", current)
			}
			if rec, _ := taskOwned(t, root, "x"); rec.ActorPID != forcer.PID || rec.TakenFromPID != qa.PID {
				t.Fatalf("owner after the refusals = %+v, want the forced claim kept", rec)
			}
			if code, err := done(forcer); code != 0 || err != nil {
				t.Fatalf("done by the new holder = %d, %v", code, err)
			}
		})
	}
}

// A process that lost a task to --force may take it back the same way; the task is then its own.
func TestADisplacedProcessTakesTheTaskBackOnlyWithForce(t *testing.T) {
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, StateTodo, "x", "task.md"), "# X\n")
	qa, _ := sleeperActor(t, "qa")
	claim := func(actor ClaimActor, force bool) (int, error) {
		return tasksFolderMoveWith(root, []string{"x"}, StateInProgress, "claim", "claimed", claimOptions{actor: actor, force: force})
	}
	for _, step := range []struct {
		actor ClaimActor
		force bool
	}{{qa, false}, {ClaimActor{}, true}, {qa, true}} {
		if code, err := claim(step.actor, step.force); code != 0 || err != nil {
			t.Fatalf("claim by %+v (force %t) = %d, %v", step.actor, step.force, code, err)
		}
	}
	if rec, _ := taskOwned(t, root, "x"); rec.ActorPID != qa.PID || rec.TakenFromPID != 0 {
		t.Fatalf("owner after qa took it back = %+v, want qa's claim with no mark", rec)
	}
	item, _ := mustCurrentTask(t, root, "x")
	if err := RewriteSubtasks(item.Dir, []Subtask{{Text: "the work", Done: true}}); err != nil {
		t.Fatal(err)
	}
	if code, err := tasksFolderMoveWith(root, []string{"x"}, StateDone, "done", "done", claimOptions{actor: qa}); code != 0 || err != nil {
		t.Fatalf("done by qa after taking it back = %d, %v", code, err)
	}
}

// After a forced claim the displaced process can no longer block or release the task either, whether
// an agent forced it or a person, whose claim binds to no process and so is no live claim to refuse
// for. It is told who holds the task now, in done's words, and takes it back only with --force.
func TestAProcessAForcedClaimDisplacedCannotBlockOrReleaseTheTask(t *testing.T) {
	qa, _ := sleeperActor(t, "qa")
	for name, forcer := range map[string]ClaimActor{"agent": selfActor(t, "claude"), "person": {}} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("USER", "alice")
			root := t.TempDir()
			writeTaskFile(t, filepath.Join(root, StateTodo, "x", "task.md"), "# X\n")
			claim := func(actor ClaimActor, force bool) {
				t.Helper()
				if code, err := tasksFolderMoveWith(root, []string{"x"}, StateInProgress, "claim", "claimed", claimOptions{actor: actor, force: force}); code != 0 || err != nil {
					t.Fatalf("claim by %+v (force %t) = %d, %v", actor, force, code, err)
				}
			}
			claim(qa, false)
			claim(forcer, true)
			holder := fmt.Sprintf("claude (PID %d)", forcer.PID)
			if forcer.PID == 0 {
				holder = "alice"
			}
			cause := fmt.Sprintf("      %s took it over from qa (PID %d).\n      It is theirs now, so stop working on it.\n", holder, qa.PID)
			item, _ := mustCurrentTask(t, root, "x")
			for verb, try := range map[string]func() error{
				"block":   func() error { return BlockTrustedTask(root, item, qa) },
				"release": func() error { return ReleaseTrustedTask(root, item, qa) },
			} {
				err := try()
				var usage *ui.UsageError
				if !errors.Is(err, ErrTaskTakenOver) || !errors.As(err, &usage) {
					t.Fatalf("%s by the displaced process = %v; want the takeover refusal", verb, err)
				}
				if want := "\n✗ Could not " + verb + " x\n\n" + cause; usage.Render(ui.Palette{}) != want {
					t.Fatalf("%s refusal:\n%s\nwant:\n%s", verb, usage.Render(ui.Palette{}), want)
				}
			}
			if current, ok := mustCurrentTask(t, root, "x"); !ok || current.State != StateInProgress {
				t.Fatalf("a refused block or release moved the task: %+v", current)
			}
			if rec, _ := taskOwned(t, root, "x"); rec.ActorPID != forcer.PID || rec.TakenFromPID != qa.PID {
				t.Fatalf("owner after the refusals = %+v, want the forced claim kept", rec)
			}
			claim(qa, true)
			if err := BlockTrustedTask(root, item, qa); err != nil {
				t.Fatalf("block after taking the task back = %v", err)
			}
		})
	}
}

// A forced claim whose own process has stopped protects nothing, like any stopped claim, so the
// process it displaced may hand the task back as anyone may.
func TestADisplacedProcessMayReleaseOnceTheForcedClaimStopped(t *testing.T) {
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, StateTodo, "x", "task.md"), "# X\n")
	qa, _ := sleeperActor(t, "qa")
	forcer, stop := sleeperActor(t, "claude")
	for _, step := range []struct {
		actor ClaimActor
		force bool
	}{{qa, false}, {forcer, true}} {
		if code, err := tasksFolderMoveWith(root, []string{"x"}, StateInProgress, "claim", "claimed", claimOptions{actor: step.actor, force: step.force}); code != 0 || err != nil {
			t.Fatalf("claim by %+v = %d, %v", step.actor, code, err)
		}
	}
	stop()
	item, _ := mustCurrentTask(t, root, "x")
	if err := ReleaseTrustedTask(root, item, qa); err != nil {
		t.Fatalf("release once the forced claim stopped = %v", err)
	}
	if current, _ := mustCurrentTask(t, root, "x"); current.State != StateTodo {
		t.Fatalf("task after the release = %s, want todo", current.State)
	}
}

// A claim or release refused for another live process's claim names that process and offers the
// one command that gets past it: taking the claim over. Release itself would be refused again.
func TestLiveClaimRefusalsNameTheHolderAndOfferOnlyTheTakeover(t *testing.T) {
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, StateTodo, "second", "task.md"), "# Second\n")
	qa, _ := sleeperActor(t, "qa")
	me := selfActor(t, "claude")
	claim := func(actor ClaimActor, force bool) (int, error) {
		return tasksFolderMoveWith(root, []string{"second"}, StateInProgress, "claim", "claimed", claimOptions{actor: actor, force: force})
	}
	if code, err := claim(qa, false); code != 0 || err != nil {
		t.Fatalf("qa's claim = %d, %v", code, err)
	}
	cause := fmt.Sprintf("      It is claimed by qa (PID %d), which is still running.\n", qa.PID)
	code, err := claim(me, false)
	var usage *ui.UsageError
	if code != 1 || !errors.Is(err, errTaskClaimedByOther) || !errors.As(err, &usage) {
		t.Fatalf("claim over a live claim = %d, %v; want a drawn refusal", code, err)
	}
	if want := "\n✗ Could not claim second\n\n" + cause + "\n  Take it over: coop tasks claim second --force\n"; usage.Render(ui.Palette{}) != want {
		t.Fatalf("claim refusal:\n%s\nwant:\n%s", usage.Render(ui.Palette{}), want)
	}
	item, _ := mustCurrentTask(t, root, "second")
	err = ReleaseTrustedTask(root, item, me)
	if !errors.Is(err, errTaskClaimedByOther) || !errors.As(err, &usage) {
		t.Fatalf("release of a live claim = %v; want a drawn refusal", err)
	}
	want := "\n✗ Could not release second\n\n" + cause +
		"\n  Take it over:    coop tasks claim second --force\n  Then release it: coop tasks release second\n"
	if got := usage.Render(ui.Palette{}); got != want {
		t.Fatalf("release refusal:\n%s\nwant:\n%s", got, want)
	}
	// The commands offered work, in that order.
	if code, err := claim(me, true); code != 0 || err != nil {
		t.Fatalf("the offered takeover = %d, %v", code, err)
	}
	item, _ = mustCurrentTask(t, root, "second")
	if err := ReleaseTrustedTask(root, item, me); err != nil {
		t.Fatalf("the offered release = %v", err)
	}
	if current, _ := mustCurrentTask(t, root, "second"); current.State != StateTodo {
		t.Fatalf("task after the release = %s, want todo", current.State)
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

// A claim bound to a process another operating system recorded names a pid this kernel cannot
// check, even when no process here has that pid. It counts as live wherever coop would otherwise
// drop it: pre-flight keeps it, a new claim needs --force, and the listing says whose pid it is.
func TestAClaimFromAnotherSystemIsKept(t *testing.T) {
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, StateTodo, "far", "task.md"), "# Far\n")
	far := ClaimActor{Label: "codex", PID: 2147483646, StartToken: starttoken.Foreign()}
	if code, err := tasksFolderMoveWith(root, []string{"far"}, StateInProgress, "claim", "claimed", claimOptions{actor: far}); code != 0 || err != nil {
		t.Fatalf("claim = %d, %v", code, err)
	}
	if ids, err := ReleaseGoneOwners([]string{root}); err != nil || len(ids) != 0 {
		t.Fatalf("ReleaseGoneOwners = %v, %v; want another system's claim kept", ids, err)
	}
	want := fmt.Sprintf("claimed by codex (PID 2147483646 on %s)", processidentity.System(far.StartToken))
	if out := captureStdout(t, func() { _, _ = tasksFolderList(root, false) }); !strings.Contains(out, want) || strings.Contains(out, "has stopped") {
		t.Fatalf("listing of another system's claim, want %q:\n%s", want, out)
	}
	if code, err := tasksFolderMoveWith(root, []string{"far"}, StateInProgress, "claim", "claimed", claimOptions{actor: selfActor(t, "claude")}); code != 1 || !errors.Is(err, errTaskClaimedByOther) {
		t.Fatalf("claim over another system's claim = %d, %v; want a refusal", code, err)
	}
	if rec, ok := taskOwned(t, root, "far"); !ok || rec.ActorPID != far.PID || rec.ActorStart != far.StartToken {
		t.Fatalf("owner after the refused claim = %+v, %t", rec, ok)
	}
	// Coop cannot tell whether that process still runs, so a release says how to take the task over.
	take := "which Coop cannot check from here. — Take it over: coop tasks claim far --force"
	if code, err := CmdTasksFolder("", root, []string{"release", "far"}); code == 0 || !errors.Is(err, errTaskClaimedByOther) || !strings.Contains(err.Error(), take) {
		t.Fatalf("release of another system's claim = %d, %v; want %q", code, err, take)
	}
	// A pid without a readable start token is unverified, not another system's.
	if label := TaskOwnerLabel(TaskOwnerRecord{Actor: "codex", ActorPID: 4242}); !strings.Contains(label, "(PID 4242)") {
		t.Fatalf("label of a claim with no start token = %q, want no system named", label)
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

// A claim takes ready work only. A blocked task returns to todo through unblock, the one path that
// records the human's answer and spends any audit authority; a done task stays in the archive. Each
// refusal leaves the folder where it is, writes no claim, and names the command that moves it on.
func TestClaimRefusesBlockedAndDoneTasks(t *testing.T) {
	const id = "2026-11-03-pick-storage"
	for name, tc := range map[string]struct {
		state, decision string
		want, notWant   string
	}{
		"blocked with an open decision": {StateBlocked, renderDecisionScaffold(id, "Pick storage"), `coop tasks unblock ` + id + ` "<answer>"`, ""},
		"blocked with a pending note":   {StateBlocked, "# Decision: Which store?\n\n**Resolution:** pending owner direction\n", `coop tasks unblock ` + id + ` "<answer>"`, ""},
		"blocked with an answer":        {StateBlocked, "# Decision: Which store?\n\n**Resolution:** Postgres.\n", "coop tasks unblock " + id, "<answer>"},
		"blocked without a decision":    {StateBlocked, "", "coop tasks unblock " + id, "<answer>"},
		"done":                          {StateDone, "", "Completed work stays in the archive", "unblock"},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, tc.state, id)
			writeTaskFile(t, filepath.Join(dir, "task.md"), "# Pick storage\n")
			if tc.decision != "" {
				writeTaskFile(t, filepath.Join(dir, "decision.md"), tc.decision)
			}
			code, err := CmdTasksFolder(root, root, []string{"claim", "pick-storage"})
			if code != 1 || err == nil {
				t.Fatalf("claim = (%d, %v), want a refusal", code, err)
			}
			if !strings.Contains(err.Error(), tc.want) || (tc.notWant != "" && strings.Contains(err.Error(), tc.notWant)) {
				t.Fatalf("refusal = %q, want %q without %q", err, tc.want, tc.notWant)
			}
			if items := mustReadTaskTree(t, root); len(items) != 1 || items[0].State != tc.state {
				t.Fatalf("the refused claim moved the task: %+v", items)
			}
			if _, owned := taskOwned(t, root, id); owned {
				t.Fatal("a refused claim wrote an owner record")
			}
		})
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
