//go:build filesystemruntimee2e

package box

import (
	"bytes"
	"context"
	"errors"
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

func TestFilesystemPreloadBeforeCaptureRuntime(t *testing.T) {
	if os.Getenv("COOP_FILESYSTEM_RUNTIME_TEST") != "1" {
		t.Skip("requires explicit local Docker qualification")
	}
	for _, outcome := range []string{"success", "capture-error", "cancel"} {
		t.Run(outcome, func(t *testing.T) { filesystemBlockedCapture(t, outcome) })
	}
}

func filesystemBlockedCapture(t *testing.T, outcome string) {
	t.Helper()
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
	// The owner must be running before capture proceeds, but it must have attached
	// no policy, and nothing may be written into the project.
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
	defer unlock()
	done := make(chan struct{})
	var admission *filesystemAdmission
	var captureErr error
	var grants []string
	if outcome == "capture-error" {
		grants = []string{"missing-grant"}
	}
	go func() {
		admission, captureErr = prepareFilesystemAdmission(ctx, runtime.Runtime{Name: "docker"}, []string{repo}, [][]string{grants}, nil)
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
			if !present || !value.State.Running {
				continue
			}
			if record.Ready != nil || len(record.Consumers) != 0 {
				t.Fatal("admission became ready or gained a consumer before capture completed", record.Consumers)
			}
			for _, name := range []string{"policy.json", "birth.json"} {
				if _, err := os.Stat(filepath.Join(filepath.Dir(file), name)); !os.IsNotExist(err) {
					t.Fatal("policy authority published before capture completed", name, err)
				}
			}
			// The old per-launch share check had its folder in the project at exactly this point.
			if entries, err := os.ReadDir(repo); err != nil || len(entries) != 0 {
				t.Fatalf("admission wrote into the project before capture: %v %v", entries, err)
			}
			observed = record
			break
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
	if outcome == "cancel" {
		cancel()
	}
	unlock()
	<-done
	switch outcome {
	case "success":
		if admission == nil || captureErr != nil || admission.record.Ready == nil {
			t.Fatal("capture did not complete admission", captureErr)
		}
		if err := admission.close(); err != nil {
			t.Fatal(err)
		}
		admission = nil
	case "capture-error":
		if admission != nil || captureErr == nil || !strings.Contains(captureErr.Error(), "ineligible filesystem read grant") {
			t.Fatal("expected host capture failure after helper start", captureErr)
		}
	case "cancel":
		if admission != nil || !errors.Is(captureErr, context.Canceled) {
			t.Fatal("canceled capture continued admission", captureErr)
		}
	}
	if _, present, err := docker.InspectContainer(t.Context(), observed.Helper); err != nil || present {
		t.Fatal("capture cleanup retained a container", observed.Helper.Name, err)
	}
	// Admission writes nothing into the project it protects.
	if entries, err := os.ReadDir(repo); err != nil || len(entries) != 0 {
		t.Fatalf("admission wrote into the project: %v %v", entries, err)
	}
	paths, err := filepath.Glob(filepath.Join(state, observed.Parent+"-*"))
	if err != nil || len(paths) != 0 {
		t.Fatal("capture failure retained custody", paths, err)
	}
}

func TestFilesystemPreparationPanicCleanupRuntime(t *testing.T) {
	if os.Getenv("COOP_FILESYSTEM_RUNTIME_TEST") != "1" {
		t.Skip("requires explicit local Docker qualification")
	}
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, err := prepareFilesystemAdmission(t.Context(), runtime.Runtime{Name: "docker"}, []string{repo}, [][]string{nil}, nil)
	if err != nil {
		t.Fatal(err)
	}
	observer, err := runtime.BindDocker(t.Context(), runtime.Runtime{Name: "docker"}, a.docker.Endpoint(), a.docker.Info().ID)
	if err != nil {
		_ = a.close()
		t.Fatal(err)
	}
	defer observer.Close()
	var recovered any
	var result error
	transferred, settled := false, false
	// A stopped consumer created while the other branch panics must still be removed.
	consumer := filesystemRef(a.record.Parent, "fixture")
	func() {
		defer func() { recovered = recover() }()
		defer a.cleanupUntransferred(&result, &transferred)
		panicking := make(chan struct{})
		result = together(t.Context(), func() error {
			<-panicking
			if err := a.consumer(consumer); err != nil {
				return err
			}
			var createErr error
			consumer.ID, createErr = a.create(t.Context(), runtime.DockerCreate{Ref: consumer, Image: a.record.Image, Options: []string{"--network", "none", "--read-only", "--cgroup-parent", a.record.cgroupParent()}, Command: []string{"unicode-table", "/tmp/unused"}})
			settled = true
			return createErr
		}, func() error {
			close(panicking)
			panic("fixture preparation panic")
		})
	}()
	if !settled || consumer.ID == "" || !strings.Contains(fmt.Sprint(recovered), "fixture preparation panic") {
		t.Fatalf("panic did not join preparation: settled=%t consumer=%s panic=%v", settled, consumer.ID, recovered)
	}
	if result != nil {
		t.Fatal("panic cleanup failed", result)
	}
	for _, ref := range []runtime.DockerRef{a.record.Helper, consumer} {
		if _, present, err := observer.InspectContainer(t.Context(), ref); err != nil || present {
			t.Fatal("panic cleanup retained a container", ref.Name, err)
		}
	}
	if _, err := os.Stat(a.dir); !os.IsNotExist(err) {
		t.Fatal("panic cleanup retained custody", err)
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
		case "internal/fsfilter/assets/shared.h":
			changed++
			replace("    char magic[8];uint32_t header[5];FILE *in=stdin;", `    const char *fault = getenv("COOP_TEST_FS_FAULT");
    if (!fault) fault = "";
    int loaded = open("/state/policy-maps-created", O_CREAT|O_EXCL|O_WRONLY|O_CLOEXEC, 0600);
    if (loaded < 0 || close(loaded)) return -1;
    if (!strcmp(fault, "load-exit")) _exit(42);
    char magic[8];uint32_t header[5];FILE *in=stdin;`)
			replace(`    for(int i=0;i<4;i++)if(bpf_map_update_elem(shared_fd('m',policy_names[i]),&key,&policy_maps[i],BPF_NOEXIST))return -1;`, `    if (!strcmp(fault,"publish-0")) _exit(48);
    for(int i=0;i<4;i++) {
        if(bpf_map_update_elem(shared_fd('m',policy_names[i]),&key,&policy_maps[i],BPF_NOEXIST))return -1;
        char stage[16];snprintf(stage,sizeof(stage),"publish-%d",i+1);
        if (!strcmp(fault,stage)) _exit(48);
    }`)
			replace(`    if(invoke(shared_fd('p',"admit_owner"),key,token))return -1;`, `    if (!strcmp(fault,"delayed") && !invoke(shared_fd('p',"admit_owner"),key,token^1)) return -1;
    if(invoke(shared_fd('p',"admit_owner"),key,token))return -1;
    if (!strcmp(fault,"delayed")) {
        if (!invoke(shared_fd('p',"admit_owner"),key,token) ||
            !invoke(shared_fd('p',"admit_owner"),key,token^1) ||
            !invoke(shared_fd('p',"release_owner"),key,token)) return -1;
    }`)
			replace(`    map_json(stdout);puts("}");if(fflush(stdout))return -1;`, `    map_json(stdout);puts("}");if(fflush(stdout))return -1;
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
	for _, fault := range []string{"delayed", "missing", "load-exit", "malformed", "truncated", "trailing", "stream-pause", "child-death", "capacity", "publish-0", "publish-1", "publish-2", "publish-3", "publish-4"} {
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
			bindFilesystemTestController(t, a, []string{repo})
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
			options := append(a.helperOptions(false, *a.record.Controller), "--mount", networkMount("bind", repo, "/sources/1", true), "-e", "COOP_TEST_FS_FAULT="+fault)
			if _, err := a.create(ctx, runtime.DockerCreate{Ref: a.record.Helper, Image: image, Options: options, Command: []string{"admit", "/sources", "/state/policy.json", parent, "/state/controller.json"}}); err != nil {
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
			await(func() bool { return marker("policy-maps-created") })
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
				if strings.HasPrefix(fault, "publish-") {
					if !marker("birth.json") {
						t.Fatal("partial publication lacks durable birth")
					}
					_, log, err := a.docker.ObserveContainerLogs(ctx, a.record.Helper, 30, 1<<20)
					if err != nil || bytes.Contains(log, []byte(`"status":"ready"`)) {
						t.Fatalf("partial publication reached READY: %s %v", log, err)
					}
				} else {
					noAuthority()
				}
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
			ref := filesystemRef(parent, "consumer")
			if err := a.consumer(ref); err != nil {
				t.Fatal(err)
			}
			client, _, err := docker.Image(ctx, os.Getenv("COOP_FILESYSTEM_CLIENT_IMAGE"))
			if err != nil {
				t.Fatal(err)
			}
			options = []string{"--network", "none", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--cgroup-parent", a.record.cgroupParent(), "--mount", networkMount("bind", repo, "/workspace", true), "--entrypoint", "python3"}
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
			if fault == "delayed" {
				if _, err := a.docker.ExecRead(ctx, ref, 4096, "python3", "-c", "import errno,os\ntry: open('/workspace/.ENV')\nexcept OSError as e: assert e.errno==errno.EPERM or e.errno==errno.ENOENT and not os.path.lexists('/workspace/.ENV')\nelse: raise AssertionError('secret exposed')"); err != nil {
					t.Fatal(err)
				}
				return
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
			if err := deniedAfterOwnerDeath(ctx, a.docker, ref, script); err != nil {
				t.Fatal("C owner death did not deny the admitted device", err)
			}
		})
	}
}
