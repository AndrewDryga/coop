package cli

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
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
// sit where the selected provider keeps this project’s sessions, and the shim records every call.
type resumeFixture struct {
	t         *testing.T
	configDir string
	repo      string
	boxHome   string
	shim      runtime.Runtime
	calls     string
	root      string // the project, as Coop resolves it
	home      string // the native home the box mounts
	provider  string
	cwd       string
}

func newResumeFixture(t *testing.T, provider, account string, writes ...string) *resumeFixture {
	t.Helper()
	f := &resumeFixture{t: t, configDir: t.TempDir(), repo: t.TempDir(), boxHome: t.TempDir(), provider: provider}
	seed := f.app(provider, true)
	if account == "" {
		account = config.DefaultProfile
	} else {
		signInCred(t, seed.cfg, provider, account)
	}
	var err error
	if f.root, err = box.ResolveRepo(f.repo); err != nil {
		t.Fatal(err)
	}
	// Prepared the way the launch will, so the sessions a test puts there before it survive it.
	if f.home, err = box.PrepareNativeHome(context.Background(), seed.cfg, provider, account, f.root, false); err != nil {
		t.Fatal(err)
	}
	f.cwd = box.Workdir(seed.cfg, f.root)
	if provider == "gemini" {
		if err := os.MkdirAll(filepath.Join(f.home, "tmp", "project"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.home, "tmp", "project", ".project_root"), []byte(f.cwd+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	f.calls = filepath.Join(dir, "calls")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + shellWord(f.calls) + "\nif [ \"$1\" = run ]; then\n  :\n"
	for _, name := range writes {
		path, contents := f.sessionFile(name)
		script += "  mkdir -p " + shellWord(filepath.Dir(path)) + "\n" +
			"  if [ ! -f " + shellWord(path) + " ]; then printf '%s\\n' " + shellWord(contents) + " > " + shellWord(path) + "; fi\n" +
			"  printf '\\n' >> " + shellWord(path) + "\n"
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

// sessionFile describes the pinned client's saved session format.
func (f *resumeFixture) sessionFile(name string) (string, string) {
	id := strings.TrimSuffix(name, ".jsonl")
	switch f.provider {
	case "codex":
		return filepath.Join(f.home, "sessions", "2026", "11", "23", "rollout-2026-11-23T10-00-00-"+name),
			fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"cwd":%q,"source":"cli"}}`, id, f.cwd)
	case "gemini":
		return filepath.Join(f.home, "tmp", "project", "chats", "session-2026-11-23T10-00-"+id[:8]+".jsonl"),
			fmt.Sprintf(`{"sessionId":%q,"projectHash":"%x","kind":"main"}`, id, sha256.Sum256([]byte(f.cwd)))
	case "grok":
		return filepath.Join(f.home, "sessions", url.PathEscape(f.cwd), id, "summary.json"), `{"session_kind":"interactive"}`
	default:
		return filepath.Join(f.home, "projects", agents.ClaudeProjectKey(f.cwd), name), "{}"
	}
}

func (f *resumeFixture) session(id string) {
	f.t.Helper()
	path, contents := f.sessionFile(id + ".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents+"\n"), 0o600); err != nil {
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
			f := newResumeFixture(t, "claude", tc.account, tc.writes...)
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
	f := newResumeFixture(t, "claude", "", resumedSession+".jsonl")
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
			f := newResumeFixture(t, "claude", "")
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

// The footer uses native metadata for a newly saved conversation and an append to a resumed one.
// Both launches run the printed target against the same account home.
func TestEveryProviderPrintsTheExactInteractiveResumeCommand(t *testing.T) {
	for _, provider := range agents.Names() {
		t.Run(provider, func(t *testing.T) {
			target := provider + "@work"
			f := newResumeFixture(t, provider, "work", resumedSession+".jsonl")
			f.session(otherSession)
			verb := "--resume"
			if provider == "codex" {
				verb = "resume"
			}
			want := "\nResume this session with Coop:\n  coop " + target + " " + verb + " " + resumedSession + "\n"
			a := f.app(provider, true)
			a.cfg.PinDefaultProfile(provider, "work")
			out := f.launch(a, provider)
			if !strings.HasSuffix(out, boxStopped+want) {
				t.Fatalf("expected %q at the end of the launch, got:\n%s", want, out)
			}
			lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
			command := strings.Fields(lines[len(lines)-1])
			out = f.launch(f.app(command[1], true), command[1], command[2:]...)
			if !strings.HasSuffix(out, boxStopped+want) {
				t.Errorf("updating the resumed session should print %q, got:\n%s", want, out)
			}
			calls := readFileString(f.calls)
			var lastRun string
			for _, line := range strings.Split(calls, "\n") {
				if strings.HasPrefix(line, "run ") {
					lastRun = line
				}
			}
			if !strings.HasSuffix(lastRun, " "+verb+" "+resumedSession) ||
				!strings.Contains(lastRun, "source="+f.home+",target=/home/node/."+provider) {
				t.Errorf("resume command did not pass the exact ID and mount the same home:\n%s", lastRun)
			}
		})
	}
}

func TestEveryProviderOmitsAnAmbiguousOrHeadlessResumeCommand(t *testing.T) {
	for _, provider := range agents.Names() {
		t.Run(provider, func(t *testing.T) {
			for _, tc := range []struct {
				name   string
				writes []string
				args   []string
			}{
				{name: "several sessions", writes: []string{resumedSession + ".jsonl", otherSession + ".jsonl"}},
				{name: "no session"},
				{name: "one prompt", args: []string{"-p", "hi"}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					f := newResumeFixture(t, provider, "work", tc.writes...)
					target := provider + "@work"
					a := f.app(target, true)
					var out string
					if len(tc.args) == 0 {
						out = f.launch(a, target)
					} else {
						// An offline integration launch refuses headless commands before the runtime.
						a.cfg.SetActiveProfile(provider, "work")
						cmd := append(a.defaultCmd(provider), agentArgs(provider, tc.args)...)
						note := a.resumeNoteAfter(provider, f.root, cmd)
						f.session(resumedSession)
						out = captureStderr(t, note)
					}
					if strings.Contains(out, "Resume this session") {
						t.Errorf("printed a resume hint:\n%s", out)
					}
				})
			}
		})
	}
}

func TestResumeCommandIgnoresOtherNativeSessionKinds(t *testing.T) {
	for _, tc := range []struct {
		provider string
		old, new string
	}{
		{provider: "codex", old: `"source":"cli"`, new: `"source":"exec"`},
		{provider: "gemini", old: `"kind":"main"`, new: `"kind":"subagent"`},
		{provider: "grok", old: `"session_kind":"interactive"`, new: `"session_kind":"headless"`},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			f := newResumeFixture(t, tc.provider, "")
			a := f.app(tc.provider, true)
			note := a.resumeNoteAfter(tc.provider, f.root, a.defaultCmd(tc.provider))
			f.session(resumedSession)
			f.session(otherSession)
			file, contents := f.sessionFile(otherSession + ".jsonl")
			if !strings.Contains(contents, tc.old) {
				t.Fatal("fixture did not include the expected native marker")
			}
			if err := os.WriteFile(file, []byte(strings.Replace(contents, tc.old, tc.new, 1)+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if out := captureStderr(t, note); !strings.Contains(out, " "+resumedSession+"\n") || strings.Contains(out, otherSession) {
				t.Errorf("expected only the interactive resume command, got %q", out)
			}
		})
	}
}

func TestResumeCommandPinsAnAddressableBuiltinDefault(t *testing.T) {
	f := newResumeFixture(t, "codex", config.DefaultProfile, resumedSession+".jsonl")
	out := f.launch(f.app("codex", true), "codex")
	if want := "  coop codex@" + config.DefaultProfile + " resume " + resumedSession + "\n"; !strings.HasSuffix(out, want) {
		t.Errorf("expected the saved account in the resume command, got:\n%s", out)
	}
}

func TestResumeCommandDoesNotFollowARuntimeRefusal(t *testing.T) {
	f := newResumeFixture(t, "gemini", "work", resumedSession+".jsonl")
	// Model a different process saving into the history during an attempted launch.
	script := strings.Replace(readFileString(f.shim.Name), "\nfi\n", "\n  exit 125\nfi\n", 1)
	if err := os.WriteFile(f.shim.Name, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	a := f.app("gemini@work", true)
	var code int
	var err error
	out := captureStderr(t, func() { code, err = a.launchAgent("gemini@work", nil) })
	if code != 125 || err != nil {
		t.Fatalf("expected runtime exit 125, got (%d, %v):\n%s", code, err, out)
	}
	if strings.Contains(out, "Resume this session") {
		t.Errorf("a refused launch printed a resume command:\n%s", out)
	}
}
