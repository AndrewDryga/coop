package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/ui"
	"github.com/AndrewDryga/coop/internal/workerconnector"
)

// Every sign refusal names what the signer actually found and the one action that resolves it. A
// key problem is never guessed for a branch that moved or a range that is not linear.
func TestApprovedSignFailures(t *testing.T) {
	cases := []struct {
		fixture string
		err     error
	}{
		{"74-sign-no-upstream", ui.CommandFailed("Could not determine which commits to sign", "This branch has no upstream.",
			[2]string{"", "Use --from with the last commit you pushed."}, [2]string{"Help:", "coop help sign"})},
		{"74-sign-merge-range", ui.CommandFailed("Could not sign these commits", "The selected range contains a merge commit.",
			[2]string{"", "Choose a linear range with --from."}, [2]string{"Help:", "coop help sign"})},
		{"74-sign-detached-head", ui.CommandFailed("Could not sign these commits", "No branch is checked out.",
			[2]string{"", "Check out the branch you want to sign."})},
		{"74-sign-branch-moved", ui.CommandFailed("Could not apply the signed commits", "The branch changed while Coop was signing.",
			[2]string{"", "Review the current branch before running coop sign again."})},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) { assertApprovedOutput(t, tc.fixture, usageBlock(t, tc.err)) })
	}
	// The same blocks are what the real parser and signer produce for those conditions.
	if _, err := signBase(t.TempDir(), ""); err == nil {
		t.Fatal("a repo with no upstream should refuse to pick a range")
	} else {
		assertApprovedOutput(t, "74-sign-no-upstream", usageBlock(t, err))
	}
}

// The two completion scripts are printed verbatim on stdout: no banner, no installation claim, no
// footer — and their comments name the SAME user-controlled path the help page does.
func TestApprovedCompletionScripts(t *testing.T) {
	for _, tc := range []struct{ fixture, shell string }{
		{"75-completion-bash", "bash"},
		{"75-completion-zsh", "zsh"},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			out := captureStdout(t, func() {
				if code, err := cmdCompletion([]string{tc.shell}); code != 0 || err != nil {
					t.Fatalf("coop completion %s exited (%d, %v)", tc.shell, code, err)
				}
			})
			assertApprovedOutput(t, tc.fixture, out)
		})
	}
	// An unsupported shell is rejected through the shared renderer, naming only accepted values.
	code, err := cmdCompletion([]string{"fish"})
	if code != 2 || err == nil {
		t.Fatalf("coop completion fish = (%d, %v), want a usage refusal", code, err)
	}
	if block := usageBlock(t, err); !strings.Contains(block, "bash") || !strings.Contains(block, "zsh") {
		t.Errorf("the refusal should name the accepted shells:\n%s", block)
	}
}

// `coop prompt` is ONE uncolored line of non-empty segments, in a fixed order, and nothing at all
// when the project is idle — an embedding shell prompt must stay clean.
func TestApprovedPromptLine(t *testing.T) {
	got := promptLine(tasks.TaskCounts{Todo: 2, Doing: 1, Blocked: 1}, 3, 2, true)
	if want := "2 todo · 1 in progress · 1 blocked · 3 forks (2 running) · unsigned commit"; got != want {
		t.Errorf("populated prompt = %q, want %q", got, want)
	}
	for _, tc := range []struct {
		want           string
		counts         tasks.TaskCounts
		forks, looping int
		unsigned       bool
	}{
		{"2 todo · 1 in progress", tasks.TaskCounts{Todo: 2, Doing: 1}, 0, 0, false},
		{"1 fork", tasks.TaskCounts{}, 1, 0, false},
		{"1 fork (1 running)", tasks.TaskCounts{}, 1, 1, false},
		{"unsigned commit", tasks.TaskCounts{}, 0, 0, true},
		{"", tasks.TaskCounts{Done: 9}, 0, 0, false},
	} {
		if got := promptLine(tc.counts, tc.forks, tc.looping, tc.unsigned); got != tc.want {
			t.Errorf("prompt = %q, want %q", got, tc.want)
		}
	}
}

// `coop sessions connect` validates the configuration BEFORE anything else: a bad file must never
// start a service, and the refusal names the file, the loader's own cause, and the page.
func TestApprovedSessionConnectRejectsItsConfigurationFirst(t *testing.T) {
	assertApprovedOutput(t, "73-sessions-connect-invalid-config", usageBlock(t,
		sessionConnectFailure("/path/to/worker.json", errors.New("unsupported worker configuration version 2"))))

	dir := t.TempDir()
	path := filepath.Join(dir, "worker.json")
	if err := os.WriteFile(path, []byte(`{"version":99}`), 0o600); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "sessions")
	t.Setenv("HOME", dir)
	code, err := runSessionConnect(freshConfig(t), path)
	if code != 1 || err == nil {
		t.Fatalf("an unsupported configuration = (%d, %v), want a refusal", code, err)
	}
	if block := usageBlock(t, err); !strings.Contains(block, "Could not start the worker") ||
		!strings.Contains(block, "coop help sessions connect") {
		t.Errorf("configuration refusal lost its shape:\n%s", block)
	}
	// Nothing was started: a rejected configuration never creates session state.
	if _, err := os.Stat(state); err == nil {
		t.Error("a rejected configuration started a local session service")
	}
}

// A configured coop_socket must AGREE with the resolved session data directory. A socket pointing
// somewhere else would connect the controller to a service whose policies nobody checked.
func TestSessionConnectPathsRefuseAForeignSocket(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	state := filepath.Join(home, "sessions")
	_, _, socket, err := sessionConnectPaths(workerConfigFor(state, filepath.Join(state, "control.sock")))
	if err != nil || socket != filepath.Join(state, "control.sock") {
		t.Fatalf("a socket inside the state root = (%q, %v), want it accepted", socket, err)
	}
	if _, _, _, err := sessionConnectPaths(workerConfigFor(state, filepath.Join(home, "elsewhere.sock"))); err == nil {
		t.Fatal("a socket outside the session data directory was accepted")
	}
	// With no explicit state directory, the documented default decides — never the socket's parent.
	resolvedState, policy, _, err := sessionConnectPaths(workerConfigFor("", ""))
	if err != nil {
		t.Fatal(err)
	}
	if resolvedState != filepath.Join(home, ".local", "state", "coop", "sessions") {
		t.Errorf("default session data directory = %q", resolvedState)
	}
	if policy != filepath.Join(home, ".config", "coop", "session-policies.yaml") {
		t.Errorf("default session policy = %q", policy)
	}
}

// workerConfigFor is the minimum of a worker configuration this slice reads: which local session
// service it is about, and the socket it expects to reach it on.
func workerConfigFor(state, socket string) workerconnector.Config {
	return workerconnector.Config{SessionStateDir: state, CoopSocket: socket}
}
