package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// The approved `coop up` / `coop down` transcripts. They run the real commands against a shim
// that answers like Docker, so the fixtures pin what coop SAYS around the runtime's own output —
// including, for --delete-volumes, that every target is named before anything is removed.

// composeShim is a stand-in runtime named `docker`, so the prose about it reads the way it does
// on a real host. It answers the four things the service commands ask: is the daemon up, which
// services does this file declare, which volumes does this project own, and then the up/down.
type composeShim struct {
	services   []string
	volumes    []string
	configJSON string // `compose config --format json`, which is where a published port comes from
	upExit     int
	upSays     string // what Compose said before it failed, when the transcript pins the cause
	downExit   int
	downSays   string
	volExit    int
}

func (s composeShim) build(t *testing.T) runtime.Runtime {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "docker")
	var script strings.Builder
	script.WriteString("#!/bin/sh\ncase \"$*\" in\n")
	script.WriteString("  info) exit 0 ;;\n")
	script.WriteString("  *\"config --services\"*)\n")
	for _, name := range s.services {
		script.WriteString("    printf '%s\\n' '" + name + "'\n")
	}
	script.WriteString("    ;;\n")
	script.WriteString("  \"volume ls\"*)\n")
	for _, name := range s.volumes {
		script.WriteString("    printf '%s\\n' '" + name + "'\n")
	}
	if s.volExit != 0 {
		script.WriteString("    exit " + strconv.Itoa(s.volExit) + "\n")
	}
	script.WriteString("    ;;\n")
	// Compose's own progress is a variable region the transcripts write as [Compose output]; the
	// shim emits exactly that, so a fixture pins where coop's lines sit around the runtime's —
	// including the blank line that separates them, which collapses when the runtime says nothing.
	script.WriteString("  *\"config --format json\"*)\n")
	if s.configJSON != "" {
		script.WriteString("    cat <<'JSON'\n" + s.configJSON + "\nJSON\n")
	}
	script.WriteString("    ;;\n")
	script.WriteString("  *\" up \"*) echo '[Compose output]'; " + says(s.upSays) + "exit " + strconv.Itoa(s.upExit) + " ;;\n")
	script.WriteString("  *\" down \"*) echo '[Compose output]'; " + says(s.downSays) + "exit " + strconv.Itoa(s.downExit) + " ;;\n")
	script.WriteString("esac\n")
	if err := os.WriteFile(path, []byte(script.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	return runtime.Runtime{Name: path}
}

// says is the runtime's own diagnostic on the failure paths whose transcript pins the cause it
// reported — an unhealthy service, a port already taken.
func says(line string) string {
	if line == "" {
		return ""
	}
	return "echo '" + line + "' >&2; "
}

// serviceProject writes a project whose Compose file declares postgres and redis, the state the
// service transcripts were approved against.
func serviceProject(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	compose := "services:\n  db:\n    image: postgres:18\n  redis:\n    image: redis:8\n"
	if err := os.WriteFile(filepath.Join(repo, ".agent", "compose.yml"), []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

func TestApprovedServiceStart(t *testing.T) {
	t.Run("28a-services-ready", func(t *testing.T) {
		repo := serviceProject(t)
		a := &app{cfg: &config.Config{RepoOverride: repo}, rt: composeShim{services: []string{"db", "redis"}}.build(t), rtSet: true}
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdUp(nil) })
		if code != 0 {
			t.Fatalf("cmdUp = %d, want success:\n%s", code, out)
		}
		assertApprovedOutput(t, "28a-services-ready", out)
	})

	t.Run("28d-no-compose-file", func(t *testing.T) {
		repo := t.TempDir()
		a := &app{cfg: &config.Config{RepoOverride: repo}, rt: composeShim{}.build(t), rtSet: true}
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdUp(nil) })
		if code != 1 {
			t.Errorf("a project with no services exited %d, want 1", code)
		}
		assertApprovedOutput(t, "28d-no-compose-file", out)
	})

	t.Run("28e-compose-has-no-services", func(t *testing.T) {
		repo := serviceProject(t)
		a := &app{cfg: &config.Config{RepoOverride: repo}, rt: composeShim{}.build(t), rtSet: true}
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdUp(nil) })
		if code != 1 {
			t.Errorf("an empty Compose config exited %d, want 1", code)
		}
		assertApprovedOutput(t, "28e-compose-has-no-services", out)
	})

	// Starting services that are already up is the same postcondition, said the same way: coop
	// does not buy a "nothing to do" line with an extra runtime query.
	t.Run("28b-services-already-running", func(t *testing.T) {
		repo := serviceProject(t)
		a := &app{cfg: &config.Config{RepoOverride: repo}, rt: composeShim{services: []string{"db", "redis"}}.build(t), rtSet: true}
		if code, _ := a.cmdUp(nil); code != 0 {
			t.Fatalf("first start = %d", code)
		}
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdUp(nil) })
		if code != 0 {
			t.Fatalf("second start = %d:\n%s", code, out)
		}
		assertApprovedOutput(t, "28b-services-already-running", out)
	})

	// The configured Compose path is the one named, and a service that really publishes a port
	// gets the URL a browser can use. A service the box reaches by name gets none.
	t.Run("28c-services-custom-path-port", func(t *testing.T) {
		repo := t.TempDir()
		if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(repo, "infra"), 0o755); err != nil {
			t.Fatal(err)
		}
		compose := "services:\n  db:\n    image: postgres:18\n  web:\n    image: nginx:1\n    expose: [\"8080\"]\n"
		if err := os.WriteFile(filepath.Join(repo, "infra", "dev-compose.yml"), []byte(compose), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, ".agent", "project.yaml"),
			[]byte("box:\n  compose: infra/dev-compose.yml\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		shim := composeShim{services: []string{"db", "web"},
			configJSON: `{"services":{"db":{},"web":{"expose":["8080"]}}}`}
		a := &app{cfg: &config.Config{RepoOverride: repo}, rt: shim.build(t), rtSet: true}
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdUp(nil) })
		if code != 0 {
			t.Fatalf("cmdUp = %d:\n%s", code, out)
		}
		assertApprovedOutput(t, "28c-services-custom-path-port", normalizePort(t, a.rt, repo, "infra/dev-compose.yml", out))
	})

	// No runtime means nothing started, and the fix is to start the runtime.
	t.Run("28g-runtime-unavailable", func(t *testing.T) {
		repo := serviceProject(t)
		a := &app{cfg: &config.Config{RepoOverride: repo}, rt: downRuntime(t), rtSet: true}
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdUp(nil) })
		if code != 1 {
			t.Errorf("a start with no runtime exited %d, want 1", code)
		}
		assertApprovedOutput(t, "28g-runtime-unavailable", out)
	})

	// A service that never became healthy is Compose's own answer, kept as the cause — and the
	// result does NOT claim everything is ready.
	t.Run("28o-service-not-healthy", func(t *testing.T) {
		repo := serviceProject(t)
		shim := composeShim{services: []string{"db", "redis"}, upExit: 1}
		a := &app{cfg: &config.Config{RepoOverride: repo}, rt: shim.build(t), rtSet: true}
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdUp(nil) })
		if code != 1 {
			t.Errorf("an unhealthy start exited %d, want 1", code)
		}
		assertApprovedOutput(t, "28o-service-not-healthy", out)
	})

	// A temporary directory an agent can see would let the snapshot coop validated be swapped
	// before Docker opens it. That is refused before anything runs.
	t.Run("28q-snapshot-unsafe", func(t *testing.T) {
		repo := serviceProject(t)
		inside := filepath.Join(repo, "tmp")
		if err := os.MkdirAll(inside, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("TMPDIR", inside)
		a := &app{cfg: &config.Config{RepoOverride: repo}, rt: composeShim{services: []string{"db"}}.build(t), rtSet: true}
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdUp(nil) })
		if code != 1 {
			t.Errorf("an exposed TMPDIR exited %d, want 1", code)
		}
		assertApprovedOutput(t, "28q-snapshot-unsafe", out)
	})

	t.Run("28h-runtime-has-no-compose", func(t *testing.T) {
		repo := serviceProject(t)
		a := &app{cfg: &config.Config{RepoOverride: repo}, rt: runtime.Runtime{Name: "container"}, rtSet: true}
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdUp(nil) })
		if code != 1 {
			t.Errorf("a runtime without Compose exited %d, want 1", code)
		}
		assertApprovedOutput(t, "28h-runtime-has-no-compose", out)
	})
}

func TestApprovedServiceStop(t *testing.T) {
	t.Run("29a-services-stopped", func(t *testing.T) {
		repo := serviceProject(t)
		a := &app{cfg: &config.Config{RepoOverride: repo}, rt: composeShim{services: []string{"db", "redis"}}.build(t), rtSet: true}
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdDown(nil) })
		if code != 0 {
			t.Fatalf("cmdDown = %d, want success:\n%s", code, out)
		}
		assertApprovedOutput(t, "29a-services-stopped", out)
	})

	// Stopping what is already stopped keeps the same postcondition and the same sentence.
	t.Run("29b-services-not-running", func(t *testing.T) {
		repo := serviceProject(t)
		a := &app{cfg: &config.Config{RepoOverride: repo}, rt: composeShim{services: []string{"db", "redis"}}.build(t), rtSet: true}
		if code, _ := a.cmdDown(nil); code != 0 {
			t.Fatalf("first stop = %d", code)
		}
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdDown(nil) })
		if code != 0 {
			t.Fatalf("second stop = %d:\n%s", code, out)
		}
		assertApprovedOutput(t, "29b-services-not-running", out)
	})

	// A stop that began and did not finish says so: claiming nothing changed after the runtime
	// already worked would send someone looking in the wrong place.
	t.Run("29j-stop-failed", func(t *testing.T) {
		repo := serviceProject(t)
		shim := composeShim{services: []string{"db", "redis"}, downExit: 1}
		a := &app{cfg: &config.Config{RepoOverride: repo}, rt: shim.build(t), rtSet: true}
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdDown(nil) })
		if code != 1 {
			t.Errorf("a failed stop exited %d, want 1", code)
		}
		assertApprovedOutput(t, "29j-stop-failed", out)
	})

	// The same failure while volumes were being deleted is a different fact, because some of them
	// may be gone.
	t.Run("29k-volume-deletion-failed", func(t *testing.T) {
		repo := serviceProject(t)
		shim := composeShim{services: []string{"db", "redis"}, volumes: projectVolumes(t, repo), downExit: 1}
		a := &app{cfg: &config.Config{RepoOverride: repo}, rt: shim.build(t), rtSet: true}
		typedAnswers(t, "y")
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdDown([]string{"--delete-volumes"}) })
		if code != 1 {
			t.Errorf("a failed deletion exited %d, want 1", code)
		}
		assertApprovedSession(t, "29k-volume-deletion-failed", normalizeProject(t, repo, out))
	})

	// The preview, then the shared gate, then the stop — in that order, so the answer is given
	// against the list and nothing is removed before it.
	t.Run("29d-delete-volumes-accepted", func(t *testing.T) {
		repo := serviceProject(t)
		shim := composeShim{services: []string{"db", "redis"}, volumes: projectVolumes(t, repo)}
		a := &app{cfg: &config.Config{RepoOverride: repo}, rt: shim.build(t), rtSet: true}
		typedAnswers(t, "y")
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdDown([]string{"--delete-volumes"}) })
		if code != 0 {
			t.Fatalf("an accepted deletion exited %d:\n%s", code, out)
		}
		assertApprovedSession(t, "29d-delete-volumes-accepted", normalizeProject(t, repo, out))
	})

	// A bare Enter is No: the gate's default is the safe one, and nothing is deleted or stopped.
	t.Run("29e-delete-volumes-declined", func(t *testing.T) {
		repo := serviceProject(t)
		shim := composeShim{services: []string{"db", "redis"}, volumes: projectVolumes(t, repo)}
		a := &app{cfg: &config.Config{RepoOverride: repo}, rt: shim.build(t), rtSet: true}
		typedAnswers(t, "")
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdDown([]string{"--delete-volumes"}) })
		if code != 2 {
			t.Errorf("a declined deletion exited %d, want 2", code)
		}
		assertApprovedSession(t, "29e-delete-volumes-declined", normalizeProject(t, repo, out))
	})

	t.Run("29c-compose-path-missing", func(t *testing.T) {
		a := &app{cfg: &config.Config{RepoOverride: t.TempDir()}, rt: composeShim{}.build(t), rtSet: true}
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdDown(nil) })
		if code != 1 {
			t.Errorf("a missing Compose file exited %d, want 1", code)
		}
		assertApprovedOutput(t, "29c-compose-path-missing", out)
	})

	// Without a terminal nobody can answer, so a deletion that was not explicitly confirmed is
	// refused — after the targets have been named, so the reader knows what they are confirming.
	t.Run("29f-delete-volumes-needs-a-terminal", func(t *testing.T) {
		repo := serviceProject(t)
		shim := composeShim{services: []string{"db", "redis"}, volumes: projectVolumes(t, repo)}
		a := &app{cfg: &config.Config{RepoOverride: repo}, rt: shim.build(t), rtSet: true}
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdDown([]string{"--delete-volumes"}) })
		if code != 2 {
			t.Errorf("an unconfirmed deletion exited %d, want 2", code)
		}
		assertApprovedOutput(t, "29f-delete-volumes-needs-a-terminal", normalizeProject(t, repo, out))
	})

	t.Run("29g-delete-volumes-yes", func(t *testing.T) {
		repo := serviceProject(t)
		shim := composeShim{services: []string{"db", "redis"}, volumes: projectVolumes(t, repo)}
		a := &app{cfg: &config.Config{RepoOverride: repo}, rt: shim.build(t), rtSet: true}
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdDown([]string{"--delete-volumes", "--yes"}) })
		if code != 0 {
			t.Fatalf("a confirmed deletion exited %d:\n%s", code, out)
		}
		assertApprovedOutput(t, "29g-delete-volumes-yes", normalizeProject(t, repo, out))
	})

	// Nothing to delete is a fact the runtime supplied, not an assumption: it is said out loud,
	// and the stop still runs.
	t.Run("29h-delete-volumes-no-targets", func(t *testing.T) {
		repo := serviceProject(t)
		a := &app{cfg: &config.Config{RepoOverride: repo}, rt: composeShim{services: []string{"db", "redis"}}.build(t), rtSet: true}
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdDown([]string{"--delete-volumes"}) })
		if code != 0 {
			t.Fatalf("a deletion with no targets exited %d:\n%s", code, out)
		}
		assertApprovedOutput(t, "29h-delete-volumes-no-targets", out)
	})

	// An unanswerable runtime stops the command: "no volumes" would be a guess, printed right
	// before a permanent deletion.
	t.Run("29i-delete-volumes-scope-unresolved", func(t *testing.T) {
		repo := serviceProject(t)
		a := &app{cfg: &config.Config{RepoOverride: repo}, rt: composeShim{volExit: 1}.build(t), rtSet: true}
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdDown([]string{"--delete-volumes"}) })
		if code != 1 {
			t.Errorf("an unresolved deletion scope exited %d, want 1", code)
		}
		assertApprovedOutput(t, "29i-delete-volumes-scope-unresolved", out)
	})
}

// The retired spelling is gone, not hidden: it must fail as an unknown flag rather than quietly
// deleting somebody's data because they typed the old form from memory.
func TestDownRejectsTheRetiredVolumesFlag(t *testing.T) {
	for _, flag := range []string{"-v", "--volumes"} {
		a := &app{cfg: &config.Config{RepoOverride: t.TempDir()}}
		code, err := a.cmdDown([]string{flag})
		if code != 2 || err == nil || !strings.Contains(err.Error(), flag) {
			t.Errorf("coop down %s = (%d, %v); want a usage refusal naming it", flag, code, err)
		}
	}
}

// downRuntime is a runtime whose daemon does not answer — the state before Docker is started.
func downRuntime(t *testing.T) runtime.Runtime {
	t.Helper()
	path := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return runtime.Runtime{Name: path}
}

// normalizePort replaces this checkout's stable per-workspace host port with the one the approved
// transcript records: the number is a property of the path, not of the copy. It asks the same
// public resolver the start used, so a transcript still fails if coop printed a different port.
func normalizePort(t *testing.T, rt runtime.Runtime, repo, compose, out string) string {
	t.Helper()
	ports := box.ServicePorts(rt, repo, filepath.Join(repo, compose))
	if len(ports) != 1 {
		t.Fatalf("the project publishes %d ports, want exactly the one the transcript records", len(ports))
	}
	return strings.ReplaceAll(out, strconv.Itoa(ports[0].HostPort), "43517")
}

// projectVolumes names the two volumes a coop-scaffolded project owns, under this project's real
// Compose project name — the value the preview prints.
func projectVolumes(t *testing.T, repo string) []string {
	t.Helper()
	project := composeProjectName(t, repo)
	return []string{project + "_pgdata", project + "_redisdata"}
}

// normalizeProject replaces the per-checkout Compose project name with the one the approved
// transcript records: the hash is a property of the path, not of the copy.
func normalizeProject(t *testing.T, repo, out string) string {
	t.Helper()
	return strings.ReplaceAll(out, composeProjectName(t, repo), "coop-atlas")
}

func composeProjectName(t *testing.T, repo string) string {
	t.Helper()
	return box.ComposeProject(repo)
}
