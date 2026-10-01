package agent

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestUsageRefreshLockIncludesDeadline(t *testing.T) {
	for _, tc := range []struct {
		name, file string
		renew      func(string, time.Time) error
	}{
		{"claude", ".credentials.json.refresh.lock", renewClaudeCredential},
		{"codex", "auth.json.refresh.lock", renewCodexCredential},
		{"grok", "auth.json.lock", renewGrokCredential},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile := t.TempDir()
			lock, err := os.OpenFile(filepath.Join(profile, tc.file), os.O_CREATE|os.O_RDWR, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- tc.renew(profile, time.Now().Add(25*time.Millisecond)) }()
			select {
			case err := <-done:
				if err == nil {
					t.Error("held refresh lock was accepted")
				}
			case <-time.After(time.Second):
				t.Error("refresh waited past its deadline behind another writer")
				// Release the fixture before awaiting its worker; a broken implementation must
				// not leave a refresh goroutine behind after this regression fails.
				_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
				<-done
			}
		})
	}
}
