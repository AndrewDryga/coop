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
		agent        Agent
	}{
		{name: "claude", marker: ".credentials.json", agent: claudeAgent{}},
		{name: "codex", marker: "auth.json", agent: codexAgent{}},
		{name: "grok", marker: "auth.json", agent: grokAgent{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile := t.TempDir()
			if err := syscall.Mkfifo(filepath.Join(profile, tc.marker), 0o600); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				if got := tc.agent.StoredCredentialStatus(profile, time.Now()); got == StoredCredentialReady {
					t.Errorf("FIFO credential reported ready")
				}
				if got := tc.agent.LiveCredentials().Portability(profile, time.Now().Add(time.Hour)); got == CredentialPortable {
					t.Errorf("FIFO credential reported portable")
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
