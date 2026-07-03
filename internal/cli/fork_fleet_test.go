package cli

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
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

// The legend line and the ## Example block both contain "[ ]"/"[E]" but aren't real
// tasks; fleet split must not turn them into phantom slice entries.
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
	for _, want := range []string{"forks:", "tasks:", "credentials:", "preset:", "coop fleet up"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("fleet template missing %q:\n%s", want, body)
		}
	}
	// The template's forks map is empty, so it parses to an empty fleet (nothing to start yet).
	if entries, err := parseFleetYAML(string(body)); err != nil || len(entries) != 0 {
		t.Errorf("template should parse to 0 forks, got %d (%v)", len(entries), err)
	}
	// Re-init refuses to clobber.
	if code, err := a.fleetInit(); err == nil || code == 0 {
		t.Errorf("re-init should refuse to clobber, got (%d, %v)", code, err)
	}
	// A repo with only a LEGACY .agent/fleet refuses too — init must not create the ambiguity.
	legacy := filepath.Join(t.TempDir(), "l")
	if err := os.MkdirAll(filepath.Join(legacy, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, ".agent", "fleet"), []byte("a claude t\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a2 := &app{cfg: &config.Config{RepoOverride: legacy}}
	if code, err := a2.fleetInit(); err == nil || code == 0 || !strings.Contains(err.Error(), "pre-v3") {
		t.Errorf("init over a pre-v3 fleet should refuse and name the migration, got (%d, %v)", code, err)
	}
}

// .agent/fleet.yaml is the primary fleet format: author order is preserved, per-fork
// preset/credentials/model/consult parse, agent defaults to the preset's lead (empty
// here) or the default agent, and every invalid shape errors with the fork named.
func TestParseFleetYAML(t *testing.T) {
	got, err := parseFleetYAML(`
forks:
  core:
    tasks: .agent/tasks.core
    preset: frontier
    credentials: [work]
  chores:
    agent: gemini
    tasks: .agent/tasks.chores
    model: gemini-3.5-flash
    credentials: [work, backup]
    consult: false
  plain:
    tasks: .agent/tasks.plain
    consult: true
`)
	if err != nil {
		t.Fatal(err)
	}
	want := []fleetEntry{
		{name: "core", agent: "", tasks: ".agent/tasks.core", preset: "frontier", profiles: []string{"work"}},
		{name: "chores", agent: "gemini", tasks: ".agent/tasks.chores", model: "gemini-3.5-flash", profiles: []string{"work", "backup"}},
		{name: "plain", agent: "claude", tasks: ".agent/tasks.plain", consult: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseFleetYAML =\n%+v\nwant\n%+v", got, want)
	}

	for name, in := range map[string]string{
		"malformed":     "forks: [\n",
		"no forks map":  "other: {}\n",
		"missing tasks": "forks:\n  a: {agent: claude}\n",
		"unknown agent": "forks:\n  a: {agent: borg, tasks: t}\n",
		"unknown field": "forks:\n  a: {tasks: t, profile: work}\n", // YAML uses credentials:, not the legacy profile=
		"bad name":      "forks:\n  ? \"a b\"\n  : {tasks: t}\n",
		"duplicate":     "forks:\n  a: {tasks: t}\n  a: {tasks: u}\n",
		"empty cred":    "forks:\n  a: {tasks: t, credentials: [\"\"]}\n",
	} {
		if _, err := parseFleetYAML(in); err == nil {
			t.Errorf("%s: want an error, got none", name)
		}
	}
}

// A fleet fork's credentials: list takes plain names, compact targets, and the
// structured {name, model} form — all normalizing to the pool's credential[@model]
// members, so a fleet can declare a same-account model-fallback chain.
func TestParseFleetYAMLCredentialTargets(t *testing.T) {
	got, err := parseFleetYAML(`
forks:
  core:
    tasks: t
    credentials:
      - work
      - work@opus
      - {name: work, model: sonnet}
`)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"work", "work@opus", "work@sonnet"}; !reflect.DeepEqual(got[0].profiles, want) {
		t.Errorf("credentials = %v, want %v", got[0].profiles, want)
	}
	for name, in := range map[string]string{
		"unknown target key": "forks:\n  a: {tasks: t, credentials: [{name: w, mode: opus}]}\n",
		"empty name":         "forks:\n  a: {tasks: t, credentials: [{model: opus}]}\n",
		"list-form target":   "forks:\n  a: {tasks: t, credentials: [[w]]}\n",
	} {
		if _, err := parseFleetYAML(in); err == nil {
			t.Errorf("%s: want an error, got none", name)
		}
	}
}

// The pre-v3 one-line .agent/fleet is NEVER read: alone or alongside fleet.yaml, its
// presence errors with the migrate-and-delete pointer; fleet.yaml alone loads.
func TestLoadFleetRejectsPreV3File(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{RepoOverride: repo}}
	if _, err := a.loadFleet(repo); err == nil || !strings.Contains(err.Error(), "coop fleet init") {
		t.Errorf("no fleet: want the init pointer, got %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "fleet"), []byte("a claude t\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := a.loadFleet(repo); err == nil || !strings.Contains(err.Error(), "no longer read") {
		t.Errorf("pre-v3 file alone: want the migrate-and-delete error, got %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "fleet.yaml"), []byte("forks:\n  b: {tasks: t}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := a.loadFleet(repo); err == nil || !strings.Contains(err.Error(), "no longer read") {
		t.Errorf("pre-v3 file alongside yaml: want the migrate-and-delete error, got %v", err)
	}
	if err := os.Remove(filepath.Join(repo, ".agent", "fleet")); err != nil {
		t.Fatal(err)
	}
	if entries, err := a.loadFleet(repo); err != nil || len(entries) != 1 || entries[0].name != "b" {
		t.Errorf("yaml alone should load: %v (%+v)", err, entries)
	}
}

func TestFleetSplit(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "r")
	for _, id := range []string{"2026-01-01-a", "2026-01-02-b", "2026-01-03-c"} {
		writeTaskFile(t, filepath.Join(repo, ".agent", "tasks", stateTodo, id, "task.md"), "# "+id+"\n")
	}
	a := &app{cfg: &config.Config{RepoOverride: repo}}
	if code, err := a.fleetSplit([]string{"2"}); err != nil || code != 0 {
		t.Fatalf("fleetSplit = (%d, %v), want (0, nil)", code, err)
	}
	// Round-robin over the sorted todo list: slice1 gets a + c, slice2 gets b — as folder copies.
	if !isTaskDir(filepath.Join(repo, ".agent", "tasks.slice1", stateTodo, "2026-01-01-a")) ||
		!isTaskDir(filepath.Join(repo, ".agent", "tasks.slice1", stateTodo, "2026-01-03-c")) {
		t.Error("slice1 should hold a and c")
	}
	if !isTaskDir(filepath.Join(repo, ".agent", "tasks.slice2", stateTodo, "2026-01-02-b")) {
		t.Error("slice2 should hold b")
	}
	// It also writes .agent/fleet.yaml with each fork's explicit tasks dir.
	fleet, _ := os.ReadFile(filepath.Join(repo, ".agent", "fleet.yaml"))
	if !strings.Contains(string(fleet), "slice1:") || !strings.Contains(string(fleet), "tasks: .agent/tasks.slice1") {
		t.Errorf(".agent/fleet.yaml = %q, want an explicit slice1 entry", fleet)
	}
	parsed, err := parseFleetYAML(string(fleet))
	if err != nil || len(parsed) != 2 {
		t.Errorf("written .agent/fleet.yaml does not parse back: %v (%d entries)", err, len(parsed))
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
