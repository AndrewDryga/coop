package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const successExitCode = 23

func main() {
	if len(os.Args) < 2 {
		fail("missing mode")
	}
	switch os.Args[1] {
	case "orphan":
		if len(os.Args) != 3 {
			fail("orphan requires a pidfile")
		}
		runOrphanProbe(os.Args[2])
	case "producer":
		if len(os.Args) != 3 {
			fail("producer requires a pidfile")
		}
		startOrphan(os.Args[2])
	case "signal":
		if len(os.Args) != 4 {
			fail("signal requires ready and received files")
		}
		awaitSignal(os.Args[2], os.Args[3])
	default:
		fail("unknown mode %q", os.Args[1])
	}
}

func runOrphanProbe(pidfile string) {
	producer := exec.Command(os.Args[0], "producer", pidfile)
	producer.Stdout, producer.Stderr = os.Stdout, os.Stderr
	if err := producer.Run(); err != nil {
		fail("producer: %v", err)
	}
	data, err := os.ReadFile(pidfile)
	if err != nil {
		fail("read orphan pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		fail("invalid orphan pid %q", data)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		fail("kill orphan %d: %v", pid, err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			os.Exit(successExitCode)
		}
		time.Sleep(10 * time.Millisecond)
	}
	status, _ := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	fail("orphan %d remained visible after SIGKILL:\n%s", pid, status)
}

func startOrphan(pidfile string) {
	child := exec.Command("/bin/sleep", "300")
	child.Stdin, child.Stdout, child.Stderr = nil, nil, nil
	if err := child.Start(); err != nil {
		fail("start orphan: %v", err)
	}
	if err := os.WriteFile(pidfile, []byte(strconv.Itoa(child.Process.Pid)+"\n"), 0o600); err != nil {
		_ = child.Process.Kill()
		fail("write orphan pid: %v", err)
	}
	// Intentionally return without Wait. The child is adopted by the container's PID 1;
	// the orphan probe kills it only after this producer has exited.
}

func awaitSignal(ready, received string) {
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM)
	defer signal.Stop(term)
	if err := os.WriteFile(ready, []byte("ready\n"), 0o666); err != nil {
		fail("write ready marker: %v", err)
	}
	<-term
	if err := os.WriteFile(received, []byte("term\n"), 0o666); err != nil {
		fail("write signal marker: %v", err)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "initprobe: "+format+"\n", args...)
	os.Exit(1)
}
