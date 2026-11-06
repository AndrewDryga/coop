//go:build boxruntimee2e

package box

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/AndrewDryga/coop/internal/runtime"
)

// Config resolution exercises real Compose semantics without launching services or publishing ports.
func TestRuntimeComposeSnapshot(t *testing.T) {
	t.Setenv("literal", "synthetic-host-value-must-not-be-imported")
	t.Setenv("COOP_AUDIT_UNSET", "")
	rt, err := runtime.Detect(os.Getenv("COOP_RUNTIME"))
	if err != nil {
		t.Fatal(err)
	}
	repo, source := writeCompose(t, `services:
  db:
    image: postgres:18
    environment: ["PASSWORD=$$literal", "EMPTY="]
    volumes: ["./data:/data:ro"]
    ports: [{target: 5432, host_ip: 127.0.0.1}]
`)
	args, cleanup, _, err := snapshotComposeArgs(repo, source, false)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err := os.WriteFile(filepath.Join(filepath.Dir(source), ".env"),
		[]byte("BAD=${COOP_AUDIT_UNSET:?implicit env file must not be read}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if err := runCompose(rt, &out, &stderr, "config", append(args, "config", "--format", "json")); err != nil {
		t.Fatalf("resolve snapshot: %v: %s", err, stderr.String())
	}
	var doc struct {
		Services map[string]struct {
			Environment map[string]string
			Volumes     []struct{ Source string }
			Ports       []struct {
				HostIP string `json:"host_ip"`
			}
		}
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	db := doc.Services["db"]
	// Compose's rendered model retains the escape for reuse as a Compose input.
	if db.Environment["PASSWORD"] != "$$literal" || db.Environment["EMPTY"] != "" {
		t.Fatalf("literal environment changed: %+v", db.Environment)
	}
	if len(db.Volumes) != 1 || db.Volumes[0].Source != filepath.Join(filepath.Dir(source), "data") {
		t.Fatalf("snapshot changed relative bind anchor: %+v", db.Volumes)
	}
	if len(db.Ports) != 1 || db.Ports[0].HostIP != "127.0.0.1" {
		t.Fatalf("automatic publication lost loopback: %+v", db.Ports)
	}
}

// Real Compose merges the shadow override by target: a directory bind of the repo hands the sidecar
// a decoy at .env, and a direct bind of .env is replaced by the decoy — the raw file never reaches it.
func TestRuntimeComposeShadowsRepoSecretsIntoSidecars(t *testing.T) {
	rt, err := runtime.Detect(os.Getenv("COOP_RUNTIME"))
	if err != nil {
		t.Fatal(err)
	}
	repo, source := writeCompose(t, `services:
  db:
    image: alpine
    volumes: ["../:/repo:ro", "../.env:/env:ro"]
`)
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("COOP_SYNTHETIC_CANARY=not-a-real-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args, cleanup, _, err := snapshotComposeArgs(repo, source, false)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	var out, stderr bytes.Buffer
	if err := runCompose(rt, &out, &stderr, "config", append(args, "config", "--format", "json")); err != nil {
		t.Fatalf("compose config: %v\n%s", err, stderr.String())
	}
	var doc struct {
		Services map[string]struct {
			Volumes []struct{ Source, Target, Type string }
		}
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	byTarget := map[string]string{}
	for _, v := range doc.Services["db"].Volumes {
		byTarget[v.Target] = v.Source
	}
	realRepo, _ := filepath.EvalSymlinks(repo)
	fromRepo := func(src string) bool { return strings.HasPrefix(src, repo) || strings.HasPrefix(src, realRepo) }
	decoyRoot, err := serviceStateRoot("decoys")
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"/env", "/repo/.env"} {
		src := byTarget[target]
		if src == "" || fromRepo(src) || filepath.Dir(src) != decoyRoot {
			t.Errorf("%s is bound from %q; want the decoy, never the repo's .env\n%s", target, src, out.String())
		}
	}
	if src := byTarget["/repo"]; !fromRepo(src) {
		t.Errorf("/repo is bound from %q; want the repo itself", src)
	}
}

func TestRuntimeComposeLogicalOwnersShareSourceButNotServices(t *testing.T) {
	rt, err := runtime.Detect(os.Getenv("COOP_RUNTIME"))
	if err != nil {
		t.Fatal(err)
	}
	repo, source := writeCompose(t, `services:
  probe:
    image: alpine:3.24.2
    command: ["sh", "-c", "sleep 300"]
    expose: ["8080"]
    volumes:
      - data:/data
      - ../live.txt:/workspace/live.txt:ro
volumes:
  data: {}
`)
	live := filepath.Join(repo, "live.txt")
	if err := os.WriteFile(live, []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const loopOwner = "runtime-loop"
	t.Cleanup(func() {
		_ = StopServicesForOwner(t.Context(), rt, repo, repo, loopOwner, true)
		_ = StopServicesForOwner(t.Context(), rt, repo, repo, "", true)
	})

	dev, err := startServicesFileContext(t.Context(), rt, repo, source, "", "", nil, io.Discard, io.Discard, false, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	loop, err := startServicesFileContext(t.Context(), rt, repo, source, loopOwner, "", nil, io.Discard, io.Discard, false, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(dev.ports) != 1 || len(loop.ports) != 1 || dev.ports[0].HostPort == loop.ports[0].HostPort {
		t.Fatalf("dev/loop ports are not distinct: dev=%+v loop=%+v", dev.ports, loop.ports)
	}

	composeExec(t, rt, repo, source, "", "echo dev > /data/owner")
	composeExec(t, rt, repo, source, loopOwner, "echo loop > /data/owner")
	if got := composeExec(t, rt, repo, source, "", "cat /data/owner"); got != "dev" {
		t.Fatalf("development data = %q", got)
	}
	if got := composeExec(t, rt, repo, source, loopOwner, "cat /data/owner"); got != "loop" {
		t.Fatalf("loop data = %q", got)
	}
	if err := os.WriteFile(live, []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"", loopOwner} {
		if got := composeExec(t, rt, repo, source, owner, "cat /workspace/live.txt"); got != "two" {
			t.Fatalf("owner %q live source = %q", owner, got)
		}
	}
	if err := StopServicesForOwner(t.Context(), rt, repo, repo, "", true); err != nil {
		t.Fatal(err)
	}
	if got := composeExec(t, rt, repo, source, loopOwner, "cat /data/owner"); got != "loop" {
		t.Fatalf("stopping development disturbed loop data: %q", got)
	}
}

// A killed run's own project goes at the next sweep, containers, volume and network, while the
// development project stays. The project's file plants a live supervisor; only coop's own override,
// naming the dead one, can make the run's services removable.
func TestRuntimeComposeReapOrphanServicesRemovesAKilledRunsProject(t *testing.T) {
	rt, err := runtime.Detect(os.Getenv("COOP_RUNTIME"))
	if err != nil {
		t.Fatal(err)
	}
	repo, source := writeCompose(t, "")
	body := `services:
  probe:
    image: alpine:3.24.2
    command: ["sh", "-c", "sleep 300"]
    labels:
      - coop=service
      - coop.host=` + liveSupervisorLabel(t, repo) + `
    volumes:
      - data:/data
volumes:
  data: {}
`
	if err := os.WriteFile(source, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	const runOwner = "killed-loop"
	run := ComposeProjectFor(repo, runOwner)
	t.Cleanup(func() {
		_ = StopServicesForOwner(context.Background(), rt, repo, repo, runOwner, true)
		_ = StopServicesForOwner(context.Background(), rt, repo, repo, "", true)
	})
	if _, err := startServicesFileContext(t.Context(), rt, repo, source, "", "", nil, io.Discard, io.Discard, false, false, nil, nil); err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{LabelKey: LabelService, LabelHost: goneSupervisorLabel(t, repo)}
	if _, err := startServicesFileContext(t.Context(), rt, repo, source, runOwner, "", labels, io.Discard, io.Discard, false, false, nil, nil); err != nil {
		t.Fatal(err)
	}

	removed, err := ReapOrphanServices(t.Context(), rt, repo)
	if err != nil || removed != 1 {
		t.Fatalf("ReapOrphanServices = %d, %v; want the killed run's one container", removed, err)
	}
	// The network sweep spans every workspace; removing just this project's network proves the same:
	// nothing is attached any more.
	if _, err := RemoveProjectNetworks(t.Context(), rt, run); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		args []string
		want string
	}{
		{[]string{"ps", "-aq", "--filter", "label=" + composeProjectLabel + "=" + run}, ""},
		{[]string{"volume", "ls", "-q", "--filter", "label=" + composeProjectLabel + "=" + run}, ""},
		{[]string{"network", "ls", "-q", "--filter", "label=" + composeProjectLabel + "=" + run}, ""},
	} {
		var out bytes.Buffer
		if code, err := rt.Run(nil, &out, io.Discard, check.args...); err != nil || code != 0 {
			t.Fatalf("%v: %d, %v", check.args, code, err)
		}
		if got := strings.TrimSpace(out.String()); got != check.want {
			t.Errorf("%v left %q", check.args, got)
		}
	}
	if got := composeExec(t, rt, repo, source, "", "echo dev"); got != "dev" {
		t.Fatalf("development project after the sweep = %q", got)
	}
}

// Two forks start their services at once, as two `coop fork <name> -d` started together do. Each
// prepares its services with `compose up --no-start`, as a filtered start does, then sweeps unused
// networks, as every start does. Each then reads its own network, as a filtered start does to address
// its services, and starts them. Docker files a created container under its network's name until it
// starts, so a sweep that counted a network's users by id took the sibling's prepared network, and that
// sibling failed (2026-11-05). Compose's own `up` makes a missing network again, so only the read
// between the steps shows the loss. Each sweeps only its sibling's project here, by the same code as
// the host-wide sweep, so the test leaves other projects' networks alone.
func TestRuntimeComposeTwoWorkspacesStartTheirServicesTogether(t *testing.T) {
	rt, err := runtime.Detect(os.Getenv("COOP_RUNTIME"))
	if err != nil {
		t.Fatal(err)
	}
	type workspace struct {
		repo string
		args []string
	}
	var workspaces []workspace
	for range 2 {
		repo, source := writeCompose(t, `services:
  probe:
    image: alpine:3.24.2
    command: ["sh", "-c", "sleep 300"]
`)
		args, cleanup, _, err := snapshotComposeArgs(repo, source, false)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(cleanup)
		t.Cleanup(func() { _ = StopServicesForOwner(context.Background(), rt, repo, repo, "", true) })
		workspaces = append(workspaces, workspace{repo, args})
	}
	var prepared, swept sync.WaitGroup
	prepared.Add(len(workspaces))
	swept.Add(len(workspaces))
	results := make(chan error, len(workspaces))
	for i, w := range workspaces {
		sibling := ComposeProject(workspaces[1-i].repo)
		network := ComposeProject(w.repo) + "_default"
		go func() {
			var stderr bytes.Buffer
			err := runCompose(rt, io.Discard, &stderr, "up --no-start", append(slices.Clone(w.args), "up", "--no-start"))
			prepared.Done()
			if err == nil {
				// Both are prepared, and neither has started, before either sweeps.
				prepared.Wait()
				_, err = RemoveProjectNetworks(t.Context(), rt, sibling)
			}
			swept.Done()
			if err != nil {
				results <- fmt.Errorf("prepare %s: %w: %s", w.repo, err, stderr.String())
				return
			}
			swept.Wait()
			if code, err := rt.Run(nil, io.Discard, io.Discard, "network", "inspect", network); err != nil || code != 0 {
				results <- fmt.Errorf("a sibling's sweep removed the prepared network %s: %d, %v", network, code, err)
				return
			}
			results <- runCompose(rt, io.Discard, &stderr, "up", append(slices.Clone(w.args), "up", "-d", "--wait"))
		}()
	}
	for range workspaces {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
}

func composeExec(t *testing.T, rt runtime.Runtime, repo, source, owner, command string) string {
	t.Helper()
	args := []string{
		"compose", "-p", ComposeProjectFor(repo, owner),
		"--project-directory", filepath.Dir(source), "--env-file", os.DevNull, "-f", source,
		"exec", "-T", "probe", "sh", "-c", command,
	}
	var stdout, stderr bytes.Buffer
	if err := runCompose(rt, &stdout, &stderr, "exec", args); err != nil {
		t.Fatalf("compose exec for owner %q: %v: %s", owner, err, stderr.String())
	}
	return strings.TrimSpace(stdout.String())
}
