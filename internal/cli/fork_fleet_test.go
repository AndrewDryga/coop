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

func TestParseFleet(t *testing.T) {
	in := "# a fleet\n" +
		"perf codex .agent/tasks.perf\n" +
		"deps gemini .agent/tasks.deps\n" +
		"docs .agent/tasks.docs\n" + // agent omitted → claude
		"api codex .agent/tasks.api profile=work\n" + // per-fork single profile
		"web .agent/tasks.web profile=work,personal\n" + // agent omitted + per-fork pool
		"big claude .agent/tasks.big profile=work model=opus\n" + // per-fork model
		"core claude .agent/tasks.core model=fable consult=1\n" + // per-fork consult
		"solo codex .agent/tasks.solo consult=off\n\n" // explicit off parses too
	got, err := parseFleet(in)
	if err != nil {
		t.Fatalf("parseFleet: %v", err)
	}
	want := []fleetEntry{
		{"perf", "codex", ".agent/tasks.perf", nil, "", false},
		{"deps", "gemini", ".agent/tasks.deps", nil, "", false},
		{"docs", "claude", ".agent/tasks.docs", nil, "", false},
		{"api", "codex", ".agent/tasks.api", []string{"work"}, "", false},
		{"web", "claude", ".agent/tasks.web", []string{"work", "personal"}, "", false},
		{"big", "claude", ".agent/tasks.big", []string{"work"}, "opus", false},
		{"core", "claude", ".agent/tasks.core", nil, "fable", true},
		{"solo", "codex", ".agent/tasks.solo", nil, "", false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseFleet = %v, want %v", got, want)
	}
	// An unknown key=value option is rejected (only profile=/model=/consult= are known).
	if _, err := parseFleet("api codex q.md bogus=1"); err == nil {
		t.Error("parseFleet: want error for an unknown option key")
	}
	// profile= with no value is rejected, not a silent empty pool.
	if _, err := parseFleet("api codex q.md profile="); err == nil {
		t.Error("parseFleet: want error for an empty profile= value")
	}
	// model= with no value is rejected too.
	if _, err := parseFleet("api codex q.md model="); err == nil {
		t.Error("parseFleet: want error for an empty model= value")
	}
	// consult= with a non-boolean value is rejected, not silently off.
	if _, err := parseFleet("api codex q.md consult=maybe"); err == nil {
		t.Error("parseFleet: want error for a non-boolean consult= value")
	}
	if _, err := parseFleet("perf"); err == nil {
		t.Error("parseFleet: want error when the tasks path is missing")
	}
	if _, err := parseFleet("perf codex"); err == nil {
		t.Error("parseFleet: want error when only an agent is given (no tasks path)")
	}
	if _, err := parseFleet("ls codex q.md"); err == nil {
		t.Error("parseFleet: want error for reserved name")
	}
	// A misspelled middle agent must not be swallowed as the path (dropping the real path) — it's an
	// error naming the agents, not a silent "no such file: borg" later.
	if _, err := parseFleet("api borg .agent/tasks.api"); err == nil {
		t.Error("parseFleet: want error for an unknown middle agent token")
	}
	// A path with spaces is rejected, not truncated to its first word.
	if _, err := parseFleet("api codex tasks with spaces.md"); err == nil {
		t.Error("parseFleet: want error for a tasks path containing spaces")
	}
	// Duplicate fork names are rejected — two lines for one name silently dropped the second before.
	if _, err := parseFleet("api codex a.md\napi gemini b.md"); err == nil {
		t.Error("parseFleet: want error for a duplicate fork name")
	}
}

// `coop fleet ls`/`list` has no fleet-level listing — it must point at the real views (fork ls / the
// live board), not error blankly (rule: `ls` is the list verb, it must lead somewhere useful).
func TestFleetLsRedirect(t *testing.T) {
	a := &app{cfg: &config.Config{}}
	for _, sub := range []string{"ls", "list"} {
		code, err := a.cmdFleet([]string{sub})
		if code != 2 || err == nil || !strings.Contains(err.Error(), "coop fork ls") {
			t.Errorf("cmdFleet([%s]) = (%d, %v), want (2, pointing at `coop fork ls`)", sub, code, err)
		}
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
	body, err := os.ReadFile(filepath.Join(repo, ".agent", "fleet"))
	if err != nil {
		t.Fatalf(".agent/fleet not written: %v", err)
	}
	for _, want := range []string{"one fork per line", "<name> [agent] <tasks-path>", "coop fleet up"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("fleet template missing %q:\n%s", want, body)
		}
	}
	// The template is all comments, so it parses to an empty fleet (nothing to start yet).
	if entries, err := parseFleet(string(body)); err != nil || len(entries) != 0 {
		t.Errorf("template should parse to 0 forks, got %d (%v)", len(entries), err)
	}
	// Re-init refuses to clobber.
	if code, err := a.fleetInit(); err == nil || code == 0 {
		t.Errorf("re-init should refuse to clobber, got (%d, %v)", code, err)
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
	// It also writes .agent/fleet with each fork's explicit tasks dir.
	fleet, _ := os.ReadFile(filepath.Join(repo, ".agent", "fleet"))
	if !strings.Contains(string(fleet), "slice1 claude .agent/tasks.slice1") {
		t.Errorf(".agent/fleet = %q, want an explicit slice1 path line", fleet)
	}
	parsed, err := parseFleet(string(fleet))
	if err != nil || len(parsed) != 2 {
		t.Errorf("written .agent/fleet does not parse back: %v (%d entries)", err, len(parsed))
	}
}

// `coop fleet down` stops listed forks but must surface (not silently leave) a running fork that
// isn't in .agent/fleet — one removed from the file, or started by hand.
func TestFleetDownWarnsRunningOrphan(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "fleet"), []byte("a claude .agent/T.md\n"), 0o644); err != nil {
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
	if !strings.Contains(string(out), "b") || !strings.Contains(string(out), "not in .agent/fleet") {
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
