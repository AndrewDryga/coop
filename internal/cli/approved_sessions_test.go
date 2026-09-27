package cli

import (
	"errors"
	"github.com/AndrewDryga/coop/internal/sessionsvc"
	"strings"
	"syscall"
	"testing"
)

func TestApprovedSessionDoctorFailures(t *testing.T) {
	const socket = "/Users/andrewdryga/.local/state/coop/sessions/control.sock"
	cases := []struct {
		fixture     string
		result      sessionDoctorResult
		socketGiven bool
		healthErr   error
	}{
		{"70-sessions-doctor-absent", sessionDoctorResult{Socket: socket}, false, syscall.ECONNREFUSED},
		{"70-sessions-doctor-unready", sessionDoctorResult{Socket: socket, Healthy: true}, false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			err := sessionDoctorFailure(tc.result, tc.socketGiven, tc.healthErr)
			assertApprovedOutput(t, tc.fixture, usageBlock(t, err))
		})
	}
	// A named socket must not be answered with a command that would listen somewhere else.
	custom := sessionDoctorFailure(sessionDoctorResult{Socket: "/tmp/other.sock"}, true, syscall.ECONNREFUSED)
	if got := usageBlock(t, custom); !strings.Contains(got, "Start the service that listens at /tmp/other.sock.") {
		t.Errorf("a custom socket lost its context:\n%s", got)
	}
}

// The compaction stages a failure can be in are read from what the run PROVED: a verified backup
// exists only once its path is set, and only the post-commit marker proves the rewrite landed.
func TestApprovedSessionCompactFailures(t *testing.T) {
	const backup = "/path/to/session-backup.sqlite"
	retained := sessionsvc.CompactionResult{BackupPath: backup}
	cases := []struct {
		fixture string
		result  sessionsvc.CompactionResult
		err     error
	}{
		{"72-compact-active-service", sessionsvc.CompactionResult{},
			errors.New("another session daemon owns this state root")},
		{"72-compact-existing-backup", sessionsvc.CompactionResult{},
			errors.New(backup + " already exists.")},
		{"72-compact-unfinished", retained,
			errors.Join(sessionsvc.ErrCompactionUnfinished, errors.New("database or disk is full"))},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			assertApprovedOutput(t, tc.fixture, usageBlock(t, sessionCompactFailure(tc.result, tc.err)))
		})
	}
	// Nothing is claimed retained before the backup verified: backupSQLiteDatabase deletes its own
	// incomplete output, so there is no Backup: row to print.
	before := usageBlock(t, sessionCompactFailure(sessionsvc.CompactionResult{}, errors.New("short read")))
	if !strings.Contains(before, "The session backup could not be verified.") || strings.Contains(before, "Backup:") {
		t.Errorf("a failure before verification must claim no retained backup:\n%s", before)
	}
}
