//go:build filesystemruntimee2e

package box

import (
	"bytes"
	"encoding/json"
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

type filesystemKernelIdentity struct {
	Kind, Name                    string
	ID, Program, Target, Function uint32
}

func normalizerFilesystemImage(t *testing.T, docker *runtime.Docker) string {
	t.Helper()
	fixture, err := os.ReadFile("testdata/filesystem_normalizer_fixture.c")
	if err != nil {
		t.Fatal(err)
	}
	changed := 0
	image := filesystemFixtureImage(t, docker, "normalizer-lifetime", func(name string, data []byte) []byte {
		replace := func(before, after string) {
			if bytes.Count(data, []byte(before)) != 1 {
				t.Fatalf("normalizer injection point changed: %s: %s", name, before)
			}
			data = bytes.Replace(data, []byte(before), []byte(after), 1)
		}
		switch name {
		case "internal/fsfilter/assets/loader.c":
			changed++
			replace("static int load_normalizer(", string(fixture)+"\nstatic int load_normalizer(")
			replace("normalizer_links[nnormalizer_links++] = link;", `normalizer_links[nnormalizer_links++] = link;
        if (nnormalizer_links == 13 && test_fault("partial")) {
            test_snapshot(); errno = ECANCELED; return -1;
        }`)
		case "internal/fsfilter/assets/shared.h":
			changed++
			replace("int main(int argc,char **argv) {", `int main(int argc,char **argv) {
    if(argc>1&&(!strcmp(argv[1],"test-gone")||!strcmp(argv[1],"test-signal")))return test_control(argc,argv);`)
			replace("if(!object||validate_hooks()||bpf_object__load(object)||load_normalizer(&options))return -1;", `if(!object||validate_hooks()||bpf_object__load(object))return -1;
    if(test_fault("load-exit")){test_snapshot();_exit(42);}
    if(load_normalizer(&options))return -1;`)
			replace(`    puts("]}");if(fflush(stdout))return -1;`, `    puts("]}");if(fflush(stdout))return -1;
    test_snapshot();
    sigset_t signals;sigemptyset(&signals);sigaddset(&signals,SIGUSR2);
    if(sigprocmask(SIG_BLOCK,&signals,NULL))return -1;
    for(;;){int signal;if(sigwait(&signals,&signal))return -1;test_signal();}`)

		}
		return data
	})
	if changed != 2 {
		t.Fatalf("normalizer fixture modified %d sources", changed)
	}
	return image
}

func filesystemNormalizerController(t *testing.T, docker *runtime.Docker, image, fault string) (*filesystemController, []filesystemKernelIdentity) {
	t.Helper()
	c := fixtureFilesystemController(t, docker, image, []string{"COOP_TEST_NORMALIZER=" + fault}, "normalizer-ids", fault != "partial" && fault != "load-exit")
	data := fixtureFilesystemMarker(t, docker, c.record.Container, c.dir, "normalizer-ids")
	var ids []filesystemKernelIdentity
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
		var id filesystemKernelIdentity
		if err := json.Unmarshal(line, &id); err != nil || id.ID == 0 {
			t.Fatalf("invalid kernel identity: %s %v", line, err)
		}
		ids = append(ids, id)
	}
	return c, ids
}

func filesystemNormalizerAdmission(t *testing.T, docker *runtime.Docker, c *filesystemController, roots []string, policies []fsfilter.Policy) *filesystemAdmission {
	t.Helper()
	binding, err := runtime.BindDockerForSetup(t.Context(), runtime.Runtime{Name: "docker"}, docker.Endpoint(), docker.Info().ID)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	parent := "coop-fs-" + randid.Hex(16)
	a := &filesystemAdmission{docker: binding, dir: dir, record: filesystemRecord{Version: 1, Parent: parent, Endpoint: binding.Endpoint(), DaemonID: binding.Info().ID, Image: c.record.Image, Helper: filesystemRef(parent, "owner"), Workspace: roots[0], Supervisor: supervisorLabelValue(workspaceScope(roots[0]), os.Getpid()), Controller: &c.record}}
	a.controller, err = leaseRecordedFilesystemController(t.Context(), binding, c.record, true, roots...)
	if err != nil {
		t.Fatal(err)
	}
	a.lease, err = lockFilesystemAdmission(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := os.Stat(dir); err == nil {
			if err = a.close(); err != nil {
				t.Error(err)
			}
		}
	})
	if err = a.persist(); err != nil {
		t.Fatal(err)
	}
	if err = filesystemJSON(filepath.Join(dir, "controller.json"), c.record.Ready); err != nil {
		t.Fatal(err)
	}
	if err = filesystemJSON(filepath.Join(dir, "policy.json"), fsfilter.Admission{Roots: policies}); err != nil {
		t.Fatal(err)
	}
	options := a.helperOptions(false)
	for i, root := range roots {
		options = append(options, "--mount", networkMount("bind", root, fmt.Sprintf("/sources/%d", i+1), true))
	}
	if _, err = a.create(t.Context(), runtime.DockerCreate{Ref: a.record.Helper, Image: c.record.Image, Options: options, Command: []string{"admit", "/sources", "/state/policy.json", parent, "/state/controller.json"}}); err != nil {
		t.Fatal(err)
	}
	if err = a.docker.StartContainer(t.Context(), a.record.Helper); err != nil {
		t.Fatal(err)
	}
	if err = a.waitReady(t.Context()); err != nil {
		t.Fatal(err)
	}
	return a
}

func filesystemNormalizerConsumer(t *testing.T, a *filesystemAdmission, roots []string) runtime.DockerRef {
	t.Helper()
	ref := filesystemRef(a.record.Parent, "consumer")
	if err := a.consumer(ref); err != nil {
		t.Fatal(err)
	}
	client, _, err := a.docker.Image(t.Context(), os.Getenv("COOP_FILESYSTEM_CLIENT_IMAGE"))
	if err != nil {
		t.Fatal(err)
	}
	options := []string{"--network", "none", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--cgroup-parent", a.record.cgroupParent(), "--entrypoint", "python3"}
	for i, root := range roots {
		options = append(options, "--mount", networkMount("bind", root, fmt.Sprintf("/root%d", i+1), true))
	}
	ref.ID, err = a.create(t.Context(), runtime.DockerCreate{Ref: ref, Image: client, Options: options, Command: []string{"-c", "import time; time.sleep(300)"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.docker.StartContainer(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	return ref
}

func filesystemNormalizerGone(t *testing.T, docker *runtime.Docker, image string, ids []filesystemKernelIdentity) {
	t.Helper()
	args := []string{"--host", docker.Endpoint(), "run", "--rm", "--network", "none", "--cap-drop", "ALL", "--cap-add", "SYS_ADMIN", "--security-opt", "seccomp=unconfined", "--entrypoint", "/usr/local/libexec/coop-fs-kernel", image, "test-gone"}
	count := 0
	for _, id := range ids {
		if id.Kind != "function" {
			args = append(args, fmt.Sprintf("%s:%d", id.Kind, id.ID))
			count++
		}
	}
	if count == 0 {
		t.Fatal("no kernel identities to verify")
	}
	if data, err := exec.CommandContext(t.Context(), docker.Binary(), args...).CombinedOutput(); err != nil {
		t.Fatalf("kernel objects survived: %v: %s", err, data)
	}
}

func TestFilesystemNormalizerLifetimeRuntime(t *testing.T) {
	if os.Getenv("COOP_FILESYSTEM_RUNTIME_TEST") != "1" || os.Getenv("COOP_FILESYSTEM_CLIENT_IMAGE") == "" {
		t.Skip("requires local Docker qualification")
	}
	docker, err := runtime.BindDocker(t.Context(), runtime.Runtime{Name: "docker"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer docker.Close()
	image := normalizerFilesystemImage(t, docker)
	for _, fault := range []string{"normal", "detach", "partial", "load-exit", "controller-death"} {
		t.Run(fault, func(t *testing.T) {
			c, ids := filesystemNormalizerController(t, docker, image, fault)
			filesystemNormalizerIdentities(t, ids, fault)
			if fault == "partial" || fault == "load-exit" {
				if c.record.Ready != nil {
					t.Fatal("partial controller reached READY")
				}
				if _, err := os.Stat(filepath.Join(c.dir, "birth.json")); !os.IsNotExist(err) {
					t.Fatal("partial replacement published birth", err)
				}
			} else {
				root := filesystemNormalizerRoot(t)
				a := filesystemNormalizerAdmission(t, docker, c, []string{root}, []fsfilter.Policy{filesystemNormalizerPolicy("alpha")})
				ref := filesystemNormalizerConsumer(t, a, []string{root})
				filesystemNormalizerExec(t, a, ref, "assert open('/root1/beta').read()=='fixture'\ndeny('/root1/alpha',open)")
				if fault == "detach" || fault == "controller-death" {
					if _, err := docker.ExecRead(t.Context(), c.record.Container, 4096, "/usr/local/libexec/coop-fs-kernel", "test-signal"); err != nil {
						t.Fatal(err)
					}
					if fault == "detach" {
						fixtureFilesystemMarker(t, docker, c.record.Container, c.dir, "replacement-detached")
						filesystemNormalizerExec(t, a, ref, "assert os.stat('/root1/beta').st_size==7\ndeny('/root1/beta',open)\ndeny('/root1/alpha',open)")
					} else {
						deadline := time.Now().Add(time.Minute)
						for {
							value, present, err := docker.InspectContainer(t.Context(), c.record.Container)
							if err != nil || !present {
								t.Fatal(err)
							}
							if !value.State.Running {
								break
							}
							if time.Now().After(deadline) {
								t.Fatal("controller did not exit")
							}
							time.Sleep(10 * time.Millisecond)
						}
						filesystemNormalizerExec(t, a, ref, "for p in ['/root1/alpha','/root1/beta']:\n for op in [open,os.stat]: deny(p,op)")
						var transient []filesystemKernelIdentity
						for _, id := range ids {
							if id.Name == "replacement" || id.Name == "normalize_name" {
								transient = append(transient, id)
							}
						}
						filesystemNormalizerGone(t, docker, image, transient)
					}
				}
				inner := filesystemInnerIdentities(a)
				if err := a.close(); err != nil {
					t.Fatal(err)
				}
				filesystemNormalizerGone(t, docker, image, inner)
			}
			retireFixtureFilesystemController(t, docker, c)
			filesystemNormalizerGone(t, docker, image, ids)
			t.Logf("%s: all captured controller identities retired", fault)
		})
	}
	t.Run("concurrent-distinct-roots", func(t *testing.T) {
		c, shared := filesystemNormalizerController(t, docker, image, "normal")
		roots := []string{filesystemNormalizerRoot(t), filesystemNormalizerRoot(t)}
		a := filesystemNormalizerAdmission(t, docker, c, roots, []fsfilter.Policy{filesystemNormalizerPolicy("alpha"), filesystemNormalizerPolicy("beta")})
		b := filesystemNormalizerAdmission(t, docker, c, roots, []fsfilter.Policy{filesystemNormalizerPolicy("beta"), filesystemNormalizerPolicy("alpha")})
		filesystemNormalizerIdentities(t, shared, "normal")
		first, second := filesystemInnerIdentities(a), filesystemInnerIdentities(b)
		for _, x := range first {
			for _, y := range second {
				if x.ID == y.ID {
					t.Fatal("shared fresh policy authority")
				}
			}
		}
		one, two := filesystemNormalizerConsumer(t, a, roots), filesystemNormalizerConsumer(t, b, roots)
		filesystemNormalizerExec(t, a, one, "deny('/root1/alpha',open)\ndeny('/root2/beta',open)\nassert open('/root1/beta').read()==open('/root2/alpha').read()=='fixture'")
		check := "deny('/root1/beta',open)\ndeny('/root2/alpha',open)\nassert open('/root1/alpha').read()==open('/root2/beta').read()=='fixture'"
		filesystemNormalizerExec(t, b, two, check)
		if err := a.close(); err != nil {
			t.Fatal(err)
		}
		filesystemNormalizerGone(t, docker, image, first)
		filesystemNormalizerExec(t, b, two, check)
		if err := b.close(); err != nil {
			t.Fatal(err)
		}
		filesystemNormalizerGone(t, docker, image, second)
		retireFixtureFilesystemController(t, docker, c)
		filesystemNormalizerGone(t, docker, image, shared)
	})
}
func filesystemInnerIdentities(a *filesystemAdmission) []filesystemKernelIdentity {
	var ids []filesystemKernelIdentity
	for _, m := range a.record.Ready.Maps {
		ids = append(ids, filesystemKernelIdentity{Kind: "map", Name: m.Name, ID: m.ID})
	}
	return ids
}

func filesystemNormalizerRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha", "beta"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("fixture"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func filesystemNormalizerPolicy(secret string) fsfilter.Policy {
	return fsfilter.Policy{Profile: fsfilter.Profile{Fold: true, Normalize: true}, Entries: []fsfilter.Entry{{Path: secret, Flags: fsfilter.Secret}}}
}

func filesystemNormalizerExec(t *testing.T, a *filesystemAdmission, ref runtime.DockerRef, script string) {
	t.Helper()
	prelude := "import errno,os\ndef deny(p,op):\n try: op(p)\n except OSError as e: assert e.errno==errno.EPERM,(p,e)\n else: raise AssertionError('allowed '+p)\n"
	if _, err := a.docker.ExecRead(t.Context(), ref, 4096, "python3", "-c", prelude+script); err != nil {
		t.Fatal(err)
	}
}

func filesystemNormalizerIdentities(t *testing.T, ids []filesystemKernelIdentity, fault string) {
	t.Helper()
	maps, guards := map[string]uint32{}, map[uint32]bool{}
	var extension, function uint32
	replacements, baseLinks := 0, 0
	for _, id := range ids {
		switch id.Kind {
		case "function":
			function = id.ID
		case "map":
			maps[id.Name] = id.ID
		case "program":
			if strings.HasPrefix(id.Name, "guard_") {
				guards[id.ID] = false
			} else if id.Name == "normalize_name" {
				extension = id.ID
			}
		}
	}
	for _, name := range []string{"work", "runes"} {
		if fault != "load-exit" && (maps["base/"+name] == 0 || maps["base/"+name] != maps["extension/"+name]) {
			t.Fatalf("extension did not reuse %s: %v", name, maps)
		}
	}
	for _, id := range ids {
		if id.Name == "replacement" {
			seen, exists := guards[id.Target]
			if extension == 0 || id.Program != extension || !exists || seen || id.Function != function {
				t.Fatalf("wrong replacement target: %+v", id)
			}
			guards[id.Target] = true
			replacements++
		} else if id.Name == "guard" {
			baseLinks++
		}
	}
	wantReplacements, wantBase := 27, 27
	if fault == "partial" {
		wantReplacements, wantBase = 13, 0
	}
	if fault == "load-exit" {
		wantReplacements, wantBase = 0, 0
	}
	if len(guards) != 27 || replacements != wantReplacements || baseLinks != wantBase {
		t.Fatalf("unexpected attachment shape: guards=%d replacements=%d base=%d", len(guards), replacements, baseLinks)
	}
}
