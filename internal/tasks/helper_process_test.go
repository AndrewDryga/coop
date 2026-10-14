package tasks

import (
	"bufio"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/procharness"
	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

// The lease/ref helpers are real independent processes. Register their full ownership before
// any readiness assertion; one Wait receipt serves explicit release/crash and abort cleanup.
type taskHelperProcess struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  *os.File
	out     *bufio.Reader
	done    chan struct{}
	waitErr error
	readers sync.WaitGroup
}

func startTaskHelperProcess(t *testing.T, cmd *exec.Cmd) *taskHelperProcess {
	t.Helper()
	stdin, stdout, err := procharness.StartStdio(cmd)
	if err != nil {
		t.Fatal(err)
	}
	p := &taskHelperProcess{cmd: cmd, stdin: stdin, stdout: stdout, out: bufio.NewReader(stdout), done: make(chan struct{})}
	t.Cleanup(func() {
		_ = p.stdin.Close()
		select {
		case <-p.done:
		default:
			_ = p.cmd.Process.Kill()
		}
		select {
		case <-p.done:
		case <-time.After(wait.Deadline):
			t.Error("task helper did not reap during cleanup")
		}
		_ = p.stdout.Close()
		drained := make(chan struct{})
		go func() { p.readers.Wait(); close(drained) }()
		select {
		case <-drained:
		case <-time.After(wait.Deadline):
			t.Error("task helper reader did not drain during cleanup")
		}
	})
	go func() { p.waitErr = cmd.Wait(); close(p.done) }()
	return p
}

func (p *taskHelperProcess) wait(t *testing.T) error {
	t.Helper()
	select {
	case <-p.done:
		return p.waitErr
	case <-time.After(wait.Deadline):
		t.Fatal("task helper did not exit")
		return nil
	}
}

func (p *taskHelperProcess) line(t *testing.T, what string) string {
	t.Helper()
	line := make(chan string, 1)
	p.readers.Add(1)
	go func() {
		defer p.readers.Done()
		s, _ := p.out.ReadString('\n')
		line <- strings.TrimSpace(s)
	}()
	select {
	case got := <-line:
		return got
	case <-time.After(5 * time.Second):
		t.Fatalf("%s helper did not report a result", what)
		return ""
	}
}
