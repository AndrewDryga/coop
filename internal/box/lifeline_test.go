package box

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/runtime"
	"golang.org/x/sys/unix"
)

// lifelineOwnerEnv makes this test binary a coop that starts one box's lifeline for the workspace
// it names, prints the box's supervisor label, and waits to be killed.
const lifelineOwnerEnv = "COOP_TEST_LIFELINE_OWNER"

// runLifelineTestProcess runs this test binary as a lifeline or as its owner when it was started as
// one, before any test runs; ok is false for an ordinary test run.
func runLifelineTestProcess() (code int, ok bool) {
	if len(os.Args) > 1 && os.Args[1] == LifelineCommand {
		return RunLifeline(os.Args[2:]), true
	}
	workspace := os.Getenv(lifelineOwnerEnv)
	if workspace == "" {
		return 0, false
	}
	lifelineExecutable = os.Executable
	if startLifeline(runtime.Runtime{Name: os.Getenv("COOP_TEST_LIFELINE_RUNTIME")}, RunSpec{Repo: workspace}) == nil {
		return 1, true
	}
	// fd 4 is a pipe end the test handed this coop without close-on-exec, as an editor might: once
	// this coop lets go of it, only a lifeline that leaked it still holds it open.
	_ = unix.Close(4)
	fmt.Println(supervisorLabelValue(workspaceScope(workspace), os.Getpid()))
	time.Sleep(time.Minute)
	return 1, true
}

// quickWaits keep the in-process tests short; the spawned lifeline uses the real ones.
var quickWaits = lifelineWaits{owner: 300 * time.Millisecond, late: 300 * time.Millisecond, total: 10 * time.Second}

func remainingFakeBoxes(t *testing.T, boxes string) []string {
	t.Helper()
	entries, err := os.ReadDir(boxes)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	slices.Sort(names)
	return names
}

// A coop that died without its quit takes exactly its own run's containers: its boxes, broker and
// services by their exact labels. Another dead run's box in the same workspace, a live run's and
// a service whose label names someone else stay for the sweep.
func TestLifelineRemovesTheDeadOwnersRunOnly(t *testing.T) {
	rt, boxes := fakeRuntime(t)
	workspace := t.TempDir()
	dead, otherDead := goneSupervisorLabel(t, workspace), goneSupervisorLabel(t, workspace)
	addFakeBox(t, boxes, "own-box", map[string]string{LabelHost: dead})
	addFakeContainer(t, boxes, "own-broker", LabelBroker, map[string]string{LabelHost: dead})
	addFakeService(t, boxes, "own-service", ComposeProjectFor(workspace, "run-a"), dead)
	addFakeBox(t, boxes, "other-dead-box", map[string]string{LabelHost: otherDead})
	addFakeService(t, boxes, "other-dead-service", ComposeProjectFor(workspace, "run-b"), otherDead)
	addFakeBox(t, boxes, "live-box", map[string]string{LabelHost: liveSupervisorLabel(t, workspace)})

	owner := lifelineOwner{box: dead, services: dead, workspace: workspace}
	if n := watchLifeline(strings.NewReader(""), rt, owner, quickWaits); n != 1 {
		t.Fatalf("lifeline removed %d boxes, want 1:\n%s", n, fakeRuntimeEvents(t))
	}
	if got, want := remainingFakeBoxes(t, boxes), []string{"live-box", "other-dead-box", "other-dead-service"}; !slices.Equal(got, want) {
		t.Fatalf("left %v, want %v:\n%s", got, want, fakeRuntimeEvents(t))
	}
}

// An editor session's child labels its services with the session's supervisor. When only the child
// dies, its lifeline removes the child's box and leaves the services the live session still uses.
func TestLifelineKeepsServicesALiveSupervisorOwns(t *testing.T) {
	rt, boxes := fakeRuntime(t)
	workspace := t.TempDir()
	child, session := goneSupervisorLabel(t, workspace), liveSupervisorLabel(t, workspace)
	addFakeBox(t, boxes, "child-box", map[string]string{LabelHost: child})
	addFakeService(t, boxes, "session-service", ComposeProjectFor(workspace, "session"), session)
	if n := watchLifeline(strings.NewReader(""), rt, lifelineOwner{box: child, services: session, workspace: workspace}, quickWaits); n != 1 {
		t.Fatalf("lifeline removed %d boxes, want the child's one:\n%s", n, fakeRuntimeEvents(t))
	}
	if got := remainingFakeBoxes(t, boxes); !slices.Equal(got, []string{"session-service"}) {
		t.Fatalf("left %v, want the live session's service", got)
	}
}

// A pipe that ends while its owner lives (or cannot be proved gone) removes nothing, and the
// lifeline still exits within its bound.
func TestLifelineKeepsALiveOwnersBox(t *testing.T) {
	rt, boxes := fakeRuntime(t)
	workspace := t.TempDir()
	live := liveSupervisorLabel(t, workspace)
	addFakeBox(t, boxes, "live-box", map[string]string{LabelHost: live})
	start := time.Now()
	if n := watchLifeline(strings.NewReader(""), rt, lifelineOwner{box: live, services: live, workspace: workspace}, quickWaits); n != 0 {
		t.Fatalf("lifeline removed %d boxes of a live owner", n)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("lifeline waited %s for a live owner, want about %s", elapsed, quickWaits.owner)
	}
	if events := fakeRuntimeEvents(t); events != "" {
		t.Fatalf("lifeline asked the runtime with a live owner:\n%s", events)
	}
}

// After a clean launch coop says quit: nothing is removed, the runtime is never asked, even though
// the owner reads as dead.
func TestLifelineDoesNothingAfterQuit(t *testing.T) {
	rt, boxes := fakeRuntime(t)
	workspace := t.TempDir()
	dead := goneSupervisorLabel(t, workspace)
	addFakeBox(t, boxes, "box", map[string]string{LabelHost: dead})
	if n := watchLifeline(strings.NewReader("quit\n"), rt, lifelineOwner{box: dead, workspace: workspace}, quickWaits); n != 0 {
		t.Fatalf("lifeline removed %d boxes after quit", n)
	}
	if events := fakeRuntimeEvents(t); events != "" {
		t.Fatalf("lifeline asked the runtime after quit:\n%s", events)
	}
}

// A dead owner whose box never appears costs two listings and the lifeline exits; a box a runtime
// client creates after the first listing is taken by the second.
func TestLifelineExitsWhenTheBoxNeverAppearsAndTakesALateOne(t *testing.T) {
	rt, _ := fakeRuntime(t)
	workspace := t.TempDir()
	dead := goneSupervisorLabel(t, workspace)
	owner := lifelineOwner{box: dead, workspace: workspace}
	start := time.Now()
	if n := watchLifeline(strings.NewReader(""), rt, owner, quickWaits); n != 0 {
		t.Fatalf("lifeline removed %d boxes, want none", n)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("lifeline took %s with no box to remove", elapsed)
	}
	boxListing := "ps -q -a --filter label=coop=box --filter label=" + LabelHost + "=" + dead + "\n"
	if got := strings.Count(fakeRuntimeEvents(t), boxListing); got != 2 {
		t.Fatalf("box listings = %d, want 2:\n%s", got, fakeRuntimeEvents(t))
	}

	rt, boxes := fakeRuntime(t)
	done := make(chan int, 1)
	go func() {
		done <- watchLifeline(strings.NewReader(""), rt, owner, lifelineWaits{owner: time.Second, late: 2 * time.Second, total: 10 * time.Second})
	}()
	brokerListing := "ps -q -a --filter label=coop=broker --filter label=" + LabelHost + "=" + dead + "\n"
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(fakeRuntimeEvents(t), brokerListing); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the first pass never ran:\n%s", fakeRuntimeEvents(t))
		}
	}
	addFakeBox(t, boxes, "late-box", map[string]string{LabelHost: dead})
	if n := <-done; n != 1 || len(remainingFakeBoxes(t, boxes)) != 0 {
		t.Fatalf("lifeline removed %d boxes, left %v; want the late box removed", n, remainingFakeBoxes(t, boxes))
	}
}

// The real chain: a coop process starts its lifeline, which runs in its own session with only the
// pipe besides /dev/null; a SIGKILL of that coop removes its box within seconds, and a clean stop
// removes nothing.
func TestLifelineOfAKilledCoopRemovesItsBox(t *testing.T) {
	rt, boxes := fakeRuntime(t)
	workspace := t.TempDir()
	t.Setenv("COOP_TEST_LIFELINE_RUNTIME", rt.Name)
	t.Setenv(lifelineOwnerEnv, workspace)
	owner := exec.Command(os.Args[0], "-test.run=^$")
	stdout, err := owner.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	inheritedRead, inheritedWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer inheritedRead.Close()
	owner.ExtraFiles = []*os.File{inheritedRead, inheritedWrite} // fd 4 in the owner: the write end
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	_ = inheritedWrite.Close()
	t.Cleanup(func() { _ = owner.Process.Kill(); _ = owner.Wait() })
	label, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(label) == "" {
		t.Fatalf("owner printed %q, %v", label, err)
	}
	label = strings.TrimSpace(label)
	addFakeBox(t, boxes, "killed-run", map[string]string{LabelHost: label})
	addFakeBox(t, boxes, "someone-else", map[string]string{LabelHost: liveSupervisorLabel(t, workspace)})

	helper := lifelineProcess(t, owner.Process.Pid)
	if helper.sid != helper.pid || helper.sid == owner.Process.Pid {
		t.Errorf("lifeline %+v is not in a session of its own", helper)
	}
	// The owner closed its copy before printing, so the write end is open only if the lifeline kept it.
	_ = inheritedRead.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := inheritedRead.Read(make([]byte, 1)); err != io.EOF {
		t.Errorf("reading a file the owner inherited = %v, want EOF: the lifeline %d holds it", err, helper.pid)
	}
	if err := syscall.Kill(owner.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = owner.Wait()
	go func() { _, _ = io.Copy(io.Discard, stdout) }()
	for deadline := time.Now().Add(10 * time.Second); slices.Contains(remainingFakeBoxes(t, boxes), "killed-run"); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the killed coop's box is still there:\n%s", fakeRuntimeEvents(t))
		}
	}
	if got := remainingFakeBoxes(t, boxes); !slices.Equal(got, []string{"someone-else"}) {
		t.Fatalf("left %v, want only the live owner's box", got)
	}
	for deadline := time.Now().Add(10 * time.Second); syscall.Kill(helper.pid, 0) == nil; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("lifeline %d outlived its work", helper.pid)
		}
	}
}

// A launch that ends cleanly stops its lifeline, which exits without asking the runtime anything.
func TestLifelineStopsCleanly(t *testing.T) {
	rt, _ := fakeRuntime(t)
	saved := lifelineExecutable
	lifelineExecutable = os.Executable
	t.Cleanup(func() { lifelineExecutable = saved })
	life := startLifeline(rt, RunSpec{Repo: t.TempDir()})
	if life == nil {
		t.Skip("this host cannot label a box, so no lifeline starts")
	}
	helper := lifelineProcess(t, os.Getpid())
	life.stop()
	for deadline := time.Now().Add(5 * time.Second); syscall.Kill(helper.pid, 0) == nil; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("lifeline %d did not quit", helper.pid)
		}
	}
	if events := fakeRuntimeEvents(t); events != "" {
		t.Fatalf("a stopped lifeline asked the runtime:\n%s", events)
	}
}

type lifelineProc struct{ pid, sid int }

// lifelineProcess finds the lifeline parent started, and its session.
func lifelineProcess(t *testing.T, parent int) lifelineProc {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=,command=").Output()
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(out), "\n") {
			var pid, ppid int
			if _, err := fmt.Sscan(line, &pid, &ppid); err != nil || ppid != parent || !strings.Contains(line, LifelineCommand) {
				continue
			}
			sid, err := unix.Getsid(pid)
			if err != nil {
				t.Fatal(err)
			}
			return lifelineProc{pid: pid, sid: sid}
		}
	}
	t.Fatalf("no lifeline under pid %d", parent)
	return lifelineProc{}
}
