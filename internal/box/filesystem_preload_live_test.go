//go:build filesystemruntimee2e

package box

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/fsfilter"
	"github.com/AndrewDryga/coop/internal/randid"
	"github.com/AndrewDryga/coop/internal/runtime"
)

func TestFilesystemPreloadCaptureFailureRuntime(t *testing.T) {
	if os.Getenv("COOP_FILESYSTEM_RUNTIME_TEST") != "1" {
		t.Skip("requires explicit local Docker qualification")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	docker, err := runtime.BindDocker(ctx, runtime.Runtime{Name: "docker"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer docker.Close()
	state, err := serviceStateRoot("filesystem")
	if err != nil {
		t.Fatal(err)
	}
	// Hold the real capture lock until the early helper is observed running.
	locked, release, lockDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		lockDone <- withHiddenFilesLock(ctx, repo, func() error {
			close(locked)
			<-release
			return nil
		})
	}()
	select {
	case <-locked:
	case err := <-lockDone:
		t.Fatal("could not hold capture lock", err)
	}
	released := false
	unlock := func() {
		if !released {
			close(release)
			released = true
			if err := <-lockDone; err != nil {
				t.Error(err)
			}
		}
	}
	done := make(chan struct{})
	var admission *filesystemAdmission
	var captureErr error
	go func() {
		admission, captureErr = prepareFilesystemAdmission(ctx, runtime.Runtime{Name: "docker"}, []string{repo}, [][]string{{"missing-grant"}}, nil)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		unlock()
		<-done
		if admission != nil {
			if err := admission.close(); err != nil {
				t.Error(err)
			}
		}
	})
	var observed filesystemRecord
	for observed.Helper.ID == "" {
		paths, err := filepath.Glob(filepath.Join(state, "coop-fs-*", "record.json"))
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range paths {
			record, err := readFilesystemRecord(filepath.Dir(file))
			if err != nil || record.Workspace != repo || record.Helper.ID == "" {
				continue
			}
			value, present, err := docker.InspectContainer(ctx, record.Helper)
			if err != nil {
				t.Fatal(err)
			}
			if present && value.State.Running {
				observed = record
				break
			}
		}
		if observed.Helper.ID != "" {
			break
		}
		select {
		case <-done:
			t.Fatal("admission returned before starting its helper", captureErr)
		case <-ctx.Done():
			t.Fatal("helper did not start before capture", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	unlock()
	<-done
	if admission != nil || captureErr == nil || !strings.Contains(captureErr.Error(), "ineligible filesystem read grant") {
		t.Fatal("expected host capture failure after helper start", captureErr)
	}
	if _, present, err := docker.InspectContainer(ctx, observed.Helper); err != nil || present {
		t.Fatal("capture failure retained its helper", err)
	}
	for _, probe := range observed.Probes {
		if _, err := os.Stat(filepath.Join(probe.Root, probe.Leaf)); !os.IsNotExist(err) {
			t.Fatal("capture failure retained its probe", err)
		}
	}
	paths, err := filepath.Glob(filepath.Join(state, observed.Parent+"-*"))
	if err != nil || len(paths) != 0 {
		t.Fatal("capture failure retained custody", paths, err)
	}
}

func preloadFilesystemImage(t *testing.T, docker *runtime.Docker) string {
	t.Helper()
	changed := 0
	image := filesystemFixtureImage(t, docker, "preload-failures", func(name string, data []byte) []byte {
		replace := func(before, after string) {
			if bytes.Count(data, []byte(before)) != 1 {
				t.Fatalf("preload injection point changed: %s: %s", name, before)
			}
			data = bytes.Replace(data, []byte(before), []byte(after), 1)
		}
		switch name {
		case "internal/fsfilter/assets/loader.c":
			changed++
			replace("    if (bpf_object__load(object)) goto fail;", `    if (bpf_object__load(object)) goto fail;
    const char *fault = getenv("COOP_TEST_FS_FAULT");
    if (!fault) fault = "";
    int loaded = open("/state/programs-loaded", O_CREAT|O_EXCL|O_WRONLY|O_CLOEXEC, 0600);
    if (loaded < 0 || close(loaded)) goto fail;
    if (!strcmp(fault, "load-exit")) _exit(42);`)
			replace("    if (fflush(stdout)) goto fail;", `    if (fflush(stdout)) goto fail;
    if (!strcmp(fault, "child-death")) {
        while (access("/state/exit-child", F_OK)) usleep(10000);
        _exit(47);
    }`)
		case "cmd/coop-fs/admit_linux.go":
			changed++
			replace("import (", "import (\n\t\"bytes\"\n\t\"fmt\"")
			replace("id, err := currentBoot()", `if os.Getenv("COOP_TEST_FS_FAULT") == "capacity" {
        sources = "/tmp/sources"
        if err := os.MkdirAll(sources+"/1", 0700); err != nil { return err }
        for i := 0; i < fsfilter.MaxPaths-1; i++ {
            if err := os.WriteFile(fmt.Sprintf("%s/1/%05d", sources, i), nil, 0600); err != nil { return err }
        }
    }
    id, err := currentBoot()`)
			replace("childErr = command.Wait()", `childErr = command.Wait()
        if os.Getenv("COOP_TEST_FS_FAULT") == "child-death" {
            _ = os.WriteFile("/state/child-reaped", nil, 0600)
            for { time.Sleep(time.Hour) }
        }`)
			replace("return fsfilter.WriteKernelPolicies(pipe, policy.Roots, unicodeData)", `var wire bytes.Buffer
    if err := fsfilter.WriteKernelPolicies(&wire, policy.Roots, unicodeData); err != nil { return err }
    data := wire.Bytes()
    switch os.Getenv("COOP_TEST_FS_FAULT") {
    case "truncated": data = data[:len(data)-1]
    case "trailing": data = append(data, 1)
    case "stream-pause":
        if _, err := pipe.Write(data[:28]); err != nil { return err }
        if err := os.WriteFile("/state/stream-started", nil, 0600); err != nil { return err }
        for { time.Sleep(time.Hour) }
    }
    _, err = pipe.Write(data)
    return err`)
		}
		return data
	})
	if changed != 2 {
		t.Fatalf("preload fixture modified %d sources", changed)
	}
	return image
}

func TestFilesystemPreloadRuntime(t *testing.T) {
	if os.Getenv("COOP_FILESYSTEM_RUNTIME_TEST") != "1" || os.Getenv("COOP_FILESYSTEM_CLIENT_IMAGE") == "" {
		t.Skip("requires explicit local Docker qualification")
	}
	docker, err := runtime.BindDocker(t.Context(), runtime.Runtime{Name: "docker"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer docker.Close()
	image := preloadFilesystemImage(t, docker)
	for _, fault := range []string{"delayed", "missing", "load-exit", "malformed", "truncated", "trailing", "stream-pause", "child-death", "capacity"} {
		t.Run(fault, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			repo, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{".env", "public"} {
				if err := os.WriteFile(filepath.Join(repo, name), []byte("fixture"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			binding, err := runtime.BindDockerForSetup(ctx, runtime.Runtime{Name: "docker"}, docker.Endpoint(), docker.Info().ID)
			if err != nil {
				t.Fatal(err)
			}
			parent := "coop-fs-" + randid.Hex(16)
			a := &filesystemAdmission{docker: binding, dir: dir, record: filesystemRecord{Version: 1, Parent: parent, Endpoint: binding.Endpoint(), DaemonID: binding.Info().ID, Image: image, Helper: filesystemRef(parent, "owner"), Workspace: repo, Supervisor: supervisorLabelValue(workspaceScope(repo), os.Getpid())}}
			a.lease, err = lockFilesystemAdmission(dir, true)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := a.close(); err != nil {
					t.Error(err)
				}
				if _, err := os.Stat(dir); !os.IsNotExist(err) {
					t.Errorf("retained preload custody: %v", err)
				}
			})
			if err := a.persist(); err != nil {
				t.Fatal(err)
			}
			options := append(a.helperOptions(false), "--mount", networkMount("bind", repo, "/sources/1", true), "-e", "COOP_TEST_FS_FAULT="+fault)
			if _, err := a.create(ctx, runtime.DockerCreate{Ref: a.record.Helper, Image: image, Options: options, Command: []string{"admit", "/sources", "/state/policy.json", "/host-cgroup/" + parent}}); err != nil {
				t.Fatal(err)
			}
			if err := a.docker.StartContainer(ctx, a.record.Helper); err != nil {
				t.Fatal(err)
			}
			await := func(ready func() bool) {
				t.Helper()
				for !ready() {
					select {
					case <-ctx.Done():
						t.Fatal("preload fixture did not reach its barrier", ctx.Err())
					case <-time.After(10 * time.Millisecond):
					}
				}
			}
			marker := func(name string) bool { _, err := os.Stat(filepath.Join(dir, name)); return err == nil }
			await(func() bool { return marker("programs-loaded") })
			noAuthority := func() {
				t.Helper()
				if marker("birth.json") {
					t.Fatal("incomplete policy acquired attachment custody")
				}
				_, log, err := a.docker.ObserveContainerLogs(ctx, a.record.Helper, 30, 1<<20)
				if err != nil || bytes.Contains(log, []byte(`"status":"ready"`)) {
					t.Fatalf("incomplete policy reached READY: %s %v", log, err)
				}
			}
			noAuthority()
			if fault == "missing" {
				cancel()
				return // Ordinary cleanup cancels the waiting owner without policy.
			}
			if fault != "load-exit" {
				policy := fsfilter.Admission{Roots: []fsfilter.Policy{{Profile: fsfilter.Profile{Fold: true, Normalize: true}, Entries: []fsfilter.Entry{{Path: ".env", Flags: fsfilter.Secret}}}}}
				if fault == "capacity" {
					policy.Roots[0].Entries = make([]fsfilter.Entry, fsfilter.MaxPaths-1)
					for i := range policy.Roots[0].Entries {
						policy.Roots[0].Entries[i] = fsfilter.Entry{Path: fmt.Sprintf("%05d", i), Flags: fsfilter.Secret}
					}
				}
				if fault == "malformed" {
					err = os.WriteFile(filepath.Join(dir, "policy.json"), []byte("invalid"), 0600)
				} else {
					err = filesystemJSON(filepath.Join(dir, "policy.json"), policy)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if fault == "stream-pause" {
				await(func() bool { return marker("stream-started") })
				noAuthority()
				cancel()
				return
			}
			if fault != "delayed" && fault != "child-death" && fault != "capacity" {
				await(func() bool {
					value, present, err := a.docker.InspectContainer(ctx, a.record.Helper)
					if err != nil || !present {
						t.Fatal("missing loader exit evidence", err)
					}
					if !value.State.Running && value.State.ExitCode == 0 {
						t.Fatal("invalid policy succeeded")
					}
					return !value.State.Running
				})
				noAuthority()
				return
			}
			if err := a.waitReady(ctx); err != nil {
				value, _, inspectErr := a.docker.InspectContainer(ctx, a.record.Helper)
				log, logErr := exec.CommandContext(ctx, docker.Binary(), "--host", docker.Endpoint(), "logs", "--tail", "20", a.record.Helper.ID).CombinedOutput()
				t.Fatalf("%v; state=%+v inspect=%v logs=%v\n%s", err, value.State, inspectErr, logErr, log)
			}
			if fault == "capacity" {
				if a.record.Ready.Pins != fsfilter.MaxPaths {
					t.Fatalf("maximum policy retained %d pins", a.record.Ready.Pins)
				}
				data, err := exec.CommandContext(ctx, docker.Binary(), "--host", docker.Endpoint(), "stats", "--no-stream", "--format", "{{json .}}", a.record.Helper.ID).CombinedOutput()
				if err != nil {
					t.Fatal("maximum policy memory observation", err)
				}
				t.Logf("100000 pinned objects within the 512MiB/32-PID helper limit: %s", data)
				return
			}
			if fault == "delayed" {
				return
			}
			ref := filesystemRef(parent, "consumer")
			if err := a.consumer(ref); err != nil {
				t.Fatal(err)
			}
			client, _, err := docker.Image(ctx, os.Getenv("COOP_FILESYSTEM_CLIENT_IMAGE"))
			if err != nil {
				t.Fatal(err)
			}
			options = []string{"--network", "none", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--cgroup-parent", "/" + parent, "--mount", networkMount("bind", repo, "/workspace", true), "--entrypoint", "python3"}
			ref.ID, err = a.create(ctx, runtime.DockerCreate{Ref: ref, Image: client, Options: options, Command: []string{"-c", "import time; time.sleep(120)"}})
			if err != nil {
				t.Fatal(err)
			}
			if err := a.docker.StartContainer(ctx, ref); err != nil {
				t.Fatal(err)
			}
			if _, err := a.docker.ExecRead(ctx, ref, 4096, "python3", "-c", "assert open('/workspace/public').read() == 'fixture'"); err != nil {
				t.Fatal("public control failed before owner death", err)
			}
			if err := os.WriteFile(filepath.Join(dir, "exit-child"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			await(func() bool { return marker("child-reaped") })
			value, present, err := a.docker.InspectContainer(ctx, a.record.Helper)
			if err != nil || !present || !value.State.Running {
				t.Fatal("Go supervisor did not survive C owner death", err)
			}
			script := "import errno\nfor p in ['public', '.env']:\n try: open('/workspace/'+p)\n except PermissionError as e: assert e.errno == errno.EPERM\n else: raise AssertionError(p+' remained readable')"
			if _, err := a.docker.ExecRead(ctx, ref, 4096, "python3", "-c", script); err != nil {
				t.Fatal("C owner death did not deny the admitted device", err)
			}
		})
	}
}
