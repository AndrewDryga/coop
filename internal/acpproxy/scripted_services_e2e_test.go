package acpproxy_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/forkctl"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/processidentity"
	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

// An editor session removes its own project services. A reload removes the generation's project
// before the next generation starts a box, once the generation's parked warm box has run and been
// stopped, and the session's end removes the last generation's. Each removal selects exactly that
// generation's project by Compose's own labels, never the development project. That no removal
// happens while a child still runs is TestACPServicesWaitForEveryChild's (internal/cli) to show:
// here the parked box's stop has already begun by the time Run returns.
func TestScriptedACPRemovesItsOwnServices(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	tmp := scriptedTempDir(t)
	coopBin, fixtureBin := filepath.Join(tmp, "coop"), filepath.Join(tmp, "acpfixture")
	buildTestBinary(t, root, coopBin, ".")
	buildTestBinary(t, root, fixtureBin, "./internal/acpproxy/testdata/acpfixture")
	repo := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	reloaded := matrixGeneration("codex", "C1", repo, "after reload")
	reloaded[1] = matrixStep{Method: "session/load", Params: map[string]any{
		"cwd": repo, "mcpServers": []any{}, "sessionId": "C1",
	}, Result: map[string]any{"configOptions": []any{}}}
	// Each generation parks a Claude box the session never switches to.
	parked := []matrixStep{{Method: "initialize", Result: map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{}, "authMethods": []any{}}}}
	plan := writeMatrixPlan(t, tmp, matrixPlan{Providers: map[string][][]matrixStep{
		"codex":  {matrixGeneration("codex", "C1", repo, "first answer"), reloaded},
		"claude": {parked, parked},
	}})
	proc := startScriptedACPEnv(t, coopBin, fixtureBin, repo, tmp, plan, "codex", map[string]string{"COOP_ACP_WARM": "1"}, "codex", "claude")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := proc.client.req(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}); err != nil {
		t.Fatalf("initialize: %v\nstderr:\n%s", err, proc.stderr.String())
	}
	response, err := proc.client.req(ctx, "session/new", map[string]any{"cwd": repo, "mcpServers": []any{}})
	if err != nil {
		t.Fatalf("session/new: %v\nstderr:\n%s", err, proc.stderr.String())
	}
	sid := responseSessionID(response)
	state := filepath.Join(tmp, "fixture-state")
	promptMatrix(t, ctx, proc, filepath.Join(state, "codex-0", "wire.jsonl"), sid, "first question", "first answer")
	awaitFixtureGeneration(t, state, "claude-0") // the parked box is running its adapter
	mark := proc.client.mark()
	if err := proc.cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	awaitScriptedEventContains(t, ctx, proc, mark, `"sessionUpdate":"config_option_update"`)
	promptMatrix(t, ctx, proc, filepath.Join(state, "codex-1", "wire.jsonl"), sid, "next question", "after reload")
	awaitFixtureGeneration(t, state, "claude-1")
	proc.stop(t)

	calls := fixtureRuntimeCalls(t, state)
	generations := boxSupervisors(calls)
	if len(generations) != 2 {
		t.Fatalf("box runs name supervisors %q, want one per generation", generations)
	}
	first, second := generations[0], generations[1]
	firstDown := servicesTeardown(t, calls, repo, first)
	if next := firstBoxRun(calls, second); firstDown > next {
		t.Fatalf("the first generation's services were removed at call %d, after the next generation's first box (call %d)", firstDown, next)
	}
	if warm := slices.IndexFunc(calls, func(call []string) bool {
		return call[0] == "run" && slices.Contains(call, box.LabelSupervisor+"="+first) && slices.Contains(call, "claude-agent-acp")
	}); warm < 0 || warm > firstDown {
		t.Fatalf("the first generation ran no parked Claude box before its services went (box at call %d, removal at %d)", warm, firstDown)
	}
	if lastDown := servicesTeardown(t, calls, repo, second); lastDown < firstBoxRun(calls, second) {
		t.Fatalf("the last generation's services were removed at call %d, before its first box", lastDown)
	}
	assertNeverSelectsProject(t, calls, box.ComposeProject(repo))
}

// A fork's editor session runs its services in the fork, so that is where its end removes them,
// never in the parent repository the editor opened.
func TestScriptedForkACPRemovesItsOwnServices(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	buildDir := t.TempDir()
	coopBin, fixtureBin := filepath.Join(buildDir, "coop"), filepath.Join(buildDir, "acpfixture")
	buildTestBinary(t, root, coopBin, ".")
	buildTestBinary(t, root, fixtureBin, "./internal/acpproxy/testdata/acpfixture")
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(tmp, "repo")
	ws := forkspace.Workspace(repo, "myfork")
	for _, dir := range []string{filepath.Join(repo, ".agent"), ws} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := forkctl.SaveForkAgent(ws, "codex"); err != nil {
		t.Fatal(err)
	}
	plan := writeMatrixPlan(t, tmp, matrixPlan{Providers: map[string][][]matrixStep{
		"codex": {matrixGeneration("codex", "forked", ws, "fork answer")},
	}})
	proc := startScriptedACPArgs(t, coopBin, fixtureBin, repo, tmp, plan, []string{"fork", "myfork", "acp"}, nil, "codex")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := proc.client.req(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}); err != nil {
		t.Fatalf("fork initialize: %v\nstderr:\n%s", err, proc.stderr.String())
	}
	response, err := proc.client.req(ctx, "session/new", map[string]any{"cwd": repo, "mcpServers": []any{}})
	if err != nil {
		t.Fatalf("fork session/new: %v\nstderr:\n%s", err, proc.stderr.String())
	}
	state := filepath.Join(tmp, "fixture-state")
	promptMatrix(t, ctx, proc, filepath.Join(state, "codex-0", "wire.jsonl"), responseSessionID(response), "first question", "fork answer")
	proc.stop(t)

	calls := fixtureRuntimeCalls(t, state)
	supervisors := boxSupervisors(calls)
	if len(supervisors) != 1 {
		t.Fatalf("box runs name supervisors %q, want the fork session's one", supervisors)
	}
	servicesTeardown(t, calls, ws, supervisors[0])
	for _, project := range []string{box.ComposeProjectFor(repo, supervisors[0]), box.ComposeProject(repo), box.ComposeProject(ws)} {
		assertNeverSelectsProject(t, calls, project)
	}
}

// The next editor session in a project removes what a killed one left, before its first box: a dead
// stack could break that box's own services. Proof decides: services whose supervisor still runs and
// the development project `coop up` runs both stay, in the project and in a fork, where a fork's
// session runs its services.
func TestScriptedACPRemovesAKilledSessionsServicesFirst(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	build := scriptedTempDir(t)
	coopBin, fixtureBin := filepath.Join(build, "coop"), filepath.Join(build, "acpfixture")
	buildTestBinary(t, root, coopBin, ".")
	buildTestBinary(t, root, fixtureBin, "./internal/acpproxy/testdata/acpfixture")
	for _, tc := range []struct {
		name string
		args []string
		fork bool
	}{
		{name: "project", args: []string{"acp", "codex"}},
		{name: "fork", args: []string{"fork", "myfork", "acp"}, fork: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := scriptedTempDir(t)
			repo, containers := filepath.Join(tmp, "repo"), filepath.Join(tmp, "containers")
			workspace := repo
			if tc.fork {
				workspace = forkspace.Workspace(repo, "myfork")
			}
			for _, dir := range []string{filepath.Join(repo, ".agent"), workspace, containers} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if tc.fork {
				if err := forkctl.SaveForkAgent(workspace, "codex"); err != nil {
					t.Fatal(err)
				}
			}
			plan := writeMatrixPlan(t, tmp, matrixPlan{Providers: map[string][][]matrixStep{"codex": {sessionOpens, sessionOpens}}})
			open := func() *scriptedACP {
				t.Helper()
				return openScriptedSession(t, coopBin, fixtureBin, repo, tmp, plan, tc.args, map[string]string{"COOP_ACP_FIXTURE_CONTAINERS": containers})
			}
			open()
			state := filepath.Join(tmp, "fixture-state")
			before := fixtureRuntimeCalls(t, state)
			killed, running := killedAndRunningLabels(t, before)
			killedProject, runningProject := box.ComposeProjectFor(workspace, "killed"), box.ComposeProjectFor(workspace, "running")
			plantService(t, containers, "killed-db", killedProject, killed)
			plantService(t, containers, "running-db", runningProject, running)
			plantService(t, containers, "dev-db", box.ComposeProject(workspace), "")

			next := open()

			if out := next.stderr.String(); !strings.Contains(out, "Removed 1 project service container") {
				t.Errorf("the session did not say what it removed:\n%s", out)
			}
			calls := fixtureRuntimeCalls(t, state)[len(before):]
			firstBox := slices.IndexFunc(calls, func(call []string) bool { return call[0] == "run" })
			removed := slices.IndexFunc(calls, func(call []string) bool { return slices.Equal(call, []string{"rm", "-f", "killed-db"}) })
			if removed < 0 || firstBox < 0 || removed > firstBox {
				t.Fatalf("the killed session's service went at call %d, the next session's first box ran at call %d:\n%s", removed, firstBox, formatCalls(calls))
			}
			project := "label=com.docker.compose.project=" + killedProject
			for _, selects := range [][]string{{"network", "ls", "-q", "--filter", project}, {"volume", "ls", "-q", "--filter", project}} {
				if at := slices.IndexFunc(calls, func(call []string) bool { return slices.Equal(call, selects) }); at < removed || at > firstBox {
					t.Fatalf("%q ran at call %d, want it between the service's removal (%d) and the first box (%d):\n%s", selects, at, removed, firstBox, formatCalls(calls))
				}
			}
			if got := remainingFiles(t, containers); !slices.Equal(got, []string{"dev-db", "running-db"}) {
				t.Fatalf("containers left = %v, want the running session's and the development one's:\n%s", got, formatCalls(calls))
			}
			for _, project := range []string{runningProject, box.ComposeProject(workspace)} {
				assertNeverSelectsProject(t, calls, project)
			}
		})
	}
}

// `coop down` removes a killed session's services too: no Compose file can stop them, and a start
// refused over one of them names this command. A running session's stay.
func TestScriptedACPKilledSessionsServicesGoWithCoopDown(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	tmp := scriptedTempDir(t)
	coopBin, fixtureBin := filepath.Join(tmp, "coop"), filepath.Join(tmp, "acpfixture")
	buildTestBinary(t, root, coopBin, ".")
	buildTestBinary(t, root, fixtureBin, "./internal/acpproxy/testdata/acpfixture")
	repo, containers := filepath.Join(tmp, "repo"), filepath.Join(tmp, "containers")
	for _, dir := range []string{filepath.Join(repo, ".agent"), containers} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	plan := writeMatrixPlan(t, tmp, matrixPlan{Providers: map[string][][]matrixStep{"codex": {sessionOpens}}})
	env := map[string]string{"COOP_ACP_FIXTURE_CONTAINERS": containers}
	openScriptedSession(t, coopBin, fixtureBin, repo, tmp, plan, []string{"acp", "codex"}, env)
	killed, running := killedAndRunningLabels(t, fixtureRuntimeCalls(t, filepath.Join(tmp, "fixture-state")))
	plantService(t, containers, "killed-db", box.ComposeProjectFor(repo, "killed"), killed)
	plantService(t, containers, "running-db", box.ComposeProjectFor(repo, "running"), running)

	down := startScriptedACPArgs(t, coopBin, fixtureBin, repo, tmp, plan, []string{"down"}, env, "codex")
	down.stop(t)

	if out := down.stderr.String(); !strings.Contains(out, "Removed 1 project service container") {
		t.Errorf("coop down did not say what it removed:\n%s", out)
	}
	if got := remainingFiles(t, containers); !slices.Equal(got, []string{"running-db"}) {
		t.Fatalf("containers left = %v, want the running session's", got)
	}
}

// sessionOpens is a provider generation that only answers the editor's initialize.
var sessionOpens = []matrixStep{{Method: "initialize", Result: map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{}, "authMethods": []any{}}}}

// openScriptedSession opens an editor session up to its first box's answer, then ends it cleanly.
func openScriptedSession(t *testing.T, coopBin, fixtureBin, repo, tmp, plan string, args []string, env map[string]string) *scriptedACP {
	t.Helper()
	proc := startScriptedACPArgs(t, coopBin, fixtureBin, repo, tmp, plan, args, env, "codex")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := proc.client.req(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}); err != nil {
		t.Fatalf("initialize: %v\nstderr:\n%s", err, proc.stderr.String())
	}
	proc.stop(t)
	return proc
}

// killedAndRunningLabels reads the supervisor label of a box a finished session ran, which names a
// process that has exited, and returns it with the same label naming this test's own running
// process. Their scope and host are the ones Coop itself wrote, which a sweep checks first.
func killedAndRunningLabels(t *testing.T, calls [][]string) (killed, running string) {
	t.Helper()
	for _, call := range calls {
		if call[0] != "run" {
			continue
		}
		for _, arg := range call {
			label, ok := strings.CutPrefix(arg, box.LabelHost+"=")
			if !ok || label == "" {
				continue
			}
			fields := strings.SplitN(label, ":", 5) // the version, scope, host, pid and start token
			if len(fields) != 5 {
				t.Fatalf("supervisor label %q does not name a host and a pid", label)
			}
			pid := os.Getpid()
			return label, strings.Join(append(fields[:3:3], strconv.Itoa(pid), processidentity.StartToken(pid)), ":")
		}
	}
	t.Fatalf("no box run carries a supervisor label:\n%s", formatCalls(calls))
	return "", ""
}

// plantService adds a container the fixture lists: a service of project, with supervisor as its
// coop.host label when it has one.
func plantService(t *testing.T, dir, id, project, supervisor string) {
	t.Helper()
	labels := map[string]string{box.LabelKey: box.LabelService, "com.docker.compose.project": project}
	if supervisor != "" {
		labels[box.LabelHost] = supervisor
	}
	data, err := json.Marshal(labels)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func remainingFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// awaitFixtureGeneration waits until a box has started the fixture adapter generation named.
func awaitFixtureGeneration(t *testing.T, state, generation string) {
	t.Helper()
	wait.For(t, "fixture generation "+generation, func() bool {
		_, err := os.Stat(filepath.Join(state, generation))
		return err == nil
	})
}

// fixtureRuntimeCalls is every runtime call the fixture recorded, in order.
func fixtureRuntimeCalls(t *testing.T, state string) [][]string {
	t.Helper()
	f, err := os.Open(filepath.Join(state, "runtime.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var calls [][]string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		var call []string
		if err := json.Unmarshal(scanner.Bytes(), &call); err != nil || len(call) == 0 {
			t.Fatalf("unreadable runtime call %q: %v", scanner.Bytes(), err)
		}
		calls = append(calls, call)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return calls
}

// boxSupervisors lists the supervisor id each box ran under, in the order the ids first ran.
func boxSupervisors(calls [][]string) []string {
	var ids []string
	for _, call := range calls {
		if call[0] != "run" {
			continue
		}
		for _, arg := range call {
			if id, ok := strings.CutPrefix(arg, box.LabelSupervisor+"="); ok && !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

func firstBoxRun(calls [][]string, superID string) int {
	return slices.IndexFunc(calls, func(call []string) bool {
		return call[0] == "run" && slices.Contains(call, box.LabelSupervisor+"="+superID)
	})
}

// servicesTeardown returns where the removal of superID's project in workspace began: its containers
// listed by Compose's project and working-directory labels, then its networks and volumes by the
// project label. It fails the test when any of the three is missing.
func servicesTeardown(t *testing.T, calls [][]string, workspace, superID string) int {
	t.Helper()
	project := "label=com.docker.compose.project=" + box.ComposeProjectFor(workspace, superID)
	steps := [][]string{
		{"ps", "-q", "-a", "--filter", project, "--filter", "label=com.docker.compose.project.working_dir=" + filepath.Join(workspace, ".agent")},
		{"network", "ls", "-q", "--filter", project},
		{"volume", "ls", "-q", "--filter", project},
	}
	start, at := -1, 0
	for _, step := range steps {
		found := slices.IndexFunc(calls[at:], func(call []string) bool { return slices.Equal(call, step) })
		if found < 0 {
			t.Fatalf("no runtime call %q after call %d; the calls were:\n%s", step, at, formatCalls(calls))
		}
		at += found
		if start < 0 {
			start = at
		}
		at++
	}
	return start
}

func assertNeverSelectsProject(t *testing.T, calls [][]string, project string) {
	t.Helper()
	for i, call := range calls {
		if slices.Contains(call, "label=com.docker.compose.project="+project) {
			t.Fatalf("runtime call %d %q selects Compose project %s", i, call, project)
		}
	}
}

func formatCalls(calls [][]string) string {
	var b strings.Builder
	for i, call := range calls {
		fmt.Fprintf(&b, "  %d: %s\n", i, strings.Join(call, " "))
	}
	return b.String()
}
