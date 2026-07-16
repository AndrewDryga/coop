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
	"time"

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
// who-runs — a target (provider[:model][/effort][@account]) OR a preset name (classified into the entry's
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
		args              []string
		prune, force, yes bool
		err               bool
	}{
		{nil, false, false, false, false},
		{[]string{"--prune"}, true, false, false, false},
		{[]string{"--prune", "--force", "--yes"}, true, true, true, false},
		{[]string{"--prune", "-f", "-y"}, true, true, true, false},
		{[]string{"--force"}, false, false, false, true}, // prune flags are meaningless without --prune
		{[]string{"--yes"}, false, false, false, true},
		{[]string{"--bogus"}, false, false, false, true}, // unknown flag
	}
	for _, c := range cases {
		prune, force, yes, err := parseFleetActionFlags("up", c.args)
		if (err != nil) != c.err {
			t.Errorf("%v: err=%v, want err=%v", c.args, err, c.err)
			continue
		}
		if err == nil && (prune != c.prune || force != c.force || yes != c.yes) {
			t.Errorf("%v: prune=%v force=%v yes=%v, want %v/%v/%v", c.args, prune, force, yes, c.prune, c.force, c.yes)
		}
	}
}

func TestFleetPruneConfirmationFailsBeforeUpOrDownWork(t *testing.T) {
	a := &app{cfg: &config.Config{RepoOverride: t.TempDir()}}
	for _, tc := range []struct {
		name string
		fn   func([]string) (int, error)
	}{{"up", a.fleetUp}, {"down", a.fleetDown}} {
		t.Run(tc.name, func(t *testing.T) {
			code, err := tc.fn([]string{"--prune"})
			if code != 2 || err == nil || !strings.Contains(err.Error(), "--yes") {
				t.Fatalf("fleet %s --prune = (%d, %v), want confirmation before fleet/config work", tc.name, code, err)
			}
		})
	}
}

func TestFleetPruneSeparatesConfirmationFromForce(t *testing.T) {
	repo := initRepo(t)
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fleetYAMLFile(repo), []byte("forks: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{RepoOverride: repo}}

	clean, err := setupFork(repo, "clean")
	if err != nil {
		t.Fatal(err)
	}
	if code, err := a.fleetPrune(nil); code != 2 || err == nil || !strings.Contains(err.Error(), "--yes") || !pathExists(clean) {
		t.Fatalf("unconfirmed clean prune = (%d, %v), exists %v; want confirmation refusal", code, err, pathExists(clean))
	}
	if code, err := a.fleetPrune([]string{"--yes"}); code != 0 || err != nil || pathExists(clean) {
		t.Fatalf("confirmed clean prune = (%d, %v), exists %v", code, err, pathExists(clean))
	}

	dirty, err := setupFork(repo, "dirty")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirty, "uncommitted.txt"), []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, err := a.fleetPrune([]string{"--yes"}); code != 0 || err != nil || !pathExists(dirty) {
		t.Fatalf("guarded dirty prune = (%d, %v), exists %v; want kept", code, err, pathExists(dirty))
	}
	if code, err := a.fleetPrune([]string{"--force"}); code != 2 || err == nil || !strings.Contains(err.Error(), "--yes") || !pathExists(dirty) {
		t.Fatalf("forced but unconfirmed prune = (%d, %v), exists %v", code, err, pathExists(dirty))
	}
	if code, err := a.fleetPrune([]string{"--force", "--yes"}); code != 0 || err != nil || pathExists(dirty) {
		t.Fatalf("forced and confirmed prune = (%d, %v), exists %v", code, err, pathExists(dirty))
	}
}

func TestFleetPruneReportsStateOnlyOrphan(t *testing.T) {
	repo := initRepo(t)
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fleetYAMLFile(repo), []byte("forks: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(forkStateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeForkWorkerState(repo, "crashed", forkWorkerState{pending: true}); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{RepoOverride: repo}}
	out := captureStderr(t, func() {
		if code, err := a.fleetPrune([]string{"--yes"}); code != 0 || err != nil {
			t.Fatalf("fleet prune state-only orphan = (%d, %v)", code, err)
		}
	})
	if !strings.Contains(out, "kept crashed") || !strings.Contains(out, "coop fork stop crashed") {
		t.Fatalf("state-only orphan output = %q, want actionable cleanup warning", out)
	}
}

func TestFleetPruneRefusesReplacementAfterConfirmation(t *testing.T) {
	repo := initRepo(t)
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fleetYAMLFile(repo), []byte("forks: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ws, err := setupFork(repo, "replacement")
	if err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{RepoOverride: repo}}
	unlock, err := lockForkState(repo, "replacement")
	if err != nil {
		t.Fatal(err)
	}
	type pruneResult struct {
		code int
		err  error
	}
	result := make(chan pruneResult, 1)
	go func() {
		code, err := a.pruneFleet(repo, true, true)
		result <- pruneResult{code, err}
	}()
	select {
	case got := <-result:
		unlock()
		t.Fatalf("fleet prune bypassed lifecycle lock: (%d, %v)", got.code, got.err)
	case <-time.After(80 * time.Millisecond):
	}
	if err := os.RemoveAll(ws); err != nil {
		unlock()
		t.Fatal(err)
	}
	if err := os.MkdirAll(ws, 0o755); err != nil {
		unlock()
		t.Fatal(err)
	}
	marker := filepath.Join(ws, "replacement.txt")
	if err := os.WriteFile(marker, []byte("keep\n"), 0o644); err != nil {
		unlock()
		t.Fatal(err)
	}
	unlock()
	select {
	case got := <-result:
		if got.code != 0 || got.err != nil {
			t.Fatalf("fleet prune after replacement = (%d, %v), want safe kept result", got.code, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fleet prune remained blocked after lifecycle unlock")
	}
	if !pathExists(marker) {
		t.Fatal("fleet prune deleted the replacement workspace")
	}
}
