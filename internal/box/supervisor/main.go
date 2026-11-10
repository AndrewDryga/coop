// coop-supervise runs only inside a Coop container, below the runtime's --init.
// The bootstrap execs it after publishing config and starting trusted forwarders.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const drainedExit, timeoutExit = 190, 191

type identity struct {
	pid   int
	start string
}

type process struct {
	identity
	parent, session int
	state, name     string
}

type procFS struct {
	root       string
	self       int
	forwarders map[int]string
}

func main() { os.Exit(run(os.Args[1:], os.Stderr)) }

func run(args []string, stderr io.Writer) int {
	if runtime.GOOS != "linux" || len(args) < 2 {
		fmt.Fprintln(stderr, "coop-supervise requires a Linux container, forwarder identities and a command")
		return 1
	}
	forwarders := make(map[int]string)
	for _, record := range strings.Fields(args[0]) {
		pid, start, ok := strings.Cut(record, ":")
		n, err := strconv.Atoi(pid)
		if !ok || err != nil || n < 1 || start == "" || strings.Trim(start, "0123456789") != "" {
			fmt.Fprintln(stderr, "coop-supervise: invalid forwarder identity")
			return 1
		}
		forwarders[n] = start
	}
	fs := procFS{root: "/proc", self: os.Getpid(), forwarders: forwarders}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	cmd := exec.Command(args[1], args[2:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(stderr, "coop-supervise:", err)
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			return 127
		}
		return 126
	}
	finished := make(chan error, 1)
	go func() { finished <- cmd.Wait() }()
	select {
	case err := <-finished:
		if code := exitCode(err); code != 0 {
			fs.terminate()
			return code
		}
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-finished:
		case <-time.After(2 * time.Second):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-finished
		}
		fs.terminate()
		return 143
	}
	code, err := fs.drain(ctx, cmd.Process.Pid, descendantTimeout(os.Getenv("COOP_DESCENDANT_TIMEOUT")), stderr)
	if err != nil {
		fmt.Fprintln(stderr, "coop-supervise:", err)
		code = 1
	}
	if ctx.Err() != nil {
		code = 143
	}
	if code == 1 || code == 143 || code == timeoutExit {
		fs.terminate()
	}
	return code
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var failure *exec.ExitError
	if errors.As(err, &failure) {
		status, ok := failure.Sys().(syscall.WaitStatus)
		if ok && status.Signaled() {
			return 128 + int(status.Signal())
		}
		code := failure.ExitCode()
		if code > 0 && code != drainedExit && code != timeoutExit {
			return code
		}
	}
	return 1
}

func descendantTimeout(value string) time.Duration {
	seconds, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		seconds = 900
	}
	return time.Duration(seconds) * time.Second
}

func parseStat(data string) (process, error) {
	open, close := strings.Index(data, "("), strings.LastIndex(data, ") ")
	if open < 1 || close < open {
		return process{}, errors.New("invalid process stat")
	}
	fields := strings.Fields(data[close+2:])
	if len(fields) < 20 {
		return process{}, errors.New("short process stat")
	}
	pid, e1 := strconv.Atoi(strings.TrimSpace(data[:open]))
	parent, e2 := strconv.Atoi(fields[1])
	session, e3 := strconv.Atoi(fields[3])
	if e1 != nil || e2 != nil || e3 != nil {
		return process{}, errors.New("invalid process identity")
	}
	return process{identity{pid, fields[19]}, parent, session, fields[0], data[open+1 : close]}, nil
}

func (fs procFS) path(pid int, name string) string {
	return filepath.Join(fs.root, strconv.Itoa(pid), name)
}

func (fs procFS) read(pid int) (process, error) {
	data, err := os.ReadFile(fs.path(pid, "stat"))
	if err != nil {
		return process{}, err
	}
	return parseStat(string(data))
}

// Scan the entire namespace: descendants can detach twice and reparent to init.
// A forwarder exemption survives only while the recorded leader still exists.
func (fs procFS) jobs() ([]process, error) {
	entries, err := os.ReadDir(fs.root)
	if err != nil {
		return nil, err
	}
	exempt := make(map[int]bool)
	for pid, start := range fs.forwarders {
		if p, err := fs.read(pid); err == nil && p.start == start {
			exempt[pid] = true
		}
	}
	var jobs []process
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		p, err := fs.read(pid)
		// procfs also returns ESRCH when the task exits between open and read.
		if os.IsNotExist(err) || errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if p.state != "Z" && pid != 1 && pid != fs.self && p.parent != fs.self && !exempt[p.session] {
			jobs = append(jobs, p)
		}
	}
	return jobs, nil
}

func (fs procFS) owned(p process) bool {
	data, err := os.ReadFile(fs.path(p.pid, "environ"))
	if err != nil {
		return false
	}
	for _, value := range strings.Split(string(data), "\x00") {
		if value == "COOP_CONSULT_OWNED=1" {
			return true
		}
	}
	return false
}

func (fs procFS) daemon(p process, provider int) bool {
	switch p.name {
	case "epmd", "ssh-agent", "gpg-agent", "dbus-daemon":
		return true
	}
	if p.session == provider {
		return false
	}
	for fd := 0; fd < 3; fd++ {
		target, err := os.Readlink(fs.path(p.pid, "fd/"+strconv.Itoa(fd)))
		if err != nil || target != "/dev/null" {
			return false
		}
	}
	return true
}

func names(jobs []process) string {
	seen := make(map[string]bool)
	var names []string
	for _, p := range jobs {
		name := strings.Map(func(r rune) rune {
			if r < 32 || r == 127 {
				return ' '
			}
			return r
		}, p.name)
		if name != "" && !seen[name] {
			names = append(names, name)
			seen[name] = true
		}
	}
	if len(names) == 0 {
		return "unknown"
	}
	return strings.Join(names, " ")
}

func (fs procFS) signal(jobs []process, sig syscall.Signal) {
	for _, p := range jobs {
		// A PID may have been reused during the grace period.
		if current, err := fs.read(p.pid); err == nil && current.identity == p.identity {
			_ = syscall.Kill(p.pid, sig)
		}
	}
}

func (fs procFS) reap(jobs []process) {
	fs.signal(jobs, syscall.SIGTERM)
	time.Sleep(time.Second)
	fs.signal(jobs, syscall.SIGKILL)
}

func (fs procFS) terminate() {
	jobs, _ := fs.jobs()
	if len(jobs) == 0 {
		return
	}
	fs.signal(jobs, syscall.SIGTERM)
	time.Sleep(2 * time.Second)
	jobs, _ = fs.jobs()
	fs.signal(jobs, syscall.SIGKILL)
}

func pause(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (fs procFS) drain(ctx context.Context, provider int, timeout time.Duration, stderr io.Writer) (int, error) {
	deadline := time.Now().Add(timeout)
	var daemonDeadline time.Time
	sawWork, rescanned, reapedConsult := false, false, false
	reaped := make(map[identity]bool)
	for ctx.Err() == nil {
		all, err := fs.jobs()
		if err != nil {
			return 1, err
		}
		var owned, jobs, daemons []process
		for _, p := range all {
			if reaped[p.identity] {
				continue
			}
			switch {
			case fs.owned(p):
				owned = append(owned, p)
			case fs.daemon(p, provider):
				daemons = append(daemons, p)
			default:
				jobs = append(jobs, p)
			}
		}
		if len(owned) > 0 {
			if !reapedConsult {
				fmt.Fprintln(stderr, "Consult(s) outlived the provider that asked — their reply can no longer be read; reaping:", names(owned))
			}
			reapedConsult = true
			fs.reap(owned)
			for _, p := range owned {
				reaped[p.identity] = true
			}
		}
		if len(jobs) == 0 && len(daemons) > 0 {
			if daemonDeadline.IsZero() {
				daemonDeadline = time.Now().Add(10 * time.Second)
			}
			if time.Now().Before(daemonDeadline) {
				pause(ctx, time.Second)
				continue
			}
			fmt.Fprintln(stderr, "The provider left a daemon running, which never exits by itself — stopping:", names(daemons))
			fs.reap(daemons)
			for _, p := range daemons {
				reaped[p.identity] = true
			}
			continue
		}
		if len(jobs) > 0 {
			if !sawWork {
				fmt.Fprintf(stderr, "The provider exited with background work still live — waiting up to %.0fs for: %s\n", timeout.Seconds(), names(jobs))
			}
			sawWork = true
			if !time.Now().Before(deadline) {
				fmt.Fprintf(stderr, "Background work did not exit within %.0fs — terminating: %s\n", timeout.Seconds(), names(jobs))
				return timeoutExit, nil
			}
			pause(ctx, time.Second)
			continue
		}
		if sawWork {
			return drainedExit, nil
		}
		if rescanned || !time.Now().Before(deadline) {
			return 0, nil
		}
		// A double-setsid job may be between its launcher and reparented leader.
		rescanned = true
		pause(ctx, 100*time.Millisecond)
	}
	return 143, nil
}
