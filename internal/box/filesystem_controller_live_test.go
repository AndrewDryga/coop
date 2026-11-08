//go:build filesystemruntimee2e

package box

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/fsfilter"
	"github.com/AndrewDryga/coop/internal/randid"
	"github.com/AndrewDryga/coop/internal/runtime"
)

func init() { cleanupFilesystemRuntime = cleanupTestFilesystemControllers }

// TestMain runs this before removing its private host state. Stop only the
// exact controllers named in that isolated registry, and exercise real recovery.
func cleanupTestFilesystemControllers() error { return cleanupTestFilesystemControllerImage("") }

func cleanupTestFilesystemControllerImage(image string) error {
	if os.Getenv("COOP_FILESYSTEM_RUNTIME_TEST") != "1" {
		return nil
	}
	state, err := serviceStateRoot("filesystem-controllers")
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(state)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var problems []error
	rt := runtime.Runtime{Name: "docker"}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "coop-fs-controller-") {
			continue
		}
		r, err := readFilesystemController(filepath.Join(state, entry.Name()))
		if err != nil {
			problems = append(problems, err)
			continue
		}
		if image != "" && r.Image != image {
			continue
		}
		docker, err := runtime.BindDockerForSetup(ctx, rt, r.Endpoint, r.DaemonID)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		err = docker.RemoveContainer(ctx, r.Container)
		docker.Close()
		if err != nil {
			problems = append(problems, err)
		}
	}
	_, err = recoverFilesystemControllers(ctx, rt, nil)
	return errors.Join(append(problems, err)...)
}

func TestFilesystemSharedControllerRuntime(t *testing.T) {
	if os.Getenv("COOP_FILESYSTEM_RUNTIME_TEST") != "1" || os.Getenv("COOP_FILESYSTEM_CLIENT_IMAGE") == "" {
		t.Skip("requires local Docker qualification")
	}
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{".env": "inert secret", "public": "public"} {
		if err = os.WriteFile(filepath.Join(repo, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	rt := runtime.Runtime{Name: "docker"}
	admit := func(grants []string) *filesystemAdmission {
		a, err := prepareFilesystemAdmission(ctx, rt, []string{repo}, [][]string{grants}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if _, err := os.Stat(a.dir); err == nil {
				if err = a.close(); err != nil {
					t.Error(err)
				}
			}
		})
		return a
	}
	denied := admit(nil)
	granted := admit([]string{".env"})
	if denied.record.Controller.Container.ID != granted.record.Controller.Container.ID {
		t.Fatal("warm admission reloaded the controller")
	}
	if denied.record.Ready.CgroupInode == granted.record.Ready.CgroupInode {
		t.Fatal("admissions share a policy identity")
	}
	for _, a := range denied.record.Ready.Maps {
		for _, b := range granted.record.Ready.Maps {
			if a.ID == b.ID {
				t.Fatal("admissions share an inner map")
			}
		}
	}
	consumer := func(a *filesystemAdmission) runtime.DockerRef {
		ref := filesystemRef(a.record.Parent, "test-consumer")
		if err := a.consumer(ref); err != nil {
			t.Fatal(err)
		}
		image, _, err := a.docker.Image(ctx, os.Getenv("COOP_FILESYSTEM_CLIENT_IMAGE"))
		if err != nil {
			t.Fatal(err)
		}
		options := []string{"--network", "none", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--cgroup-parent", a.record.cgroupParent(), "--mount", networkMount("bind", repo, "/workspace", true), "--entrypoint", "python3"}
		ref.ID, err = a.create(ctx, runtime.DockerCreate{Ref: ref, Image: image, Options: options, Command: []string{"-c", "import time;time.sleep(120)"}})
		if err != nil {
			t.Fatal(err)
		}
		if err = a.docker.StartContainer(ctx, ref); err != nil {
			t.Fatal(err)
		}
		return ref
	}
	d, g := consumer(denied), consumer(granted)
	check := func(a *filesystemAdmission, ref runtime.DockerRef, secret, public bool) {
		// .ENV names the secret only where the volume folds case (APFS); elsewhere use .env.
		script := fmt.Sprintf(`import errno,os
secret='/workspace/.ENV' if os.path.lexists('/workspace/.ENV') else '/workspace/.env'
for path,allowed in [(secret,%s),('/workspace/public',%s)]:
 try: open(path).read()
 except OSError as e: assert not allowed and e.errno==errno.EPERM,(path,e)
 else: assert allowed,path
print('POLICY_OK')`, map[bool]string{true: "True", false: "False"}[secret], map[bool]string{true: "True", false: "False"}[public])
		if !public {
			// Nothing is readable once the admission lost its owner.
			if err := deniedAfterOwnerDeath(ctx, a.docker, ref, script); err != nil {
				t.Fatalf("policy check: %v", err)
			}
			return
		}
		out, err := a.docker.ExecRead(ctx, ref, 4096, "python3", "-c", script)
		if err != nil || !bytes.Contains(out, []byte("POLICY_OK")) {
			t.Fatalf("policy check: %v %s", err, out)
		}
	}
	check(denied, d, false, true)
	check(granted, g, true, true)
	if err = denied.docker.RemoveContainer(ctx, denied.record.Helper); err != nil {
		t.Fatal(err)
	}
	check(denied, d, false, false)
	check(granted, g, true, true)
	if err = denied.close(); err != nil {
		t.Fatal(err)
	}
	check(granted, g, true, true)
	old := granted.record.Controller.Container.ID
	if err = granted.docker.RemoveContainer(ctx, granted.record.Controller.Container); err != nil {
		t.Fatal(err)
	}
	check(granted, g, false, false)
	fresh := admit(nil)
	if fresh.record.Controller.Container.ID == old {
		t.Fatal("dead controller revived")
	}
	f := consumer(fresh)
	check(fresh, f, false, true)
	check(granted, g, false, false)
	if err = granted.close(); err != nil {
		t.Fatal(err)
	}
	check(fresh, f, false, true)
	// Losing a published receipt cannot authorize pre-publication cleanup.
	if err = fresh.docker.RemoveContainer(ctx, f); err != nil {
		t.Fatal(err)
	}
	// A stopped, unrecorded descendant remains restartable and blocks retirement.
	unknown := filesystemRef(fresh.record.Parent, "unknown-descendant")
	client, _, err := fresh.docker.Image(ctx, os.Getenv("COOP_FILESYSTEM_CLIENT_IMAGE"))
	if err != nil {
		t.Fatal(err)
	}
	unknown.ID, err = fresh.docker.CreateContainer(ctx, runtime.DockerCreate{Ref: unknown, Image: client,
		Options: []string{"--network", "none", "--cgroup-parent", nestedCgroupParent(fresh.record), "--entrypoint", "true"}})
	if err != nil {
		t.Fatal(err)
	}
	blocked := fresh.docker.CgroupParentUnused(ctx, fresh.record.cgroupParent())
	if err = fresh.docker.RemoveContainer(ctx, unknown); err != nil {
		t.Fatal(err)
	}
	if blocked == nil {
		t.Fatal("unknown stopped descendant did not block retirement")
	}
	if err = fresh.docker.CgroupParentUnused(ctx, fresh.record.cgroupParent()); err != nil {
		t.Fatal(err)
	}
	if err = fresh.docker.RemoveContainer(ctx, fresh.record.Helper); err != nil {
		t.Fatal(err)
	}
	birthPath := filepath.Join(fresh.dir, "birth.json")
	birth, err := os.ReadFile(birthPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(birthPath); err != nil {
		t.Fatal(err)
	}
	retireErr := fresh.retireSharedPolicy(ctx)
	if err = os.WriteFile(birthPath, birth, 0600); err != nil {
		t.Fatal(err)
	}
	if retireErr == nil {
		t.Fatal("published admission lost custody after missing birth")
	}
	t.Log("opposite policies, independent owner death, sibling retirement and fresh controller after death passed")
}

func bindFilesystemTestController(t *testing.T, a *filesystemAdmission, roots []string) {
	t.Helper()
	c, err := acquireFilesystemController(t.Context(), a.docker, a.record.Image, roots)
	if err != nil {
		t.Fatal(err)
	}
	a.controller = c
	a.record.Controller = &c.record
	if err := filesystemJSON(filepath.Join(a.dir, "controller.json"), c.record.Ready); err != nil {
		c.closeLease()
		t.Fatal(err)
	}
}

func fixtureFilesystemController(t *testing.T, docker *runtime.Docker, image string, env []string, marker string, ready bool) *filesystemController {
	t.Helper()
	state, err := filesystemControllerState()
	if err != nil {
		t.Fatal(err)
	}
	name := "coop-fs-controller-" + randid.Hex(16)
	dir, err := os.MkdirTemp(state, name+"-")
	if err != nil {
		t.Fatal(err)
	}
	driver, err := filesystemCgroups(docker.Info())
	if err != nil {
		t.Fatal(err)
	}
	c := &filesystemController{dir: dir, record: filesystemControllerRecord{Version: 1, Name: name, Directory: filepath.Base(dir), Endpoint: docker.Endpoint(), DaemonID: docker.Info().ID, Image: image, Container: filesystemRef(name, "controller"), CgroupDriver: driver}}
	c.lease, err = filesystemControllerLock(t.Context(), filepath.Join(dir, "lease"), false, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { retireFixtureFilesystemController(t, docker, c) })
	if err = c.persist(); err != nil {
		t.Fatal(err)
	}
	opts := (&filesystemAdmission{dir: dir}).helperOptions(false, c.record.systemd())
	for _, e := range env {
		opts = append(opts, "-e", e)
	}
	if _, err = c.create(t.Context(), docker, runtime.DockerCreate{Ref: c.record.Container, Image: image, Options: opts, Command: []string{"controller", name}}); err != nil {
		t.Fatal(err)
	}
	if err = docker.StartContainer(t.Context(), c.record.Container); err != nil {
		t.Fatal(err)
	}
	fixtureFilesystemMarker(t, docker, c.record.Container, dir, marker)
	if ready {
		value, data, err := docker.ObserveContainerLogs(t.Context(), c.record.Container, 30, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range bytes.Split(data, []byte{'\n'}) {
			var r fsfilter.ControllerReady
			if json.Unmarshal(line, &r) == nil && r.Validate() == nil {
				c.record.Ready = &r
			}
		}
		if c.record.Ready == nil {
			t.Fatalf("controller did not publish readiness: %s", data)
		}
		c.record.StartedAt = value.State.StartedAt
		if err = filesystemJSON(filepath.Join(dir, "ready.json"), c.record.Ready); err != nil {
			t.Fatal(err)
		}
		if err = c.persist(); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func fixtureFilesystemMarker(t *testing.T, docker *runtime.Docker, ref runtime.DockerRef, dir, name string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	for {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err == nil {
			return data
		}
		if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			data, _ := docker.ContainerLogs(context.Background(), ref, 30, 1<<20)
			t.Fatalf("missing %s: %s", name, data)
		case <-time.After(10 * time.Millisecond):
		}
	}
}
func retireFixtureFilesystemController(t *testing.T, docker *runtime.Docker, c *filesystemController) {
	t.Helper()
	if _, err := os.Stat(c.dir); os.IsNotExist(err) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c.closeLease()
	if err := c.remove(ctx, docker, c.record.Container); err != nil {
		t.Error(err)
		return
	}
	if _, err := recoverFilesystemController(ctx, runtime.Runtime{Name: "docker"}, c.dir); err != nil {
		t.Error(err)
	}
}

func TestFilesystemConcurrentEnrollmentRuntime(t *testing.T) {
	if os.Getenv("COOP_FILESYSTEM_RUNTIME_TEST") != "1" {
		t.Skip("requires local Docker qualification")
	}
	const count = 24
	roots := make([]string, count)
	for i := range roots {
		var err error
		roots[i], err = filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(roots[i], ".env"), []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	type outcome struct {
		controller string
		err        error
	}
	results := make(chan outcome, count)
	limit := make(chan struct{}, 4)
	for _, root := range roots {
		go func() {
			limit <- struct{}{}
			defer func() { <-limit }()
			a, err := prepareFilesystemAdmission(ctx, runtime.Runtime{Name: "docker"}, []string{root}, [][]string{nil}, nil)
			if err != nil {
				results <- outcome{err: err}
				return
			}
			results <- outcome{controller: a.record.Controller.Name, err: a.close()}
		}()
	}
	controller := ""
	for range roots {
		result := <-results
		if result.err != nil {
			t.Error(result.err)
			continue
		}
		if controller == "" {
			controller = result.controller
		}
		if controller != result.controller {
			t.Error("concurrent enrollment selected different live controllers")
		}
	}
}

// nestedCgroupParent names a cgroup inside an admission's, in the form its Docker driver takes.
func nestedCgroupParent(r filesystemRecord) string {
	if parent, ok := strings.CutSuffix(r.cgroupParent(), ".slice"); ok {
		return parent + "-nested.slice"
	}
	return r.cgroupParent() + "/nested"
}
