package cli

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
)

const (
	resumedSession = "6f1d2b3c-4a5e-4f60-8a7b-9c0d1e2f3a4b"
	otherSession   = "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
	boxStopped     = "\nThe Coop box has stopped: main process exited with status 0.\n"
)

// resumeFixture is one project and account whose box is a runtime shim: when a box runs, the shim
// appends a line to each session file named in writes, as the agent does while it works. The files
// sit where Claude keeps this project's sessions in its native home, and the shim records every call.
type resumeFixture struct {
	t         *testing.T
	configDir string
	repo      string
	boxHome   string
	shim      runtime.Runtime
	calls     string
	root      string // the project, as Coop resolves it
	home      string // the native home the box mounts
	sessions  string // where Claude keeps this project's sessions in it
}

func newResumeFixture(t *testing.T, account string, writes ...string) *resumeFixture {
	t.Helper()
	f := &resumeFixture{t: t, configDir: t.TempDir(), repo: t.TempDir(), boxHome: t.TempDir()}
	seed := f.app("claude", true)
	if account == "" {
		account = config.DefaultProfile
	} else {
		signInCred(t, seed.cfg, "claude", account)
	}
	var err error
	if f.root, err = box.ResolveRepo(f.repo); err != nil {
		t.Fatal(err)
	}
	// Prepared the way the launch will, so the sessions a test puts there before it survive it.
	if f.home, err = box.PrepareNativeHome(context.Background(), seed.cfg, "claude", account, f.root, false); err != nil {
		t.Fatal(err)
	}
	f.sessions = filepath.Join(f.home, "projects", agents.ClaudeProjectKey(box.Workdir(seed.cfg, f.root)))
	dir := t.TempDir()
	f.calls = filepath.Join(dir, "calls")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + strconv.Quote(f.calls) + "\n" +
		"if [ \"$1\" = run ]; then\n  mkdir -p " + strconv.Quote(f.sessions) + "\n"
	for _, name := range writes {
		script += "  printf '{}\\n' >> " + strconv.Quote(filepath.Join(f.sessions, name)) + "\n"
	}
	script += "fi\n"
	shim := filepath.Join(dir, "runtime")
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	f.shim = runtime.Runtime{Name: shim}
	return f
}

// app is a Coop process started with argv0 as its first word, offline because the shim has no
// network gateway. Each call is a new process, with a configuration of its own.
func (f *resumeFixture) app(argv0 string, homes bool) *app {
	cfg := &config.Config{ConfigDir: f.configDir, RepoOverride: f.repo, HomeInBox: "/home/node", BoxHome: f.boxHome,
		BaseImage: "test-base", ImageOverride: "test-image", Homes: homes}
	cfg.SetEgress("none")
	return &app{cfg: cfg, rt: f.shim, rtSet: true, argv: []string{argv0}}
}

// session is a session file Claude has already written here, before the run.
func (f *resumeFixture) session(id string) {
	f.t.Helper()
	if err := os.MkdirAll(f.sessions, 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.sessions, id+".jsonl"), []byte("{}\n"), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// launch runs `coop <target> <args>` and returns what Coop printed.
func (f *resumeFixture) launch(a *app, target string, args ...string) string {
	f.t.Helper()
	var code int
	var err error
	out := captureStderr(f.t, func() { code, err = a.launchAgent(target, args) })
	if code != 0 || err != nil {
		f.t.Fatalf("coop %s %v = (%d, %v)\n%s", target, args, code, err, out)
	}
	return out
}

// Claude prints `claude --resume <id>` as it exits, a command that cannot find the session Coop
// keeps in the project's native home. Once the box has stopped, Coop prints the command that can:
// the one the person typed, with --resume <id> added. A run that wrote no session, or could not be
// told from another, prints nothing.
func TestLaunchNamesTheCommandThatResumesTheSessionItWrote(t *testing.T) {
	for _, tc := range []struct {
		name    string
		target  string
		account string
		args    []string
		before  []string // sessions already there
		writes  []string // sessions the box writes
		noHome  bool     // COOP_HOMES=0: no native home is mounted
		want    string   // the command printed; "" for none
	}{
		{name: "a new session", target: "claude", writes: []string{resumedSession + ".jsonl"},
			want: "coop claude --resume " + resumedSession},
		{name: "a continued session, not the others", target: "claude", before: []string{resumedSession, otherSession},
			writes: []string{resumedSession + ".jsonl"}, want: "coop claude --resume " + resumedSession},
		{name: "the account it ran as", target: "claude@work", account: "work", writes: []string{resumedSession + ".jsonl"},
			want: "coop claude@work --resume " + resumedSession},
		{name: "the model and effort it ran with", target: "claude:opus/high", writes: []string{resumedSession + ".jsonl"},
			want: "coop claude:opus/high --resume " + resumedSession},
		{name: "a name the shell would expand", target: "claude:opus[1m]", writes: []string{resumedSession + ".jsonl"},
			want: "coop 'claude:opus[1m]' --resume " + resumedSession},
		{name: "a prompt typed after the name", target: "claude", args: []string{"fix the build"}, writes: []string{resumedSession + ".jsonl"},
			want: "coop claude --resume " + resumedSession},
		{name: "no conversation", target: "claude"},
		{name: "two sessions written, as another box sharing the home would", target: "claude",
			writes: []string{resumedSession + ".jsonl", otherSession + ".jsonl"}},
		{name: "files that are not sessions", target: "claude",
			writes: []string{"notes.jsonl", resumedSession + ".txt"}},
		{name: "no home mounted", target: "claude", writes: []string{resumedSession + ".jsonl"}, noHome: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newResumeFixture(t, tc.account, tc.writes...)
			for _, id := range tc.before {
				f.session(id)
			}
			out := f.launch(f.app(tc.target, !tc.noHome), tc.target, tc.args...)
			want := boxStopped
			if tc.want != "" {
				want += "\nResume this session with Coop:\n  " + tc.want + "\n"
			}
			if !strings.HasSuffix(out, want) {
				t.Errorf("output does not end with %q:\n%s", want, out)
			}
			if tc.want == "" && strings.Contains(out, "Resume this session") {
				t.Errorf("a run Coop cannot tie to one session was told how to resume:\n%s", out)
			}
		})
	}
}

// A preset starts its lead under the preset's own routing, so the command that reopens the session
// is the preset's, not the bare agent's.
func TestPresetLaunchNamesThePresetToResume(t *testing.T) {
	f := newResumeFixture(t, "", resumedSession+".jsonl")
	writePresetFile(t, f.repo, "solo", "lead:\n  agent: [claude]\n")
	a := f.app("solo", true)
	p, err := a.loadRunPreset("coop", "solo")
	if err != nil {
		t.Fatal(err)
	}
	var code int
	out := captureStderr(t, func() { code, err = a.launchPreset(p, nil) })
	if code != 0 || err != nil {
		t.Fatalf("coop solo = (%d, %v)\n%s", code, err, out)
	}
	if want := boxStopped + "\nResume this session with Coop:\n  coop solo --resume " + resumedSession + "\n"; !strings.HasSuffix(out, want) {
		t.Errorf("output does not end with %q:\n%s", want, out)
	}
}

// The printed command is the way back in: run as a new Coop process, it passes --resume <id> to
// Claude and mounts the native home the session was written to, so Claude finds it.
func TestThePrintedResumeCommandReopensTheSameSession(t *testing.T) {
	f := newResumeFixture(t, "work", resumedSession+".jsonl")
	out := f.launch(f.app("claude@work", true), "claude@work")
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	command := strings.Fields(lines[len(lines)-1])
	if want := []string{"coop", "claude@work", "--resume", resumedSession}; !slices.Equal(command, want) {
		t.Fatalf("printed command = %q, want %q\n%s", command, want, out)
	}
	f.launch(f.app(command[1], true), command[1], command[2:]...)
	var runs []string
	for _, call := range strings.Split(readFileString(f.calls), "\n") {
		if strings.HasPrefix(call, "run ") {
			runs = append(runs, call)
		}
	}
	if len(runs) != 2 {
		t.Fatalf("%d boxes ran, want 2:\n%s", len(runs), readFileString(f.calls))
	}
	if !strings.HasSuffix(runs[1], " claude --dangerously-skip-permissions --resume "+resumedSession) ||
		!strings.Contains(runs[1], "source="+f.home+",target=/home/node/.claude") {
		t.Errorf("the resumed box did not open session %s in the home it was written to (%s):\n%s", resumedSession, f.home, runs[1])
	}
}

// A command that answers one prompt, or manages Claude instead of opening it, leaves a transcript but
// no resume line of Claude's own, so Coop adds none either.
func TestNothingIsSaidAfterACommandThatOpensNoSession(t *testing.T) {
	base := []string{"claude", "--dangerously-skip-permissions"}
	for _, tc := range []struct {
		name string
		args []string
		want bool // the control: a run opened for a conversation does print
	}{
		{"a conversation", nil, true},
		{"one prompt", []string{"-p", "hi"}, false},
		{"one prompt, long flag", []string{"--print", "hi"}, false},
		{"a subcommand", []string{"mcp", "list"}, false},
		{"the version", []string{"--version"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newResumeFixture(t, "")
			note := f.app("claude", true).resumeNoteAfter("claude", f.root, append(slices.Clone(base), tc.args...))
			f.session(resumedSession) // what the run leaves behind
			want := ""
			if tc.want {
				want = "\nResume this session with Coop:\n  coop claude --resume " + resumedSession + "\n"
			}
			if out := captureStderr(t, note); out != want {
				t.Errorf("claude %v printed %q, want %q", tc.args, out, want)
			}
		})
	}
}
