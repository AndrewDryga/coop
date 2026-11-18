//go:build darwin || linux

package agent

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestProviderCredentialReadersRefuseFIFOsWithoutBlocking(t *testing.T) {
	for _, tc := range []struct {
		name, marker string
		read         func(profile string) bool // whether the reader accepted the file
	}{
		{name: "claude", marker: ".credentials.json", read: func(profile string) bool {
			_, err := ReadCredentialArtifact(filepath.Join(profile, ".credentials.json"), claudeCredentialLimit)
			return err == nil
		}},
		{name: "codex", marker: "auth.json", read: func(profile string) bool {
			_, err := ReadCredentialArtifact(filepath.Join(profile, "auth.json"), codexCredentialLimit)
			return err == nil
		}},
		{name: "grok", marker: "auth.json", read: func(profile string) bool {
			_, err := ReadCredentialArtifact(filepath.Join(profile, "auth.json"), grokCredentialLimit)
			return err == nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile := t.TempDir()
			if err := syscall.Mkfifo(filepath.Join(profile, tc.marker), 0o600); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				if tc.read(profile) {
					t.Errorf("FIFO credential was read")
				}
			}()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				// Let a regressed blocking reader finish so the test process does not strand a goroutine.
				if unblock, err := os.OpenFile(filepath.Join(profile, tc.marker), os.O_RDWR|syscall.O_NONBLOCK, 0); err == nil {
					_ = unblock.Close()
				}
				t.Fatal("provider credential reader blocked on a FIFO")
			}
		})
	}
}
