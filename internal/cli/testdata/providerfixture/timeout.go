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

	"github.com/AndrewDryga/coop/internal/testutil/procharness"
)

func waitForConsultSignal() (int, string, error) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	got := <-signals
	sig, _ := got.(syscall.Signal)
	return 128 + int(sig), got.String(), nil
}

func serveTimeout(root, trace, scenarioPath string, args []string) error {
	if len(args) < 5 || args[0] != "-k" || args[1] != "30" {
		return errors.New("timeout requires exact -k 30 SECONDS PROVIDER ARGV")
	}
	seconds, err := strconv.Atoi(args[2])
	if err != nil || seconds < 1 || seconds > 3600 {
		return fmt.Errorf("timeout seconds %q are outside 1..3600", args[2])
	}
	provider, err := providerToken(args[3])
	if err != nil || provider == "timeout" {
		return errors.New("timeout provider is invalid")
	}
	executable, err := exec.LookPath(provider)
	if err != nil {
		return err
	}
	if provider == "tar" {
		if len(args) != 9 || args[4] != "--exclude=./.git" || args[5] != "--exclude=./.git/*" || args[6] != "-cf" || args[8] != "." {
			return errors.New("timeout tar requires the exact delegate snapshot argv")
		}
		destinationDir, err := procharness.CanonicalUnderRoot(root, filepath.Dir(args[7]))
		if err != nil || !pathAtOrBelow(filepath.Join(root, "tmp"), destinationDir) || !strings.HasPrefix(filepath.Base(args[7]), "tree-") || filepath.Ext(args[7]) != ".tar" {
			return errors.New("timeout tar destination is outside fixture attempt state")
		}
	} else {
		executable, err = procharness.CanonicalUnderRoot(root, executable)
		if err != nil || filepath.Dir(executable) != filepath.Join(root, "bin") {
			return errors.New("timeout provider alias is outside the fixture bin directory")
		}
	}
	cmd := exec.Command(executable, args[4:]...)
	cmd.Env = os.Environ()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := record(root, trace, traceRecord{Source: "timeout", Event: "start", PID: os.Getpid(), ParentPID: os.Getppid(), Argv: traceProviderArgv(args)}); err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(time.Duration(seconds) * time.Second)
	defer timer.Stop()
	select {
	case err := <-done:
		exitCode, sig := commandExit(err)
		_ = record(root, trace, traceRecord{Source: "timeout", Event: "exit", PID: os.Getpid(), ExitCode: &exitCode, Signal: sig})
		if err != nil {
			return &fixtureExitError{Code: exitCode, Err: err}
		}
		return nil
	case <-timer.C:
		_ = cmd.Process.Signal(syscall.SIGTERM)
		var childErr error
		select {
		case childErr = <-done:
		case <-time.After(250 * time.Millisecond):
			_ = cmd.Process.Kill()
			childErr = <-done
		}
		_, sig := commandExit(childErr)
		exitCode := 124
		_ = record(root, trace, traceRecord{Source: "timeout", Event: "exit", PID: os.Getpid(), ExitCode: &exitCode, Signal: sig})
		os.Exit(exitCode)
		return nil
	}
}

func serveFlock(root, trace string, args []string) error {
	if len(args) != 3 || args[0] != "-w" {
		return errors.New("flock requires exact -w SECONDS FD")
	}
	seconds, err := strconv.Atoi(args[1])
	if err != nil || seconds < 1 || seconds > 400000 {
		return errors.New("flock wait is outside 1..400000 seconds")
	}
	fd, err := strconv.Atoi(args[2])
	if err != nil || fd < 3 || fd > 64 {
		return errors.New("flock descriptor is outside 3..64")
	}
	if err := record(root, trace, traceRecord{Source: "flock", Event: "start", PID: os.Getpid(), ParentPID: os.Getppid(), Argv: append([]string(nil), args...)}); err != nil {
		return err
	}
	if os.Getenv("COOP_PROVIDER_FIXTURE_DELEGATE_CALL") == "1" {
		if err := os.WriteFile(filepath.Join(root, "state", delegateContenderFileName), []byte("waiting\n"), 0o600); err != nil {
			return err
		}
	}
	deadline := time.Now().Add(time.Duration(seconds) * time.Second)
	for {
		err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			if err := record(root, trace, traceRecord{Source: "flock", Event: "acquired", PID: os.Getpid()}); err != nil {
				return err
			}
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return err
		}
		if !time.Now().Before(deadline) {
			return errors.New("flock wait expired")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func serveSetsid(root, trace string, args []string) error {
	if len(args) < 2 || args[0] != "timeout" {
		return errors.New("setsid requires the fixture timeout alias and its argv")
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if _, err := procharness.CanonicalUnderRoot(root, self); err != nil {
		return err
	}
	if err := record(root, trace, traceRecord{Source: "setsid", Event: "exec", PID: os.Getpid(), ParentPID: os.Getppid(), Argv: traceProviderArgv(args)}); err != nil {
		return err
	}
	if group, err := syscall.Getpgid(0); err != nil || group == os.Getpid() {
		return errors.New("fixture setsid alias must be launched outside its own process group")
	}
	if _, err := syscall.Setsid(); err != nil {
		return err
	}
	return syscall.Exec(self, append([]string{"timeout"}, args[1:]...), os.Environ())
}
