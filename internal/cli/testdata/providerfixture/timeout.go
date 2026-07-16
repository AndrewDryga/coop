package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
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
	executable, err = procharness.CanonicalUnderRoot(root, executable)
	if err != nil || filepath.Dir(executable) != filepath.Join(root, "bin") {
		return errors.New("timeout provider alias is outside the fixture bin directory")
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
