package agent

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

const (
	watchedSession = "11111111-2222-4333-8444-555555555555"
	nextSession    = "22222222-2222-4333-8444-555555555555"
)

func claudeResumer(t *testing.T) SessionResumer {
	t.Helper()
	claude, ok := Get("claude")
	if !ok {
		t.Fatal("claude is not registered")
	}
	resumer, ok := claude.(SessionResumer)
	if !ok {
		t.Fatal("claude does not say how its sessions resume")
	}
	return resumer
}

func appendToSession(t *testing.T, path string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("{}\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

// What a run wrote is what differs between a reading before it and one after: a session it began
// is not there before, and one it continued has grown. A session it left alone is not listed.
func TestClaudeWatchSessionsListsWhatARunWrote(t *testing.T) {
	ws := "/work/repo"
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, bucket string)
		want []string
	}{
		{"nothing written", func(*testing.T, string) {}, nil},
		{"a session begun", func(t *testing.T, bucket string) {
			mustWrite(t, filepath.Join(bucket, nextSession+".jsonl"), "{}\n")
		}, []string{nextSession}},
		{"a session continued", func(t *testing.T, bucket string) {
			appendToSession(t, filepath.Join(bucket, watchedSession+".jsonl"))
		}, []string{watchedSession}},
		{"one begun and one continued", func(t *testing.T, bucket string) {
			appendToSession(t, filepath.Join(bucket, watchedSession+".jsonl"))
			mustWrite(t, filepath.Join(bucket, nextSession+".jsonl"), "{}\n")
		}, []string{watchedSession, nextSession}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			bucket := filepath.Join(home, "projects", ClaudeProjectKey(ws))
			mustWrite(t, filepath.Join(bucket, watchedSession+".jsonl"), "{}\n")
			written := claudeResumer(t).WatchSessions(home, ws)
			tc.run(t, bucket)
			if got := written(); !slices.Equal(got, tc.want) {
				t.Errorf("sessions written = %v, want %v", got, tc.want)
			}
		})
	}
	t.Run("a home with no sessions yet", func(t *testing.T) {
		home := t.TempDir()
		written := claudeResumer(t).WatchSessions(home, ws)
		mustWrite(t, filepath.Join(home, "projects", ClaudeProjectKey(ws), nextSession+".jsonl"), "{}\n")
		if got := written(); !slices.Equal(got, []string{nextSession}) {
			t.Errorf("sessions written = %v, want [%s]", got, nextSession)
		}
	})
}

// The box can write in the project's sessions folder, so only a regular file named for a valid
// session id counts, and only in this project's folder.
func TestClaudeWatchSessionsIgnoreWhatIsNotASession(t *testing.T) {
	home, ws := t.TempDir(), "/work/repo"
	bucket := filepath.Join(home, "projects", ClaudeProjectKey(ws))
	outside := filepath.Join(t.TempDir(), "outside.jsonl")
	mustWrite(t, outside, "{}\n")
	if err := os.MkdirAll(bucket, 0o700); err != nil {
		t.Fatal(err)
	}
	written := claudeResumer(t).WatchSessions(home, ws)
	mustWrite(t, filepath.Join(bucket, watchedSession+".jsonl"), "{}\n") // the one session
	mustWrite(t, filepath.Join(bucket, "notes.jsonl"), "{}\n")
	mustWrite(t, filepath.Join(bucket, "$(touch pwned);`id`.jsonl"), "{}\n")
	mustWrite(t, filepath.Join(bucket, nextSession+".txt"), "{}\n")
	mustWrite(t, filepath.Join(bucket, nextSession+".jsonl.bak"), "{}\n")
	if err := os.Mkdir(filepath.Join(bucket, nextSession+".jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(bucket, "33333333-2222-4333-8444-555555555555", "subagent.jsonl"), "{}\n")
	if err := os.Symlink(outside, filepath.Join(bucket, "44444444-2222-4333-8444-555555555555.jsonl")); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(home, "projects", ClaudeProjectKey(ws+"-other"), "55555555-2222-4333-8444-555555555555.jsonl"), "{}\n")
	if got := written(); !slices.Equal(got, []string{watchedSession}) {
		t.Errorf("sessions written = %v, want only %s", got, watchedSession)
	}
}

// A link where the sessions folder or the project's folder should be could lead the reading
// anywhere on the host, so neither is followed.
func TestClaudeWatchSessionsRefuseLinkedHistory(t *testing.T) {
	ws := "/work/repo"
	resumer := claudeResumer(t)
	t.Run("the sessions folder", func(t *testing.T) {
		home, outside := t.TempDir(), t.TempDir()
		if err := os.Symlink(outside, filepath.Join(home, "projects")); err != nil {
			t.Fatal(err)
		}
		written := resumer.WatchSessions(home, ws)
		mustWrite(t, filepath.Join(outside, ClaudeProjectKey(ws), watchedSession+".jsonl"), "{}\n")
		if got := written(); len(got) != 0 {
			t.Errorf("a linked sessions folder was read: %v", got)
		}
	})
	linkProject := func(t *testing.T, home, target string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(home, "projects"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(home, "projects", ClaudeProjectKey(ws))); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("the project's folder, to a folder outside", func(t *testing.T) {
		home, outside := t.TempDir(), t.TempDir()
		linkProject(t, home, outside)
		written := resumer.WatchSessions(home, ws)
		mustWrite(t, filepath.Join(outside, watchedSession+".jsonl"), "{}\n")
		if got := written(); len(got) != 0 {
			t.Errorf("a project folder linked outside was read: %v", got)
		}
	})
	// The Go runtime follows a link that stays inside the sessions folder, so only the check in
	// claudeSessionStamps keeps this one out.
	t.Run("the project's folder, to another project's", func(t *testing.T) {
		home := t.TempDir()
		linkProject(t, home, "elsewhere")
		written := resumer.WatchSessions(home, ws)
		mustWrite(t, filepath.Join(home, "projects", "elsewhere", watchedSession+".jsonl"), "{}\n")
		if got := written(); len(got) != 0 {
			t.Errorf("a project folder linked to another's was read: %v", got)
		}
	})
}

// Every reading closes what it opens: a launch reads twice, and a long-lived session many times.
func TestClaudeWatchSessionsLeaveNoDescriptorOpen(t *testing.T) {
	home, ws := t.TempDir(), "/work/repo"
	resumer := claudeResumer(t)
	mustWrite(t, filepath.Join(home, "projects", ClaudeProjectKey(ws), watchedSession+".jsonl"), "{}\n")
	before := openSessionFDCount()
	for range 32 {
		resumer.WatchSessions(home, ws)()
		resumer.WatchSessions(home, ws+"-missing")()
	}
	if after := openSessionFDCount(); before >= 0 && after > before {
		t.Fatalf("repeated readings leaked descriptors: before=%d after=%d", before, after)
	}
}

func TestClaudeResumeArgs(t *testing.T) {
	if got := claudeResumer(t).ResumeArgs(watchedSession); !slices.Equal(got, []string{"--resume", watchedSession}) {
		t.Errorf("ResumeArgs = %v", got)
	}
}
