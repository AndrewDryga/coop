//go:build cooplivetest

package runtime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/liveprocess"
)

func TestLiveInterruptibleProcessGroupRequiresAuthenticatedControlFD(t *testing.T) {
	for _, valid := range []bool{false, true} {
		output, _, err := runLiveProcessGroupHelper(t, valid, false, false)
		if err != nil {
			t.Fatalf("valid=%t helper: %v (%s)", valid, err, output)
		}
		if valid && !strings.Contains(output, "inherited\n") {
			t.Fatalf("valid control result = %q", output)
		}
		if !valid && !strings.Contains(output, "rejected\n") {
			t.Fatalf("invalid control result = %q", output)
		}
	}
}

func TestLiveRunInterruptibleInheritsCallerGroup(t *testing.T) {
	output, _, err := runLiveProcessGroupHelper(t, true, false, false)
	if err != nil || !strings.Contains(output, "inherited\n") {
		t.Fatalf("live runtime group helper = %q, %v", output, err)
	}
}

func TestLiveRunInterruptibleRevokesCredentialsBeforeSignal(t *testing.T) {
	output, _, err := runLiveProcessGroupHelper(t, true, true, false)
	if err != nil || !strings.Contains(output, "revoked\n") {
		t.Fatalf("live runtime cancellation helper = %q, %v", output, err)
	}
}

func TestLiveRunInterruptibleLeavesRetryableTombstoneOnDeletionFailure(t *testing.T) {
	output, tombstone, err := runLiveProcessGroupHelper(t, true, true, true)
	if err != nil || !strings.Contains(output, "revocation_failed\n") {
		t.Fatalf("live runtime failed revocation helper = %q, %v", output, err)
	}
	if _, err := os.Stat(tombstone); err != nil {
		t.Fatalf("parent-known credential tombstone missing: %v", err)
	}
	if err := os.RemoveAll(tombstone); err != nil {
		t.Fatal(err)
	}
}

func TestLiveProcessGroupHelper(t *testing.T) {
	if os.Getenv("COOP_TEST_LIVE_RUNTIME_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	attrs, err := interruptibleProcessGroup()
	if os.Getenv("COOP_TEST_LIVE_RUNTIME_VALID") != "1" {
		if err == nil {
			t.Fatal("invalid live control was accepted")
		}
		os.Stdout.WriteString("rejected\n")
		return
	}
	if err != nil || attrs != nil {
		t.Fatalf("authenticated live process group = (%+v, %v), want inherited", attrs, err)
	}
	if os.Getenv("COOP_TEST_LIVE_RUNTIME_CANCEL") == "1" {
		removeFailure := os.Getenv("COOP_TEST_LIVE_RUNTIME_REMOVE_FAILURE") == "1"
		if removeFailure {
			removeLiveCredentialTree = func(string) error { return errors.New("injected removal failure") }
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ready := os.Getenv("COOP_TEST_LIVE_RUNTIME_READY")
		go func() {
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if _, err := os.Stat(ready); err == nil {
					cancel()
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			cancel()
		}()
		_, err := (Runtime{Name: "/bin/sh"}).RunInterruptible(
			ctx, nil, nil, nil, "-c",
			`trap 'test ! -e "$COOP_CONFIG_DIR" && printf revoked > "$COOP_TEST_REVOKE_OBSERVED"; exit 0' TERM; : > "$COOP_TEST_LIVE_RUNTIME_READY"; while :; do :; done`,
		)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("live runtime cancellation = %v", err)
		}
		if removeFailure {
			if _, err := os.Stat(os.Getenv(liveprocess.RevokePathEnv)); err != nil {
				t.Fatalf("failed revocation did not retain its tombstone: %v", err)
			}
			os.Stdout.WriteString("revocation_failed\n")
			return
		}
		data, err := os.ReadFile(os.Getenv("COOP_TEST_REVOKE_OBSERVED"))
		if err != nil || string(data) != "revoked" {
			t.Fatalf("runtime observed revocation = %q, %v", data, err)
		}
		os.Stdout.WriteString("revoked\n")
		return
	}
	var output strings.Builder
	code, err := (Runtime{Name: "/bin/sh"}).RunInterruptible(
		t.Context(), nil, &output, nil, "-c",
		"test ! -e /dev/fd/3; test -z \"${COOP_TEST_LIVE_CONTROL_FD+x}\"; test -z \"${COOP_TEST_LIVE_REVOKE_PATH+x}\"; ps -o pgid= -p $$",
	)
	if err != nil || code != 0 {
		t.Fatalf("live runtime group probe = code %d err %v", code, err)
	}
	got, err := strconv.Atoi(strings.TrimSpace(output.String()))
	if err != nil || got != syscall.Getpgrp() {
		t.Fatalf("live runtime pgid = %q, want caller group %d", output.String(), syscall.Getpgrp())
	}
	os.Stdout.WriteString("inherited\n")
}

func runLiveProcessGroupHelper(t *testing.T, valid, cancelRuntime, removeFailure bool) (string, string, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "control")
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	marker := liveprocess.ControlMarker
	if !valid {
		marker = "invalid\n"
	}
	if err := os.WriteFile(path, []byte(marker), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestLiveProcessGroupHelper$")
	configDir := filepath.Join(filepath.Dir(path), "config")
	revokePath := filepath.Join(filepath.Dir(configDir), ".coop-live-revoked-00000000000000000000000000000000")
	if err := os.Mkdir(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "credential"), []byte("projected"), 0o600); err != nil {
		t.Fatal(err)
	}
	observed := filepath.Join(filepath.Dir(path), "revoked")
	ready := filepath.Join(filepath.Dir(path), "ready")
	cmd.Env = []string{
		"PATH=/usr/bin:/bin", liveprocess.ControlFDEnv + "=3",
		"COOP_TEST_LIVE_RUNTIME_HELPER=1", "COOP_TEST_LIVE_RUNTIME_VALID=" + map[bool]string{true: "1"}[valid],
		"COOP_TEST_LIVE_RUNTIME_CANCEL=" + map[bool]string{true: "1"}[cancelRuntime],
		"COOP_TEST_LIVE_RUNTIME_REMOVE_FAILURE=" + map[bool]string{true: "1"}[removeFailure],
		"COOP_CONFIG_DIR=" + configDir, "COOP_TEST_REVOKE_OBSERVED=" + observed,
		liveprocess.RevokePathEnv + "=" + revokePath,
		"COOP_TEST_LIVE_RUNTIME_READY=" + ready,
	}
	cmd.ExtraFiles = []*os.File{file}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	err = cmd.Run()
	return output.String(), revokePath, err
}
