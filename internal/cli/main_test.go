package cli

import (
	"encoding/base64"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/tasks"
)

const (
	detachedHandoffModeEnv        = "COOP_TEST_DETACHED_HANDOFF_MODE"
	detachedHandoffReadyEnv       = "COOP_TEST_DETACHED_HANDOFF_READY"
	detachedHandoffReleaseEnv     = "COOP_TEST_DETACHED_HANDOFF_RELEASE"
	detachedHandoffRepoEnv        = "COOP_TEST_DETACHED_HANDOFF_REPO"
	detachedHandoffNameEnv        = "COOP_TEST_DETACHED_HANDOFF_NAME"
	detachedHandoffReservationEnv = "COOP_TEST_DETACHED_HANDOFF_RESERVATION"
)

func TestMain(m *testing.M) {
	switch os.Getenv(detachedHandoffModeEnv) {
	case "cli":
		if err := detachedHandoffReadyAndWait(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(97)
		}
		os.Exit(Main(os.Args[1:]))
	case "publisher":
		reservation, err := base64.RawURLEncoding.DecodeString(os.Getenv(detachedHandoffReservationEnv))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(97)
		}
		if err := forkspace.PublishReservedWorker(os.Getenv(detachedHandoffRepoEnv), os.Getenv(detachedHandoffNameEnv), reservation, os.Getpid()); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(97)
		}
		if err := os.WriteFile(os.Getenv(detachedHandoffReadyEnv), []byte("ready\n"), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(97)
		}
		for {
			time.Sleep(time.Hour)
		}
	}
	root, err := os.MkdirTemp("", "coop-test-task-leases-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.Setenv(tasks.TestLeaseAuthorityRootEnv, root); err != nil {
		fmt.Fprintln(os.Stderr, err)
		_ = os.RemoveAll(root)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(root)
	os.Exit(code)
}

func detachedHandoffReadyAndWait() error {
	if err := os.WriteFile(os.Getenv(detachedHandoffReadyEnv), []byte("ready\n"), 0o600); err != nil {
		return err
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(os.Getenv(detachedHandoffReleaseEnv)); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for detached handoff release")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
