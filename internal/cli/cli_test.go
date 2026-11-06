package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/ui"
)

func useEmptyMainConfig(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "coop.conf")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COOP_CONF", path)
}

// The fallback error line can quote project.yaml, which a box can write: yaml quotes a value it
// could not use, and an unknown key, as written. The line escapes what would act on the terminal,
// a newline inside a quoted key does not start a line of its own, and the line breaks and tabs an
// error puts there itself stay.
func TestFallbackErrorLineEscapesBoxChosenText(t *testing.T) {
	color := regexp.MustCompile("\x1b\\[[0-9;]*m")
	for _, tc := range []struct{ name, file, want string }{
		{"a value", "serve:\n  ports: [\"evil\\x1b[2J\\nfake\"]\n", `"evil\u001B[2..." should be a number`},
		{"a key", "\"evil\\x1b[2J\\nfake\": 1\n", `unknown setting "evil\u001B[2J\u000Afake"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := project.Parse([]byte(tc.file))
			if err == nil {
				t.Fatal("project.yaml parsed")
			}
			out := color.ReplaceAllString(captureStderr(t, func() { reportExit(-1, err) }), "")
			if strings.ContainsRune(out, '\x1b') || !strings.Contains(out, tc.want) || strings.Count(out, "\n") != 2 {
				t.Errorf("error line = %q, want it escaped on yaml's two lines with %q", out, tc.want)
			}
		})
	}
	planted := fmt.Errorf("read task: %w", &fs.PathError{Op: "open", Path: "/repo/evil\nfake", Err: fs.ErrPermission})
	if out := color.ReplaceAllString(captureStderr(t, func() { reportExit(-1, planted) }), ""); out != "✗ read task: open /repo/evil\\u000Afake: permission denied\n" {
		t.Errorf("file error = %q, want the file name's newline escaped", out)
	}
	out := color.ReplaceAllString(captureStderr(t, func() { reportExit(-1, errors.New("could not commit\n\tgit: index.lock exists")) }), "")
	if out != "✗ could not commit\n\tgit: index.lock exists\n" {
		t.Errorf("multi-line error = %q, want its own line break and tab kept", out)
	}
}

// A bad setting is refused while the settings load, so the runtime is never called. Docker refuses a
// bad memory or CPU cap, or a relative path in the box, only once the box is being created: the QA
// run saw COOP_MEMORY=lots reach "Starting" and fail there with "Docker operation failed; outcome
// unknown".
func TestMainRejectsInvalidConfigBeforeRuntime(t *testing.T) {
	for _, tc := range []struct {
		name, conf, key, value string
		want                   []string
	}{
		{"a boolean in coop.conf", "COOP_HOMES=flase\n", "", "", []string{"coop.conf:1", "Invalid value for COOP_HOMES."}},
		{"a memory cap in coop.conf", "COOP_MEMORY=lots\n", "", "", []string{"coop.conf:1", "Invalid value for COOP_MEMORY.", "Use a size such as 512m or 4g."}},
		{"a memory cap", "", "COOP_MEMORY", "lots", []string{"✗ Invalid value for COOP_MEMORY", "Use a size such as 512m or 4g."}},
		{"a CPU cap", "", "COOP_CPUS", "many", []string{"✗ Invalid value for COOP_CPUS", "Use a number of CPUs such as 0.5 or 2."}},
		{"a relative working folder", "", "COOP_WORKDIR", "relative", []string{`✗ COOP_WORKDIR must be an absolute path, not "relative"`}},
		{"a relative home in the box", "", "COOP_HOME_IN_BOX", "home", []string{`✗ COOP_HOME_IN_BOX must be an absolute path, not "home"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conf := filepath.Join(t.TempDir(), "coop.conf")
			if err := os.WriteFile(conf, []byte(tc.conf), 0o600); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(t.TempDir(), "runtime-called")
			runtime := filepath.Join(t.TempDir(), "runtime")
			script := "#!/bin/sh\n: > \"" + marker + "\"\n"
			if err := os.WriteFile(runtime, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("COOP_CONF", conf)
			t.Setenv("COOP_RUNTIME", runtime)
			if tc.key != "" {
				t.Setenv(tc.key, tc.value)
			}

			stderr := captureStderr(t, func() {
				if code := Main([]string{"run", "--", "true"}); code != 1 {
					t.Errorf("Main invalid config exit = %d, want 1", code)
				}
			})
			for _, want := range tc.want {
				if !strings.Contains(stderr, want) {
					t.Errorf("invalid config stderr = %q, want %q", stderr, want)
				}
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("runtime marker stat = %v, want runtime untouched", err)
			}
		})
	}
}

// A broken settings file reads like every other refusal: the headline, then the file and line and
// the problem on their own lines, then the fix. The QA run saw one line with ".:" between the parts.
func TestMainDrawsSettingsErrorsAsABlock(t *testing.T) {
	t.Setenv("COOP_NO_UPDATE_CHECK", "1")
	t.Setenv("COOP_RUNTIME", filepath.Join(t.TempDir(), "missing-runtime"))
	for _, tc := range []struct {
		name, body, line, problem, action string
	}{
		{"a line that is not a setting", "COOP_PIDS=512\nthis is not valid\n", ":2",
			"This line is not a setting.", "Write settings as KEY=value, one per line."},
		{"a setting twice", "COOP_PIDS=512\nCOOP_PIDS=256\n", ":2",
			`Setting "COOP_PIDS" is set twice in this file.`, "Keep the line you want and delete the other."},
		{"an invalid value", "COOP_CACHE=maybe\n", ":1",
			"Invalid value for COOP_CACHE.", "Use true or false."},
		{"an environment-only setting", "COOP_HOME=/x\n", ":1",
			"COOP_HOME can only be set in the environment, not in coop.conf.", "Delete this line, and set it in the environment instead."},
		{"a missing COOP_CONF file", "", "",
			"No such file or directory.", "Fix that file, or point COOP_CONF somewhere else."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "nope.conf")
			if tc.body != "" {
				path = filepath.Join(t.TempDir(), "coop.conf")
				if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("COOP_CONF", path)
			var code int
			var stdout string
			stderr := captureStderr(t, func() { stdout = captureStdout(t, func() { code = Main([]string{"run", "--", "true"}) }) })
			want := "\n✗ Could not load Coop settings\n\n      " + path + tc.line + "\n      " + tc.problem + "\n\n  " + tc.action + "\n"
			if code != 1 || stdout != "" || stderr != want {
				t.Errorf("coop run = %d, stdout %q\nstderr:\n%s\nwant:\n%s", code, stdout, stderr, want)
			}
		})
	}
}

// The version answers even when the settings are broken: which build is installed is the first thing
// to know about a broken setting, and the version reads none.
func TestVersionPrintsWithBrokenSettings(t *testing.T) {
	conf := filepath.Join(t.TempDir(), "coop.conf")
	if err := os.WriteFile(conf, []byte("COOP_PIDS=512\nthis is not valid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COOP_CONF", conf)
	for _, argv := range [][]string{{"version"}, {"-v"}, {"--version"}} {
		var code int
		var stdout string
		stderr := captureStderr(t, func() { stdout = captureStdout(t, func() { code = Main(argv) }) })
		if code != 0 || stdout != "coop "+resolveVersion()+"\n" || stderr != "" {
			t.Errorf("coop %s = %d, stdout %q, stderr %q; want the version and exit 0", argv[0], code, stdout, stderr)
		}
	}
	var code int
	captureStderr(t, func() { code = Main([]string{"version", "extra"}) })
	if code != 2 {
		t.Errorf("coop version extra = %d, want 2: a broken file does not loosen its arguments", code)
	}
	captureStdout(t, func() { code = Main([]string{"version", "--help"}) })
	if code != 0 {
		t.Errorf("coop version --help = %d, want 0", code)
	}
}

// A relative COOP_CONFIG_DIR would put sign-ins and the MCP file inside the working folder, usually
// the repository a box mounts. Every command refuses it before it writes a file or asks a question:
// init wrote rel2/mcp.json into the project, and login asked for an API key before failing.
func TestMainRefusesRelativeConfigDirBeforeWritingOrAsking(t *testing.T) {
	useEmptyMainConfig(t)
	hermeticGit(t)
	work := t.TempDir()
	gitInit(t, work)
	t.Chdir(work)
	t.Setenv("COOP_NO_UPDATE_CHECK", "1")
	marker := filepath.Join(t.TempDir(), "runtime-called")
	runtime := filepath.Join(t.TempDir(), "runtime")
	if err := os.WriteFile(runtime, []byte("#!/bin/sh\necho \"$@\" >> \""+marker+"\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COOP_RUNTIME", runtime)
	stdin, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	oldStdin := os.Stdin
	os.Stdin = stdin
	t.Cleanup(func() { os.Stdin = oldStdin })

	t.Setenv("COOP_CONFIG_DIR", "rel2")
	for _, argv := range [][]string{{"init"}, {"credentials"}, {"login", "gemini@rel"}} {
		var code int
		var stdout string
		stderr := captureStderr(t, func() { stdout = captureStdout(t, func() { code = Main(argv) }) })
		if code != 1 || stdout != "" || stderr != "✗ COOP_CONFIG_DIR must be an absolute path, not \"rel2\"\n" {
			t.Errorf("coop %s = %d, stdout %q, stderr %q; want exit 1 and only the refusal", strings.Join(argv, " "), code, stdout, stderr)
		}
		if entries, _ := os.ReadDir(work); len(entries) != 1 || entries[0].Name() != ".git" {
			t.Fatalf("coop %s changed the working folder: %v, want only .git", strings.Join(argv, " "), entries)
		}
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("coop %s ran the runtime (stat err %v)", strings.Join(argv, " "), err)
		}
	}

	t.Setenv("COOP_CONFIG_DIR", t.TempDir())
	stdout := captureStdout(t, func() {
		if code := Main([]string{"credentials"}); code != 0 {
			t.Errorf("coop credentials with an absolute COOP_CONFIG_DIR exit = %d, want 0", code)
		}
	})
	if !strings.Contains(stdout, "Accounts available to Coop") {
		t.Errorf("coop credentials with an absolute COOP_CONFIG_DIR printed %q", stdout)
	}
}

func TestRejectArgs(t *testing.T) {
	if err := rejectArgs("doctor", nil); err != nil {
		t.Errorf("no args should be ok, got %v", err)
	}
	// It names the first extra token, the command it was given to, and that command's own page; a
	// token that starts with "-" is an option the command does not have.
	for args, want := range map[string]string{
		"help --bogus": `Unexpected argument "help" for "coop doctor" — Usage: coop doctor — Help: coop help doctor`,
		"--bogus help": `Unknown option "--bogus" for "coop doctor" — See available options: coop help doctor`,
	} {
		if err := rejectArgs("doctor", strings.Fields(args)); err == nil || err.Error() != want {
			t.Errorf("rejectArgs(doctor, %s) = %v, want %q", args, err, want)
		}
	}
}

// The stdout "views" must gate color on stdout (ui.For(os.Stdout)), not on stderr (the package-level
// ui.Bold/ui.Dim), so `coop credentials | grep` / `coop help | cat` from an interactive shell get clean
// text. In `go test` both streams are non-tty, so this locks the non-tty-clean invariant; it can't
// reproduce the stderr-tty/stdout-pipe split without a pty (that repro is in the task log). fork ls
// needs a live fork to print its header, so it's covered by review — its header uses the same one-liner.
func TestStdoutViewsNoANSI(t *testing.T) {
	if s := helpText(&config.Config{}); strings.ContainsRune(s, '\x1b') {
		t.Errorf("helpText leaked ESC:\n%q", s)
	}
	capture := func(fn func()) string {
		old := os.Stdout
		r, w, _ := os.Pipe()
		os.Stdout = w
		fn()
		_ = w.Close()
		os.Stdout = old
		out, _ := io.ReadAll(r)
		return string(out)
	}
	a := modelsApp(t)
	views := map[string]func(){
		"commandHelp": func() { printCommandHelp(commandHelp["tasks"]) },
		"models":      func() { _, _ = a.cmdModels(nil) },
		"profiles":    func() { _, _ = a.cmdCredentials(nil) },
	}
	for name, fn := range views {
		if out := capture(fn); strings.ContainsRune(out, '\x1b') {
			t.Errorf("%s leaked ESC into piped stdout:\n%q", name, out)
		}
	}
}

func TestMainCommandHelpArg(t *testing.T) {
	useEmptyMainConfig(t)
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	code := Main([]string{"build", "help"})
	_ = w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	if code != 0 {
		t.Errorf("`coop build help` exit = %d, want 0", code)
	}
	if s := string(out); !strings.Contains(s, "coop build: build the Coop box image") || !strings.Contains(s, "Usage:\n  coop build") {
		t.Errorf("`coop build help` should print build's help; got:\n%s", s)
	}
}

// coop run reads -h and --help as a request for its page only before its command; from the command
// on they are the command's. Other commands take either anywhere before --.
func TestHelpAskedStopsAtRunsCommand(t *testing.T) {
	for line, want := range map[string]bool{
		"run --help":               true,
		"run -h":                   true,
		"run --egress open --help": true,
		"run help":                 true,
		"run ls --help":            false,
		"run --egress open ls -h":  false,
		"run -- --help":            false,
		"claude --continue --help": true,
		"claude -- --help":         false,
		"build help":               true,
	} {
		if got := helpAsked(strings.Fields(line)); got != want {
			t.Errorf("helpAsked(coop %s) = %v, want %v", line, got, want)
		}
	}
}

// TestMainBarePrintsHelp verifies bare `coop` prints help and exits 0 without a
// container runtime (it returns before runtime detection) — so a stray `coop`
// never launches an agent; running one is explicit (`coop claude`).
func TestMainBarePrintsHelp(t *testing.T) {
	useEmptyMainConfig(t)
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	code := Main(nil)
	_ = w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)

	if code != 0 {
		t.Errorf("bare coop exit = %d, want 0", code)
	}
	if s := string(out); !strings.Contains(s, "Usage") || !strings.Contains(s, "coop <target>") {
		t.Errorf("bare coop should print help listing `coop <target>`; got:\n%s", s)
	}
}

// `coop help <cmd> [<sub>]` shows that command's page (≡ `coop <cmd> [<sub>] --help`), and
// `coop help <unknown>` is a usage error (exit 2), never the top-level menu.
func TestMainHelpSubcommand(t *testing.T) {
	useEmptyMainConfig(t)
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	codeBuild := Main([]string{"help", "build"}) // == coop build --help, no runtime needed
	cfg := &config.Config{}
	codeFork, _ := helpForPath([]string{"fork"}, cfg, true)          // the fork family help
	codeClaude, _ := helpForPath([]string{"claude"}, cfg, true)      // coop's own page for the agent
	codeLeaf, _ := helpForPath([]string{"tasks", "add"}, cfg, true)  // a real leaf resolves
	codeBogus, bogusErr := helpForPath([]string{"bogus"}, cfg, true) // unknown → usage error
	_ = w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)

	if codeBuild != 0 || !strings.Contains(string(out), "Usage:\n  coop build") {
		t.Errorf("`coop help build` = %d; want 0 + build's help, got:\n%s", codeBuild, out)
	}
	if codeFork != 0 {
		t.Errorf("helpForPath(fork) = %d, want 0", codeFork)
	}
	if codeClaude != 0 || !strings.Contains(string(out), "coop claude: run Claude") {
		t.Errorf("helpForPath(claude) = %d; want 0 + coop's own Claude page, got:\n%s", codeClaude, out)
	}
	if codeLeaf != 0 {
		t.Errorf("helpForPath(tasks add) = %d, want 0", codeLeaf)
	}
	if codeBogus != 2 || bogusErr == nil {
		t.Errorf("helpForPath(bogus) = (%d, %v), want (2, unknown-command error)", codeBogus, bogusErr)
	}
}

// `coop version` takes no arguments — extras are a usage error (exit 2), like every other no-arg
// command, not silently ignored.
func TestVersionRejectsExtraArgs(t *testing.T) {
	useEmptyMainConfig(t)
	oo, oe := os.Stdout, os.Stderr
	_, w, _ := os.Pipe()
	os.Stdout, os.Stderr = w, w
	codeExtra := Main([]string{"version", "foo"})
	codeOK := Main([]string{"version"})
	_ = w.Close()
	os.Stdout, os.Stderr = oo, oe
	if codeExtra != 2 {
		t.Errorf("Main(version foo) = %d, want 2 (usage error)", codeExtra)
	}
	if codeOK != 0 {
		t.Errorf("Main(version) = %d, want 0", codeOK)
	}
}

// `coop help help` / `coop help version` must not print a broken "forwards --help — run 'coop help
// --help'" pointer (neither has an underlying CLI, and `coop help --help` errors). help prints the
// top-level reference; version a synopsis.
func TestHelpForHelpAndVersion(t *testing.T) {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	cfg := &config.Config{}
	codeHelp, _ := helpForPath([]string{"help"}, cfg, true)
	codeVer, _ := helpForPath([]string{"version"}, cfg, true)
	_ = w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	if codeHelp != 0 || codeVer != 0 {
		t.Errorf("helpForPath help=%d version=%d, want 0/0", codeHelp, codeVer)
	}
	if s := string(out); strings.Contains(s, "forwards --help") {
		t.Errorf("help/version must not print the broken passthrough pointer:\n%s", s)
	}
	if s := string(out); !strings.Contains(s, "Usage") {
		t.Errorf("`coop help help` should print the top-level reference:\n%s", s)
	}
}

// Pure-local families work with NO container runtime; only box-running commands surface the runtime
// error. Detect is lazy (a.ensureRuntime), not eager in Main — so install→init→browse the queue and
// CI `coop tasks lint` don't require Docker.
func TestRuntimeDetectIsLazy(t *testing.T) {
	// No codex either: `coop models` refreshes a due catalog, and Codex's comes from its CLI.
	t.Setenv("PATH", t.TempDir())
	bogus := func() *app {
		return &app{cfg: &config.Config{RuntimeName: "coop-no-such-runtime-xyz", ConfigDir: t.TempDir()}}
	}
	if err := bogus().ensureRuntime(); err == nil {
		t.Fatal("ensureRuntime with a bogus runtime should error")
	}
	// A box-running command (dispatched) hits the runtime error up front...
	if code, err := bogus().dispatch([]string{"build"}); err == nil || code == 0 {
		t.Errorf("coop build with no runtime should fail, got (%d, %v)", code, err)
	}
	// ...but a pure-local one never detects. Capture stdout so the listing stays quiet.
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	code, err := bogus().dispatch([]string{"models"})
	_ = w.Close()
	os.Stdout = old
	_, _ = io.ReadAll(r)
	if code != 0 || err != nil {
		t.Errorf("coop models with no runtime should succeed (pure-local), got (%d, %v)", code, err)
	}
}

// The exit-code contract is coop's machine interface (CI/scripts branch on it): 0 success · 1 failure
// or findings · 2 usage. Pin representative cases so it can't drift silently (documented in docs/configuration.md).
func TestExitCodeContract(t *testing.T) {
	a := modelsApp(t)
	a.cfg.RepoOverride = t.TempDir()
	// 2 — usage: an unknown command, and bad arguments.
	if code, _ := a.dispatch([]string{"nonesuch-xyz"}); code != 2 {
		t.Errorf("unknown command exit = %d, want 2", code)
	}
	if code, _ := a.dispatch([]string{"fork", "rm", "a", "b"}); code != 2 {
		t.Errorf("bad-arguments exit = %d, want 2", code)
	}
	// A name that refers to nothing is rejected input too, as an unknown fork or preset is.
	if code, _ := cmdTasksFolder("", t.TempDir(), []string{"rm", "no-such-task", "--yes"}); code != 2 {
		t.Errorf("no-such-task exit = %d, want 2", code)
	}
	// 1 — failure: the input was fine, but the command could not do its job (the task is done).
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, stateDone, "2026-01-01-shipped", "task.md"), "# Shipped\n")
	if code, _ := cmdTasksFolder("", root, []string{"claim", "shipped"}); code != 1 {
		t.Errorf("failure (claiming a done task) exit = %d, want 1", code)
	}
	// 0 — success: a pure-local listing (stdout discarded).
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	code, _ := a.dispatch([]string{"models"})
	_ = w.Close()
	os.Stdout = old
	_, _ = io.ReadAll(r)
	if code != 0 {
		t.Errorf("ok command exit = %d, want 0", code)
	}
}

// A final review that cannot find a task's one commit refuses in the shared failure block: the count
// it found, where it looked, and how to repair the history, never a bare error line.
func TestPendingReviewCommitsRefusalIsAFailureBlock(t *testing.T) {
	base := strings.Repeat("ab", 20)
	for _, tc := range []struct {
		found   int
		commits []string
		cause   string
		remedy  string
	}{
		{0, nil, "found 0", "Restore one commit with that trailer, then run the loop again."},
		{2, []string{"1a2b3c4 by Worker <w@x>", "5d6e7f8 by Andrew <a@x>"}, "found 2: 1a2b3c4 by Worker <w@x>, 5d6e7f8 by Andrew <a@x>",
			"Drop the extra commit or squash them, then run the loop again."},
	} {
		err := fmt.Errorf("recover pending final review: %w", &tasks.PendingReviewCommitsError{TaskID: "fix-login", Base: base, Found: tc.found, Commits: tc.commits})
		got := usageBlock(t, err)
		want := "\n✗ Final review cannot continue\n\n" +
			"      Task fix-login needs exactly one commit with \"Coop-Task: fix-login\" after " + base[:12] +
			", where this run started; " + tc.cause + ".\n\n" +
			"  " + tc.remedy + "\n"
		if got != want {
			t.Errorf("found %d:\n--- got ---\n%s\n--- want ---\n%s", tc.found, got, want)
		}
		if asUsage(ui.Reported(err)) != nil {
			t.Error("a refusal the loop already drew would be drawn again")
		}
	}
}

// A --review-task import of a task Coop never recorded as complete is a refusal with a headline,
// the cause in plain words and the command that records it, not a bare internal error line.
func TestUnrecordedCompletionRefusalIsAFailureBlock(t *testing.T) {
	err := fmt.Errorf("import archived task for final review: %w", &tasks.UnrecordedCompletionError{TaskID: "fix-login"})
	want := "\n✗ Final review cannot continue\n\n" +
		"      Task fix-login is done, but Coop has no record of its completion.\n\n" +
		"  Record it with coop tasks done fix-login, then run the loop again.\n"
	if got := usageBlock(t, err); got != want {
		t.Errorf("--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if usage := asUsage(err); usage == nil || usage.ExitCode != 1 {
		t.Errorf("refusal = %+v, want a failure that exits 1", usage)
	}
}

// `coop help claude` prints the page generated from Claude's adapter, to the byte, and no generic
// all-commands footer: the page ends with its own pointer. docs/cli.md pins the page's bytes
// (make docs-check); this pins the help ROUTE that prints it.
func TestHelpForAgentPrintsItsPage(t *testing.T) {
	var code int
	out := captureStdout(t, func() { code, _ = helpForPath([]string{"claude"}, &config.Config{}, true) })
	if code != 0 {
		t.Fatalf("helpForPath(claude) = %d, want 0", code)
	}
	if want := agentHelp("claude") + "\n"; out != want {
		t.Errorf("coop help claude printed:\n%s\nwant the generated page:\n%s", out, want)
	}
}

// Every other agent gets the SAME page generated from its own adapter: its command, its human
// name, its argument label — and an effort in a page's example must be one that agent accepts.
func TestHelpForAgentIsGeneratedPerAdapter(t *testing.T) {
	codex := agentHelp("codex")
	for _, want := range []string{
		"coop codex: run Codex in the box",
		"coop codex[:<model>][/<effort>][@<account>] [<options>] [-- <codex-args>...]",
		"coop codex:gpt-6-astra",
		"pass all remaining arguments directly to Codex",
		"coop models codex       list Codex models",
	} {
		if !strings.Contains(codex, want) {
			t.Errorf("codex page missing %q:\n%s", want, codex)
		}
	}
	gemini := agentHelp("gemini")
	if !strings.Contains(gemini, "coop gemini[:<model>][/<effort>][@<account>]") {
		t.Errorf("gemini takes an effort, so its usage must show one:\n%s", gemini)
	}
	geminiAgent, _ := agents.Get("gemini")
	example := "coop gemini:" + geminiAgent.ExampleModel() + "/high@work"
	if !strings.Contains(gemini, example) {
		t.Errorf("gemini page lacks its effort example %q:\n%s", example, gemini)
	}
	if target, err := agents.ParseTarget(strings.TrimPrefix(example, "coop ")); err != nil {
		t.Errorf("gemini page's example %q is refused: %v (%v)", example, err, target)
	}
	// The rows still line up on the widest cell, whatever the agent's name length.
	for _, name := range agents.Names() {
		for _, line := range strings.Split(agentHelp(name), "\n") {
			if strings.HasPrefix(line, "  coop models ") && !strings.Contains(line, "   list ") {
				t.Errorf("%s: models row lost its column gap: %q", name, line)
			}
		}
		// -p is the one spelling of a one-prompt run, so every page shows it the same way.
		for _, want := range []string{`  -p "<prompt>"          run one prompt and exit`, "  coop " + name + ` -p "fix the failing test"`} {
			if !strings.Contains(agentHelp(name), want) {
				t.Errorf("%s page missing %q:\n%s", name, want, agentHelp(name))
			}
		}
	}
}

// `coop fork --help` must match every other command's template: a Usage line under the title and
// the standard footer.
func TestForkHelpTemplate(t *testing.T) {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	code, _ := forkHelp("")
	_ = w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	s := string(out)
	if code != 0 {
		t.Errorf("forkHelp exit = %d, want 0", code)
	}
	if !strings.Contains(s, "Usage: coop fork <name> [<target|preset>] [<options>]") {
		t.Errorf("fork help missing a Usage line:\n%s", s)
	}
	// The page ends with its own next step, so it never gets the generic all-commands footer.
	if !strings.HasSuffix(strings.TrimRight(s, "\n"), "Command options:          coop help fork <command>") {
		t.Errorf("fork help should end with its own pointer:\n%s", s)
	}
}
