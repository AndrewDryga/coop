package cli

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

// `coop fleet ls`/`list` has no fleet-level listing — it must point at the real views (fork ls / the
// live board), not error blankly (rule: `ls` is the list verb, it must lead somewhere useful).
func TestFleetLsRedirect(t *testing.T) {
	a := &app{cfg: &config.Config{}}
	code, err := a.cmdFleet([]string{"ls"}) // v3: only `ls` redirects; `list` is a plain unknown verb
	if code != 2 || err == nil || !strings.Contains(err.Error(), "coop fork ls") {
		t.Errorf("cmdFleet([ls]) = (%d, %v), want (2, pointing at `coop fork ls`)", code, err)
	}
}

// fleet up fails fast, but when forks already started the error must be loud about the partial fleet
// and name the cleanup, so a half-started fleet isn't discovered later.
func TestFleetAbortErr(t *testing.T) {
	none := fleetAbortErr("api", errors.New("boom"), 0).Error()
	if !strings.Contains(none, "api") || !strings.Contains(none, "boom") {
		t.Errorf("abort err (none started) should name the fork and cause: %q", none)
	}
	if strings.Contains(none, "fleet down") {
		t.Errorf("abort err with nothing started shouldn't mention cleanup: %q", none)
	}
	some := fleetAbortErr("web", errors.New("boom"), 2).Error()
	for _, want := range []string{"web", "2 fork", "coop fleet down"} {
		if !strings.Contains(some, want) {
			t.Errorf("abort err (2 started) missing %q: %q", want, some)
		}
	}
}

func TestPolicyScan(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	ws, err := setupFork(repo, "x")
	if err != nil {
		t.Fatalf("setupFork: %v", err)
	}
	// .env (classic) plus files the old hand-rolled regex MISSED but SecretGlobs covers — the gate
	// now shares the shadow decider, so these must be flagged. safe.txt must not be.
	for name, body := range map[string]string{
		".env": "SECRET=1\n", "kubeconfig": "apiVersion: v1\n", ".npmrc": "//r/:_authToken=x\n",
		"service_account.json": "{}\n", "safe.txt": "ok\n",
	} {
		if err := os.WriteFile(filepath.Join(ws, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", "add files")
	if err := gitFetchInto(repo, ws, "x"); err != nil {
		t.Fatal(err)
	}
	warns := strings.Join(policyScan(repo, "review/x"), "\n")
	for _, want := range []string{".env", "kubeconfig", ".npmrc", "service_account.json"} {
		if !strings.Contains(warns, want) {
			t.Errorf("policyScan missed credential file %q: %q", want, warns)
		}
	}
	if strings.Contains(warns, "safe.txt") {
		t.Errorf("policyScan wrongly flagged safe.txt: %q", warns)
	}
}

// A real token in an ordinary (non-secret-named) file passes a filename check but must
// be caught by policyScan's content scan.
func TestPolicyScanContent(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	ws, err := setupFork(repo, "leak")
	if err != nil {
		t.Fatalf("setupFork: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(ws, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "config", "prod.yml"),
		[]byte("host: db\ntoken: ghp_abcdefghijklmnopqrstuvwxyz0123456789\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", "config")
	if err := gitFetchInto(repo, ws, "leak"); err != nil {
		t.Fatal(err)
	}
	warns := strings.Join(policyScan(repo, "review/leak"), "\n")
	if !strings.Contains(warns, "config/prod.yml") || !strings.Contains(warns, "GitHub token") {
		t.Errorf("policyScan missed the token in config/prod.yml: %q", warns)
	}
}

func TestFleetInit(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "r")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{RepoOverride: repo}}
	if code, err := a.fleetInit(); err != nil || code != 0 {
		t.Fatalf("fleetInit = (%d, %v), want (0, nil)", code, err)
	}
	body, err := os.ReadFile(filepath.Join(repo, ".agent", "fleet.yaml"))
	if err != nil {
		t.Fatalf(".agent/fleet.yaml not written: %v", err)
	}
	for _, want := range []string{"forks:", "tasks:", "agent:", "coop fleet up"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("fleet template missing %q:\n%s", want, body)
		}
	}
	// preset: was dropped — the who-runs lives in agent: now, so the template must not resurrect it.
	if strings.Contains(string(body), "preset:") {
		t.Errorf("fleet template should no longer show a preset: key (agent: absorbs presets):\n%s", body)
	}
	// The template's forks map is empty, so it parses to an empty fleet (nothing to start yet).
	if entries, err := parseFleetYAML(string(body)); err != nil || len(entries) != 0 {
		t.Errorf("template should parse to 0 forks, got %d (%v)", len(entries), err)
	}
	// Re-init refuses to clobber.
	if code, err := a.fleetInit(); err == nil || code == 0 {
		t.Errorf("re-init should refuse to clobber, got (%d, %v)", code, err)
	}
}

// .agent/fleet.yaml is the primary fleet format: author order is preserved, agent: is the
// who-runs — a target (provider[:model][@account]) OR a preset name (classified into the entry's
// preset field), no implicit default — and every invalid shape errors with the fork named.
func TestParseFleetYAML(t *testing.T) {
	got, err := parseFleetYAML(`
forks:
  core:
    tasks: .agent/tasks.core
    agent: frontier
  chores:
    agent: gemini:gemini-3.5-flash@work
    tasks: .agent/tasks.chores
  plain:
    agent: claude
    tasks: .agent/tasks.plain
`)
	if err != nil {
		t.Fatal(err)
	}
	want := []fleetEntry{
		// agent: frontier is a preset name (not a target) → classified into preset, agent cleared.
		{name: "core", agent: "", tasks: ".agent/tasks.core", preset: "frontier"},
		{name: "chores", agent: "gemini:gemini-3.5-flash@work", tasks: ".agent/tasks.chores"},
		{name: "plain", agent: "claude", tasks: ".agent/tasks.plain"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseFleetYAML =\n%+v\nwant\n%+v", got, want)
	}

	for name, in := range map[string]string{
		"malformed":           "forks: [\n",
		"no forks map":        "other: {}\n",
		"missing tasks":       "forks:\n  a: {agent: claude}\n",
		"no who":              "forks:\n  a: {tasks: t}\n",                                  // agent: is required (a target or a preset name)
		"malformed target":    "forks:\n  a: {agent: \"claude:\", tasks: t}\n",              // a known provider with an empty :model
		"unknown model key":   "forks:\n  a: {agent: claude, tasks: t, model: opus}\n",      // the model rides agent:
		"unknown cred key":    "forks:\n  a: {agent: claude, tasks: t, credential: work}\n", // the account rides agent:
		"account ladder":      "forks:\n  a: {agent: \"claude@work,personal\", tasks: t}\n", // a fork takes one account
		"unknown field":       "forks:\n  a: {tasks: t, sidekick: yes}\n",
		"unknown profile key": "forks:\n  a: {tasks: t, profile: work}\n",
		"preset key dropped":  "forks:\n  a: {tasks: t, preset: frontier}\n",             // preset: is retired — agent: absorbs it
		"consult not a key":   "forks:\n  a: {tasks: t, agent: claude, consult: true}\n", // consult was dropped
		"bad name":            "forks:\n  ? \"a b\"\n  : {tasks: t}\n",
		"duplicate":           "forks:\n  a: {tasks: t}\n  a: {tasks: u}\n",
	} {
		if _, err := parseFleetYAML(in); err == nil {
			t.Errorf("%s: want an error, got none", name)
		}
	}
}

// composeTarget rebuilds a target from the pieces a fork parsed out of one (detachForkLoop's
// re-exec): :model and @account fold in, model's own @account is honored, and a contradictory
// pair (model @a + credential b) errors.
func TestComposeTarget(t *testing.T) {
	cases := []struct {
		agent, model, cred, want string
		wantErr                  bool
	}{
		{"claude", "", "", "claude", false},
		{"claude", "opus-4.8", "", "claude:opus-4.8", false},
		{"claude", "", "work", "claude@work", false},
		{"claude", "opus-4.8", "work", "claude:opus-4.8@work", false},
		{"gemini", "gemini-3.5-flash@work", "", "gemini:gemini-3.5-flash@work", false},     // model carries the account
		{"gemini", "gemini-3.5-flash@work", "work", "gemini:gemini-3.5-flash@work", false}, // same account, no conflict
		{"gemini", "gemini-3.5-flash@work", "personal", "", true},                          // two different accounts → error
	}
	for _, c := range cases {
		got, err := composeTarget(c.agent, c.model, "", c.cred)
		if c.wantErr {
			if err == nil {
				t.Errorf("composeTarget(%q,%q,%q) = %q, want error", c.agent, c.model, c.cred, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("composeTarget(%q,%q,%q) = (%q, %v), want %q", c.agent, c.model, c.cred, got, err, c.want)
		}
	}
}

// Only .agent/fleet.yaml is read; a stray one-line .agent/fleet file is simply ignored.
func TestLoadFleetIgnoresStrayFile(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{RepoOverride: repo}}
	if _, err := a.loadFleet(repo); err == nil || !strings.Contains(err.Error(), "coop fleet init") {
		t.Errorf("no fleet: want the init pointer, got %v", err)
	}
	// A stray .agent/fleet is not a fleet — loading still points at init, not the file.
	if err := os.WriteFile(filepath.Join(repo, ".agent", "fleet"), []byte("a claude t\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := a.loadFleet(repo); err == nil || !strings.Contains(err.Error(), "coop fleet init") {
		t.Errorf("stray .agent/fleet should be ignored (init pointer), got %v", err)
	}
	// fleet.yaml loads regardless of the stray file sitting beside it.
	if err := os.WriteFile(filepath.Join(repo, ".agent", "fleet.yaml"), []byte("forks:\n  b: {agent: claude, tasks: t}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if entries, err := a.loadFleet(repo); err != nil || len(entries) != 1 || entries[0].name != "b" {
		t.Errorf("yaml should load (stray file ignored): %v (%+v)", err, entries)
	}
}

// `coop fleet down` stops listed forks but must surface (not silently leave) a running fork that
// isn't in .agent/fleet — one removed from the file, or started by hand.
func TestFleetDownWarnsRunningOrphan(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "fleet.yaml"), []byte("forks:\n  a: {agent: claude, tasks: .agent/T.md}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A running fork "b" that isn't in the fleet (its workspace exists + a live pidfile).
	if err := os.MkdirAll(forkWorkspace(repo, "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(forkStateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeForkPid(repo, "b", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{RepoOverride: repo}}
	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	_, _ = a.fleetDown(nil)
	_ = w.Close()
	os.Stderr = old
	out, _ := io.ReadAll(r)
	if !strings.Contains(string(out), "b") || !strings.Contains(string(out), "not in .agent/fleet.yaml") {
		t.Errorf("expected a warning about running orphan b:\n%s", out)
	}
}

// `fleet down` must not print success and exit zero when one listed fork cannot be stopped. It
// attempts every listed fork, then returns an actionable aggregate so automation sees the failure.
func TestFleetDownPropagatesForkStopFailure(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "fleet.yaml"), []byte("forks:\n  perf: {agent: claude, tasks: .agent/T.md}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(forkWorkspace(repo, "perf"), 0o755); err != nil {
		t.Fatal(err)
	}
	worker := exec.Command("sleep", "30")
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = worker.Process.Kill()
		_ = worker.Wait()
	})
	if err := writeForkPid(repo, "perf", worker.Process.Pid); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{RepoOverride: repo, RuntimeName: "coop-test-runtime-that-does-not-exist"}}
	code, err := a.fleetDown(nil)
	if code != 1 || err == nil {
		t.Fatalf("fleetDown = (%d, %v), want (1, error)", code, err)
	}
	for _, want := range []string{"perf", "coop fleet down", "coop fork stop perf"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("fleetDown error missing %q: %v", want, err)
		}
	}
	if err := worker.Process.Signal(syscall.Signal(0)); err != nil {
		t.Errorf("worker should remain tracked when runtime detection fails: %v", err)
	}
}

func TestParseFleetActionFlags(t *testing.T) {
	cases := []struct {
		args         []string
		prune, force bool
		err          bool
	}{
		{nil, false, false, false},
		{[]string{"--prune"}, true, false, false},
		{[]string{"--prune", "--force"}, true, true, false},
		{[]string{"--prune", "-f"}, true, true, false},
		{[]string{"--force"}, false, false, true}, // --force is meaningless without --prune
		{[]string{"--bogus"}, false, false, true}, // unknown flag
	}
	for _, c := range cases {
		prune, force, err := parseFleetActionFlags("up", c.args)
		if (err != nil) != c.err {
			t.Errorf("%v: err=%v, want err=%v", c.args, err, c.err)
			continue
		}
		if err == nil && (prune != c.prune || force != c.force) {
			t.Errorf("%v: prune=%v force=%v, want %v/%v", c.args, prune, force, c.prune, c.force)
		}
	}
}
