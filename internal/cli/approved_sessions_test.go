package cli

import (
	"bytes"
	"errors"
	"strings"
	"syscall"
	"testing"

	"github.com/AndrewDryga/coop/internal/sessionsvc"
	"github.com/AndrewDryga/coop/internal/ui"
)

const approvedPolicyFile = "~/.config/coop/session-policies.yaml"

func review() sessionConfigurationView {
	return sessionConfigurationView{
		Name: "code-review", Repository: "/Users/andrewdryga/Projects/os/coop", Access: "Read-only",
		Targets: []string{"codex:gpt-5.6-sol/high@personal"},
		Network: "Filtered — only approved network traffic is allowed",
		Rules:   []string{"OpenAI provider endpoints", "github.com:443 · TLS"},
	}
}

// TestApprovedSessionConfigurations pins the human view of `coop sessions policies` — the question
// it answers is what a remote application may ask this machine to do, so every block leads with the
// project, the agents, the file access and the resolved network rules. Digests stay in --json.
func TestApprovedSessionConfigurations(t *testing.T) {
	work := sessionConfigurationView{
		Name: "code-work", Repository: "/Users/andrewdryga/Projects/os/coop",
		Access:  "Read, edit, and commit in a separate project copy",
		Targets: []string{"codex:gpt-5.6-sol/high@personal", "claude:opus/high@work"},
		Network: "Filtered — only approved network traffic is allowed",
		Rules: []string{"OpenAI and Anthropic provider endpoints", "github.com:443 · TLS",
			"registry.npmjs.org:443 · TLS"},
	}
	export := review()
	export.Export = true
	unresolved := sessionConfigurationView{
		Name: "code-review", Repository: "/Users/andrewdryga/Projects/os/coop", Access: "Read-only",
		Targets:          []string{"codex:gpt-5.6-sol/high@personal"},
		Issue:            "This project's network rules have not been approved.",
		ApprovalRequired: true,
	}
	bare := sessionConfigurationView{
		Name: "questions", Access: "Questions and answers only; no project files or tools",
		Targets: []string{"claude:opus@personal"}, Network: "Unrestricted",
	}
	cases := []struct {
		fixture string
		views   []sessionConfigurationView
	}{
		{"71-sessions-policies", []sessionConfigurationView{review(), work}},
		{"71-sessions-policies-export", []sessionConfigurationView{export}},
		{"71-sessions-policies-unresolved", []sessionConfigurationView{unresolved}},
		{"71-sessions-policies-bare", []sessionConfigurationView{bare}},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			var out bytes.Buffer
			renderSessionConfigurations(&out, ui.Palette{}, approvedPolicyFile, tc.views)
			assertApprovedOutput(t, tc.fixture, out.String())
		})
	}
}

// A configuration file that defines nothing is INVALID, not a successful empty list.
func TestApprovedEmptySessionConfigurations(t *testing.T) {
	err := ui.CommandFailed("Could not load remote session configurations",
		"/Users/andrewdryga/.config/coop/session-policies.yaml must define at least one configuration.",
		[2]string{"Help:", "coop help sessions policies"})
	assertApprovedOutput(t, "71-sessions-policies-empty", usageBlock(t, err))
}

// The doctor's failures name what is actually wrong. "No service is listening" is claimed only for
// a socket that refused the connection; a service that answered but is not ready is its own
// failure; and a socket the user named keeps its own remedy rather than the default service's.
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
