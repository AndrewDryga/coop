package box

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/runtime"
	"golang.org/x/sys/unix"
)

// LifelineCommand is the hidden coop subcommand a box's lifeline runs as (RunLifeline).
const LifelineCommand = "__box-lifeline"

// lifelineExecutable is the binary a lifeline runs. Only the coop binary turns it on (EnableLifeline):
// a test binary started with LifelineCommand would run its whole suite instead.
var lifelineExecutable = func() (string, error) { return "", errors.New("box lifelines are off in this binary") }

// EnableLifeline lets every box this process starts have a lifeline. coop's main calls it.
func EnableLifeline() { lifelineExecutable = os.Executable }

// lifelineWaits bounds what a lifeline does once its coop is gone, so it never outlives the run by
// more than seconds.
type lifelineWaits struct {
	owner time.Duration // for the kernel to show the owner gone: an unreaped coop still looks alive
	late  time.Duration // before the second removal, which takes a box a runtime client still created
	total time.Duration // for all of the removal
}

var defaultLifelineWaits = lifelineWaits{owner: 5 * time.Second, late: 2 * time.Second, total: 30 * time.Second}

// lifeline is coop's end of a box's lifeline: the write end of a pipe only this process holds.
//
// Nothing else stops a box when coop dies without running its cleanup (SIGKILL, an OOM kill, a
// crash): Docker keeps a container running when its attach client goes, and the box has no way to
// tell. So before a box starts, coop starts a small copy of itself in its own session, holding the
// read end. When the read ends without "quit", coop is gone; the copy proves that with the box's own
// supervisor label, exactly as the next launch's sweep would, and removes that run's containers.
type lifeline struct {
	mu sync.Mutex
	w  *os.File
}

// startLifeline starts the lifeline of one launch, or returns nil when it cannot: a box whose
// supervisor no later process could prove dead gets none, and a lifeline that does not start leaves
// the box to the next launch's sweep, as before.
func startLifeline(rt runtime.Runtime, spec RunSpec) *lifeline {
	label := supervisorLabelValue(supervisorScope(spec), os.Getpid())
	if label == "" || rt.Name == "" {
		return nil
	}
	self, err := lifelineExecutable()
	if err != nil {
		return nil
	}
	r, w, err := os.Pipe() // close-on-exec: no runtime client or provider coop starts holds an end
	if err != nil {
		return nil
	}
	cmd := exec.Command(self, LifelineCommand, rt.Name, label, serviceSupervisor(spec), spec.Repo)
	// Its own session, so a Ctrl-C or a closed terminal never reaches it, and no folder of the run
	// held open. Standard streams are /dev/null; the pipe is its only other file.
	cmd.Dir = "/"
	cmd.ExtraFiles = []*os.File{r}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = cmd.Start()
	_ = r.Close()
	if err != nil {
		_ = w.Close()
		return nil
	}
	go func() { _ = cmd.Wait() }() // reaped whenever it ends; coop never waits on it
	return &lifeline{w: w}
}

// network tells the lifeline the filtered run this launch recorded, so it settles that run too.
func (l *lifeline) network(runID string) {
	if l == nil || runID == "" || strings.ContainsAny(runID, " \r\n") {
		return
	}
	l.send("network " + runID)
}

// stop ends the lifeline: the launch is over and its own cleanup ran.
func (l *lifeline) stop() {
	if l == nil {
		return
	}
	l.send("quit")
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = l.w.Close()
}

func (l *lifeline) send(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = io.WriteString(l.w, line+"\n")
}

// RunLifeline is the lifeline itself: rt, the box's supervisor label, its services' supervisor label
// and the workspace, with the pipe on fd 3. It always exits 0: nobody reads its status.
func RunLifeline(args []string) int {
	if len(args) != 4 {
		return 2
	}
	pipe := os.NewFile(3, "lifeline")
	if pipe == nil {
		return 2
	}
	closeInheritedFiles()
	syscall.CloseOnExec(3) // the runtime commands it runs must not hold the pipe either
	watchLifeline(pipe, runtime.Runtime{Name: args[0]}, lifelineOwner{box: args[1], services: args[2], workspace: args[3]}, defaultLifelineWaits)
	return 0
}

// closeInheritedFiles closes what coop's own parent handed it without close-on-exec (an editor's
// extra pipes, say): os/exec passes those on too, and a lifeline must hold nothing but its pipe. Every
// file Go opens is close-on-exec, so a file past the pipe without the flag came through exec.
func closeInheritedFiles() {
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		return
	}
	for _, entry := range entries {
		fd, err := strconv.Atoi(entry.Name())
		if err != nil || fd <= 3 {
			continue
		}
		if flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err == nil && flags&unix.FD_CLOEXEC == 0 {
			_ = unix.Close(fd)
		}
	}
}

// lifelineOwner is what one lifeline may remove: containers carrying these exact supervisor labels.
type lifelineOwner struct{ box, services, workspace string }

// watchLifeline reads the pipe until coop says quit or the pipe ends. An end without quit means the
// coop holding the write end is gone, but only the kernel's word that the box's supervisor is gone
// (supervisorProvablyDead, the sweep's own test) lets it remove anything. It reports how many boxes
// it removed.
func watchLifeline(pipe io.Reader, rt runtime.Runtime, owner lifelineOwner, waits lifelineWaits) int {
	ref, ok := parseSupervisorLabel(owner.box)
	if !ok {
		return 0
	}
	networkRun := ""
	lines := bufio.NewScanner(pipe)
	for lines.Scan() {
		line := lines.Text()
		if line == "quit" {
			return 0
		}
		if id, ok := strings.CutPrefix(line, "network "); ok {
			networkRun = id
		}
	}
	host, _ := thisHost()
	for deadline := time.Now().Add(waits.owner); !supervisorProvablyDead(ref, host); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			return 0 // the owner lives on, or cannot be proved gone: the next sweep judges again
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), waits.total)
	defer cancel()
	return reapLifeline(ctx, rt, owner, networkRun, waits.late)
}

// reapLifeline removes what a dead coop's launch left, each step the same exact-label removal the
// sweep runs: its boxes and broker helpers, then its run's services, then its filtered run.
func reapLifeline(ctx context.Context, rt runtime.Runtime, owner lifelineOwner, networkRun string, late time.Duration) int {
	orphans := []OrphanBox{{Evidence: owner.box}}
	removed := 0
	pass := func() {
		n, _ := removeBySupervisor(ctx, rt, LabelBox, orphans)
		removed += n
		_, _ = removeBySupervisor(ctx, rt, LabelBroker, orphans)
	}
	pass()
	// The runtime client coop started outlives it, and may finish creating the box after the first
	// pass; the second takes that one.
	select {
	case <-time.After(late):
		pass()
	case <-ctx.Done():
	}
	if owner.services != "" {
		// Judged on the services' own label: an editor session's services name the session, which may live.
		host, _ := thisHost()
		_, _ = reapOrphanServices(ctx, rt, owner.workspace, host, owner.services)
	}
	if networkRun != "" {
		_, _ = RecoverNetworkRuns(ctx, rt, networkRun) // proves the run's supervisor gone itself
	}
	return removed
}
