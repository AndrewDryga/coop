//go:build filesystemruntimee2e

package box

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/fsfilter"
	"github.com/AndrewDryga/coop/internal/runtime"
	"go.yaml.in/yaml/v3"
)

func TestFilesystemAdmissionRuntime(t *testing.T) {
	if os.Getenv("COOP_FILESYSTEM_RUNTIME_TEST") != "1" {
		t.Skip("requires explicit local Docker filesystem qualification")
	}
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("inert fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	a, err := prepareFilesystemAdmission(ctx, runtime.Runtime{Name: "docker"}, []string{repo}, [][]string{nil}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("admitted %s with %d hooks and %d pinned objects", a.record.Parent, a.record.Ready.Programs, a.record.Ready.Pins)
	if err := a.revalidate(); err != nil {
		t.Error(err)
	}
	if err := a.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.dir); !os.IsNotExist(err) {
		t.Fatalf("retained admission after successful cleanup: %v", err)
	}
}

func TestFilesystemEmptyPolicyRuntime(t *testing.T) {
	if os.Getenv("COOP_FILESYSTEM_RUNTIME_TEST") != "1" {
		t.Skip("requires explicit local Docker filesystem qualification")
	}
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	a, err := prepareFilesystemAdmission(ctx, runtime.Runtime{Name: "docker"}, []string{repo}, [][]string{nil}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := a.close(); err != nil {
			t.Error(err)
		}
	}()
	if len(a.snapshots[0].policy.Entries) != 0 {
		t.Fatal("empty fixture acquired secret entries")
	}
	if err := a.revalidate(); err != nil {
		t.Fatal(err)
	}
}

func TestFilesystemServiceRuntime(t *testing.T) {
	image := os.Getenv("COOP_FILESYSTEM_CLIENT_IMAGE")
	if image == "" || os.Getenv("COOP_FILESYSTEM_RUNTIME_TEST") != "1" {
		t.Skip("requires an explicit installed client image and local Docker")
	}
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{".env": "inert fixture", "public": "public fixture", "long-secret-fixture.pem": "inert certificate fixture"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	script := `import errno,os,time
assert open('public').read()=='public fixture'
for p in ['.env','.ENV','LONG-SECRET-FIXTURE.PEM']:
 try: open(p).read()
 except OSError as e: assert e.errno==errno.EPERM,(p,e)
 else: raise AssertionError(p+' readable')
open('service-proof','w').write('guarded service works')
time.sleep(120)`
	doc := map[string]any{"services": map[string]any{"fixture": map[string]any{
		"image": image, "volumes": []string{".:/workspace"}, "working_dir": "/workspace",
		"command":     []string{"python3", "-c", script},
		"healthcheck": map[string]any{"test": []string{"CMD-SHELL", "test -f /workspace/service-proof"}, "interval": "1s", "timeout": "2s", "retries": 3},
	}}}
	data, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(repo, "compose.yml")
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	rt := runtime.Runtime{Name: "docker"}
	defer func() {
		if err := DownServicesFile(rt, repo, file, false, io.Discard, io.Discard); err != nil {
			t.Error(err)
		}
	}()
	var output bytes.Buffer
	if _, err := UpServicesReviewed(rt, repo, file, nil, &output, &output); err != nil {
		t.Fatalf("service start: %v\n%s", err, output.String())
	}
	proof, err := os.ReadFile(filepath.Join(repo, "service-proof"))
	if err != nil || string(proof) != "guarded service works" {
		t.Fatalf("service proof: %q %v", proof, err)
	}
	t.Log("actual Compose service and guarded shell healthcheck passed")
	state, err := serviceStateRoot("filesystem")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(state)
	if err != nil {
		t.Fatal(err)
	}
	var record filesystemRecord
	for _, entry := range entries {
		candidate, err := readFilesystemRecord(filepath.Join(state, entry.Name()))
		if err == nil && candidate.Workspace == repo && candidate.Services {
			record = candidate
		}
	}
	if record.Parent == "" {
		t.Fatal("no retained filesystem admission")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	docker, err := runtime.BindDockerForSetup(ctx, rt, record.Endpoint, record.DaemonID)
	if err != nil {
		t.Fatal(err)
	}
	defer docker.Close()
	service := record.Consumers[len(record.Consumers)-1]
	if err := docker.StopContainer(ctx, service, 1); err != nil {
		t.Fatal(err)
	}
	if removed, err := RecoverFilesystemAdmissions(ctx, rt, repo); err != nil || removed != 0 {
		t.Fatalf("stopped service lost protection: %d %v", removed, err)
	}
	if code, err := rt.Run(nil, io.Discard, io.Discard, "--host", record.Endpoint, "start", service.ID); err != nil || code != 0 {
		t.Fatalf("restart: %d %v", code, err)
	}
	if _, err := docker.ExecRead(ctx, service, 1024, "python3", "-c", `import errno
assert open('/workspace/public').read()=='public fixture'
try: open('/workspace/.ENV').read()
except OSError as e: assert e.errno==errno.EPERM
else: raise AssertionError('restart exposed secret')`); err != nil {
		t.Fatal("restart proof", err)
	}
	t.Log("stopped consumer retained protection; restart kept case denial")
	if err := docker.StopContainer(ctx, record.Helper, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := docker.ExecRead(ctx, service, 1024, "python3", "-c", `import errno
for p in ['/workspace/public','/workspace/.env']:
 try: open(p).read()
 except OSError as e: assert e.errno==errno.EPERM,(p,e)
 else: raise AssertionError('helper death exposed '+p)`); err != nil {
		t.Fatal("owner death proof", err)
	}
	t.Log("helper loss denied all reads on the guarded share; teardown will reconcile retained pins")
}

func TestFilesystemServiceGrantsRuntime(t *testing.T) {
	image := os.Getenv("COOP_FILESYSTEM_CLIENT_IMAGE")
	if image == "" || os.Getenv("COOP_FILESYSTEM_RUNTIME_TEST") != "1" {
		t.Skip("requires an explicit installed client image and local Docker")
	}
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"tls.key": "inert granted fixture"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	services := map[string]any{}
	for _, name := range []string{"granted", "ordinary"} {
		volumes := []string{".:/workspace:ro"}
		if name == "granted" {
			volumes = append(volumes, "./tls.key:/key:ro")
		}
		services[name] = map[string]any{
			"image": image, "volumes": volumes, "command": []string{"sleep", "120"},
			"healthcheck": map[string]any{"test": []string{"CMD", "true"}, "interval": "1s", "timeout": "2s", "retries": 3},
		}
	}
	data, err := yaml.Marshal(map[string]any{"services": services})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(repo, "compose.yml")
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	rt := runtime.Runtime{Name: "docker"}
	review, err := ReviewServiceStart(repo, file, rt, true)
	if err != nil || review.Secrets == nil {
		t.Fatalf("secret review: %v", err)
	}
	if len(review.Secrets.Files) != 1 || review.Secrets.Files[0].Path != "tls.key" {
		t.Fatalf("unexpected approval scope: %+v", review.Secrets.Files)
	}
	if err := review.Secrets.Approve(); err != nil {
		t.Fatal(err)
	}
	// Approval covers the reviewed path in every readonly bind in this Compose
	// project. A new secret added after that review must not inherit the grant.
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("inert ungranted fixture"), 0644); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := DownServicesFile(rt, repo, file, false, io.Discard, io.Discard); err != nil {
			t.Error(err)
		}
	}()
	var output bytes.Buffer
	if _, err := UpServicesReviewed(rt, repo, file, review, &output, &output); err != nil {
		t.Fatalf("approved service start: %v\n%s", err, output.String())
	}
	state, err := serviceStateRoot("filesystem")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(state)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, entry := range entries {
		r, err := readFilesystemRecord(filepath.Join(state, entry.Name()))
		if err != nil || r.Workspace != repo || !r.Services {
			continue
		}
		docker, err := runtime.BindDockerForSetup(t.Context(), rt, r.Endpoint, r.DaemonID)
		if err != nil {
			t.Fatal(err)
		}
		defer docker.Close()
		script := `import errno,sys
for p in ['/workspace/.env','/workspace/.ENV']:
 try: open(p).read()
 except OSError as e: assert e.errno==errno.EPERM,(p,e)
 else: raise AssertionError(p+' exposed')
for p in ['/workspace/tls.key','/workspace/TLS.KEY']:
 assert open(p).read()=='inert granted fixture'
if sys.argv[1]=='granted':
 assert open('/key').read()=='inert granted fixture'
 try: open('/key','w')
 except OSError as e: assert e.errno in [errno.EPERM,errno.EROFS],e
 else: raise AssertionError('read grant allowed a write')`
		consumer := r.Consumers[len(r.Consumers)-1]
		if evidence, err := docker.ExecRead(t.Context(), consumer, 4096, "python3", "-c", "import json,sys\ntry:\n exec("+strconv.Quote(script)+")\nexcept Exception as e:\n print(json.dumps({'failure': str(e)}))\nelse:\n print('GRANT_OK')", r.Service); err != nil || !bytes.Contains(evidence, []byte("GRANT_OK")) {
			t.Fatalf("%s: %v\n%s", r.Service, err, evidence)
		}
		checked++
	}
	if checked != 2 {
		t.Fatalf("expected two protected services, checked %d", checked)
	}
	t.Log("both reviewed readonly services can read the granted identity; newly added secrets stay denied and the grant cannot write")
}

func TestFilesystemBoxRunRuntime(t *testing.T) {
	image := os.Getenv("COOP_FILESYSTEM_CLIENT_IMAGE")
	if image == "" || os.Getenv("COOP_FILESYSTEM_RUNTIME_TEST") != "1" {
		t.Skip("requires an explicit installed client image and local Docker")
	}
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{".env": "inert fixture", "public": "public fixture"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	var output bytes.Buffer
	cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", BaseImage: image, Egress: "none", NoNewPrivileges: true, Memory: "256m", Pids: "64", CPUs: "1"}
	code, err := Run(cfg, runtime.Runtime{Name: "docker"}, RunSpec{Ctx: ctx, Repo: repo, Image: image, Workdir: "/workspace", Batch: true, Quiet: true, Stdout: &output, Stderr: &output, Cmd: []string{"python3", "-c", `import errno,os
assert open('public').read()=='public fixture'
for p in ['.env','.ENV']:
 try: open(p).read()
 except OSError as e: assert e.errno==errno.EPERM,(p,e)
 else: raise AssertionError(p+' readable')
open('edited','w').write('works')
os.link('public','public-link')
assert open('public-link').read()=='public fixture'
print('guarded box works')`}})
	if code != 0 || err != nil {
		t.Fatalf("run code %d: %v\n%s", code, err, output.String())
	}
	if !bytes.Contains(output.Bytes(), []byte("guarded box works")) {
		t.Fatalf("no workload proof: %s", output.String())
	}
	t.Log(output.String())
}

func TestFilesystemServiceReuseRuntime(t *testing.T) {
	image := os.Getenv("COOP_FILESYSTEM_CLIENT_IMAGE")
	if image == "" || os.Getenv("COOP_FILESYSTEM_RUNTIME_TEST") != "1" {
		t.Skip("requires explicit local Docker qualification")
	}
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{".env": "inert secret", "public": "initial public"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	doc := map[string]any{"services": map[string]any{"fixture": map[string]any{
		"image": image, "volumes": []string{".:/workspace:ro"}, "working_dir": "/workspace", "command": []string{"sleep", "120"},
		"healthcheck": map[string]any{"test": []string{"CMD-SHELL", "test -r /workspace/public"}, "interval": "1s", "timeout": "2s", "retries": 3},
	}}}
	data, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(repo, "compose.yml")
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	rt := runtime.Runtime{Name: "docker"}
	defer func() {
		if err := DownServicesFile(rt, repo, file, false, io.Discard, io.Discard); err != nil {
			t.Error(err)
		}
	}()
	var starts []time.Time
	start := func() filesystemRecord {
		t.Helper()
		var output bytes.Buffer
		if _, err := UpServicesReviewed(rt, repo, file, nil, &output, &output); err != nil {
			t.Fatalf("up: %v\n%s", err, output.String())
		}
		state, err := serviceStateRoot("filesystem")
		if err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(state)
		if err != nil {
			t.Fatal(err)
		}
		var records []filesystemRecord
		for _, entry := range entries {
			r, err := readFilesystemRecord(filepath.Join(state, entry.Name()))
			if err == nil && r.Workspace == repo && r.Services {
				records = append(records, r)
			}
		}
		if len(records) != 1 {
			t.Fatalf("expected one retained admission, got %d", len(records))
		}
		docker, err := runtime.BindDockerForSetup(t.Context(), rt, records[0].Endpoint, records[0].DaemonID)
		if err != nil {
			t.Fatal(err)
		}
		defer docker.Close()
		value, present, err := docker.InspectContainer(t.Context(), records[0].Consumers[len(records[0].Consumers)-1])
		if err != nil || !present || !value.State.Running {
			t.Fatalf("service is not running: %v", err)
		}
		starts = append(starts, value.State.StartedAt)
		return records[0]
	}
	first := start()
	if err := os.WriteFile(filepath.Join(repo, "public-next"), []byte("edited public"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(repo, "public-next"), filepath.Join(repo, "public")); err != nil {
		t.Fatal(err)
	}
	second := start()
	if second.Parent != first.Parent || second.Consumers[len(second.Consumers)-1].ID != first.Consumers[len(first.Consumers)-1].ID || !starts[0].Equal(starts[1]) {
		t.Fatal("ordinary public edit restarted unchanged service")
	}
	t.Log("second up reused exact consumer and helper after an ordinary host atomic save")
	if err := os.WriteFile(filepath.Join(repo, ".env-next"), []byte("replaced inert secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(repo, ".env-next"), filepath.Join(repo, ".env")); err != nil {
		t.Fatal(err)
	}
	third := start()
	if third.Parent == second.Parent {
		t.Fatal("changed protected identity reused old admission")
	}
	t.Log("changed protected identity re-admitted the service and retired its old controller")
	docker, err := runtime.BindDockerForSetup(t.Context(), rt, third.Endpoint, third.DaemonID)
	if err != nil {
		t.Fatal(err)
	}
	defer docker.Close()
	if err := docker.RemoveContainer(t.Context(), third.Controller.Container); err != nil {
		t.Fatal(err)
	}
	fourth := start()
	if fourth.Parent == third.Parent || fourth.Controller.Name == third.Controller.Name {
		t.Fatal("dead controller reused instead of replacing the service admission")
	}
	t.Log("controller death replaced the service under a fresh protected generation")
}

func TestFilesystemMixedServicesRuntime(t *testing.T) {
	image := os.Getenv("COOP_FILESYSTEM_CLIENT_IMAGE")
	if image == "" || os.Getenv("COOP_FILESYSTEM_RUNTIME_TEST") != "1" {
		t.Skip("requires explicit local Docker qualification")
	}
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{".env": "inert secret", "public": "public fixture"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	services := map[string]any{}
	for _, name := range []string{"alpha", "beta"} {
		services[name] = map[string]any{
			"image": image, "volumes": []string{".:/workspace:ro"}, "working_dir": "/workspace", "command": []string{"sleep", "300"},
			"healthcheck": map[string]any{"test": []string{"CMD-SHELL", "test -r /workspace/public"}, "interval": "1s", "timeout": "2s", "retries": 1},
		}
	}
	file := filepath.Join(repo, "compose.yml")
	write := func() {
		t.Helper()
		data, err := yaml.Marshal(map[string]any{"services": services})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	rt := runtime.Runtime{Name: "docker"}
	defer func() {
		if err := DownServicesFile(rt, repo, file, false, io.Discard, io.Discard); err != nil {
			t.Error(err)
		}
	}()
	start := func() error {
		var output bytes.Buffer
		_, err := UpServicesReviewed(rt, repo, file, nil, &output, &output)
		t.Log(output.String())
		return err
	}
	if err := start(); err != nil {
		t.Fatal(err)
	}
	read := func() map[string]filesystemRecord {
		t.Helper()
		state, err := serviceStateRoot("filesystem")
		if err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(state)
		if err != nil {
			t.Fatal(err)
		}
		records := map[string]filesystemRecord{}
		for _, entry := range entries {
			r, err := readFilesystemRecord(filepath.Join(state, entry.Name()))
			if err != nil {
				t.Logf("read admission %s: %v", entry.Name(), err)
			}
			if err == nil && r.Workspace == repo && r.Services {
				if _, duplicate := records[r.Service]; duplicate {
					t.Fatalf("duplicate retained admission for %s", r.Service)
				}
				records[r.Service] = r
			}
		}
		return records
	}
	first := read()
	if len(first) != 2 {
		t.Fatalf("initial service admissions: %d", len(first))
	}
	docker, err := runtime.BindDockerForSetup(t.Context(), rt, first["beta"].Endpoint, first["beta"].DaemonID)
	if err != nil {
		t.Fatal(err)
	}
	defer docker.Close()
	consumer := func(r filesystemRecord) runtime.DockerRef { return r.Consumers[len(r.Consumers)-1] }
	beta := consumer(first["beta"])
	if err := docker.StopContainer(t.Context(), beta, 1); err != nil {
		t.Fatal(err)
	}
	if removed, err := RecoverFilesystemAdmissions(t.Context(), rt, repo); err != nil || removed != 0 {
		t.Fatalf("stopped service admission lost: %d %v", removed, err)
	}
	services["alpha"].(map[string]any)["environment"] = map[string]string{"FIXTURE_REVISION": "2"}
	write()
	if err := start(); err != nil {
		t.Fatal(err)
	}
	second := read()
	if len(second) != 2 || second["alpha"].Parent == first["alpha"].Parent || second["beta"].Parent != first["beta"].Parent || consumer(second["beta"]).ID != beta.ID {
		t.Fatal("mixed service start failed to replace only changed alpha and reuse stopped beta")
	}
	for _, r := range second {
		if _, err := docker.ExecRead(t.Context(), consumer(r), 1024, "python3", "-c", `import errno
assert open('/workspace/public').read()=='public fixture'
try: open('/workspace/.ENV').read()
except OSError as e: assert e.errno==errno.EPERM
else: raise AssertionError('secret exposed')`); err != nil {
			t.Fatal(err)
		}
	}
	t.Log("changed alpha replaced; stopped beta reused and restarted with secret protection intact")
	services["alpha"].(map[string]any)["healthcheck"].(map[string]any)["test"] = []string{"CMD-SHELL", "false"}
	write()
	if err := start(); err == nil {
		t.Fatal("unhealthy replacement accepted")
	}
	value, present, err := docker.InspectContainer(t.Context(), beta)
	if err != nil || !present || !value.State.Running {
		t.Fatalf("failed replacement destroyed reused sibling: %t %v", present, err)
	}
	if _, err := RecoverFilesystemAdmissions(t.Context(), rt, repo); err != nil {
		t.Fatal(err)
	}
	remaining := read()
	if len(remaining) != 1 || remaining["beta"].Parent != first["beta"].Parent {
		t.Fatalf("failed start retained unexpected service admissions: %d", len(remaining))
	}
	t.Log("failed alpha healthcheck cleaned its new admission while reused beta remained running")
}

// The child retains the real admission lease and attached Docker client until
// the parent kills it. No synthetic stale-PID record stands in for a host crash.
func TestFilesystemCrashOwnerRuntime(t *testing.T) {
	repo := os.Getenv("COOP_FILESYSTEM_CRASH_REPO")
	if repo == "" || os.Getenv("COOP_FILESYSTEM_RUNTIME_TEST") != "1" {
		t.Skip("child of the filesystem crash test")
	}
	a, err := prepareFilesystemAdmission(t.Context(), runtime.Runtime{Name: "docker"}, []string{repo}, [][]string{nil}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := a.close(); err != nil {
			t.Error(err)
		}
	}()
	options := []string{"--network", "none", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--mount", networkMount("bind", repo, "/workspace", false)}
	image, _, err := a.docker.Image(t.Context(), os.Getenv("COOP_FILESYSTEM_CLIENT_IMAGE"))
	if err != nil {
		t.Fatal(err)
	}
	code, err := a.run(t.Context(), image, []string{"sleep", "120"}, options, nil, io.Discard, io.Discard, func() error {
		return os.WriteFile(os.Getenv("COOP_FILESYSTEM_CRASH_READY"), []byte(a.dir), 0600)
	})
	t.Fatalf("crash fixture unexpectedly returned: %d %v", code, err)
}

func TestFilesystemHostCrashRecoveryRuntime(t *testing.T) {
	if os.Getenv("COOP_FILESYSTEM_RUNTIME_TEST") != "1" || os.Getenv("COOP_FILESYSTEM_CLIENT_IMAGE") == "" {
		t.Skip("requires explicit local Docker qualification")
	}
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("inert crash fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(t.TempDir(), "ready")
	log, err := os.Create(filepath.Join(t.TempDir(), "child.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	child := exec.Command(os.Args[0], "-test.run=^TestFilesystemCrashOwnerRuntime$", "-test.v")
	child.Env = append(os.Environ(), "COOP_FILESYSTEM_CRASH_REPO="+repo, "COOP_FILESYSTEM_CRASH_READY="+ready)
	child.Stdout, child.Stderr = log, log
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if child.ProcessState == nil {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := RecoverFilesystemAdmissions(ctx, runtime.Runtime{Name: "docker"}, repo); err != nil {
			t.Error(err)
		}
	}()
	var dir string
	deadline := time.NewTimer(time.Minute)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
waitReady:
	for {
		select {
		case <-deadline.C:
			data, _ := os.ReadFile(log.Name())
			t.Fatalf("child admission timed out: %s", data)
		case <-tick.C:
			data, err := os.ReadFile(ready)
			if err == nil && len(data) > 0 {
				dir = string(data)
				break waitReady
			}
		}
	}
	record, err := readFilesystemRecord(dir)
	if err != nil {
		t.Fatal(err)
	}
	rt := runtime.Runtime{Name: "docker"}
	if n, err := RecoverFilesystemAdmissions(t.Context(), rt, repo); err != nil || n != 0 {
		t.Fatalf("live supervisor was reaped: %d %v", n, err)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := child.Wait(); err == nil {
		t.Fatal("child exited normally instead of SIGKILL")
	}
	docker, err := runtime.BindDockerForSetup(t.Context(), rt, record.Endpoint, record.DaemonID)
	if err != nil {
		t.Fatal(err)
	}
	defer docker.Close()
	consumer := record.Consumers[len(record.Consumers)-1]
	value, present, err := docker.InspectContainer(t.Context(), consumer)
	if err != nil || !present || !value.State.Running {
		t.Fatalf("crash did not leave a running consumer: %v", err)
	}
	output, err := docker.ExecRead(t.Context(), consumer, 1024, "python3", "-c", `import errno
try: open('/workspace/.ENV').read()
except OSError as e: assert e.errno==errno.EPERM,e
else: raise AssertionError('secret readable after host crash')`)
	if err != nil {
		t.Fatalf("protection after host crash: %v %s", err, output)
	}
	if n, err := RecoverFilesystemAdmissions(t.Context(), rt, repo); err != nil || n != 1 {
		t.Fatalf("crashed admission recovery: %d %v", n, err)
	}
	for _, ref := range append(record.Consumers, record.Helper) {
		if _, present, err := docker.InspectContainer(t.Context(), ref); err != nil || present {
			t.Fatalf("retained %s after recovery: %v", ref.Name, err)
		}
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("retained state after recovery: %v", err)
	}
	t.Log("SIGKILL of the real host supervisor kept the running consumer protected; exact recovery removed consumers, helper and kernel attachments")
}

func TestFilesystemPolicyOperationsRuntime(t *testing.T) {
	image := os.Getenv("COOP_FILESYSTEM_CLIENT_IMAGE")
	if image == "" || os.Getenv("COOP_FILESYSTEM_RUNTIME_TEST") != "1" {
		t.Skip("requires explicit local Docker qualification")
	}
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repo, "private"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{".env": "inert fixture", "public": "public fixture", "private/key": "inert fixture", "Café": "inert Unicode fixture", ".coopignore": "private/\nCafé\n"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	for target, alias := range map[string]string{".env": "a-secret-alias", ".coopignore": "policy-alias"} {
		if err := os.Link(filepath.Join(repo, target), filepath.Join(repo, alias)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("public", filepath.Join(repo, "hidden-link.pem")); err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile("testdata/filesystem_workflows.py")
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", BaseImage: image, Egress: "none", NoNewPrivileges: true, Memory: "256m", Pids: "64", CPUs: "1"}
	code, err := Run(cfg, runtime.Runtime{Name: "docker"}, RunSpec{Ctx: t.Context(), Repo: repo, Image: image, Workdir: "/workspace", Batch: true, Quiet: true, Stdout: &output, Stderr: &output, Cmd: []string{"python3", "-c", string(script)}})
	if err != nil || code != 0 {
		t.Fatalf("policy workflows: %d %v\n%s", code, err, output.String())
	}
	t.Log(output.String())
}

func TestFilesystemBootGuardRuntime(t *testing.T) {
	image := os.Getenv("COOP_FILESYSTEM_CLIENT_IMAGE")
	if image == "" || os.Getenv("COOP_FILESYSTEM_RUNTIME_TEST") != "1" {
		t.Skip("requires explicit local Docker qualification")
	}
	for _, mode := range []string{"guard", "healthcheck"} {
		t.Run(mode, func(t *testing.T) {
			repo, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			a, err := prepareFilesystemAdmission(t.Context(), runtime.Runtime{Name: "docker"}, []string{repo}, [][]string{nil}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := a.close(); err != nil {
					t.Error(err)
				}
			}()
			command := []string{"sh", "-c", "echo executed > /workspace/unexpected"}
			control, err := a.writeGuard(t.Context(), fsfilter.ExecutionManifest{Command: command, Healthcheck: command})
			if err != nil {
				t.Fatal(err)
			}
			stale := fsfilter.ExecutionManifest{BootID: "00000000-0000-0000-0000-000000000000", Command: command, Healthcheck: command}
			data, err := json.Marshal(stale)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(filepath.Join(control, "manifest.json"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(control, "manifest.json"), data, 0444); err != nil {
				t.Fatal(err)
			}
			ref := filesystemRef(a.record.Parent, "stale-boot")
			if err := a.consumer(ref); err != nil {
				t.Fatal(err)
			}
			options := []string{"--network", "none", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--cgroup-parent", a.record.cgroupParent(), "--entrypoint", fsfilter.ControlPath + "/guard", "--mount", networkMount("bind", control, fsfilter.ControlPath, true), "--mount", networkMount("bind", repo, "/workspace", false)}
			imageID, _, err := a.docker.Image(t.Context(), image)
			if err != nil {
				t.Fatal(err)
			}
			ref.ID, err = a.create(t.Context(), runtime.DockerCreate{Ref: ref, Image: imageID, Options: options, Command: []string{mode, fsfilter.ControlPath + "/manifest.json"}})
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			code, err := a.docker.StartAttached(t.Context(), ref, nil, &output, &output, nil)
			if err != nil || code == 0 || !bytes.Contains(output.Bytes(), []byte("re-admission after a runtime restart")) {
				t.Fatalf("wrong-boot %s: %d %v %s", mode, code, err, output.String())
			}
			if _, err := os.Stat(filepath.Join(repo, "unexpected")); !os.IsNotExist(err) {
				t.Fatal("image command ran before boot guard", err)
			}
			t.Log("stale boot refused before executing image code")
		})
	}
}

// A host rename can replace a pathname already warm in the guest cache. Sparse
// final identity checks are safe only if those fresh opens see the pinned object.
func TestFilesystemWarmedPublicAliasesRuntime(t *testing.T) {
	image := os.Getenv("COOP_FILESYSTEM_CLIENT_IMAGE")
	if image == "" || os.Getenv("COOP_FILESYSTEM_RUNTIME_TEST") != "1" {
		t.Skip("requires an explicit installed client image and local Docker")
	}
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(repo, 0777); err != nil {
		t.Fatal(err)
	}
	var grants []string
	targets := map[string]string{}
	for i := range 3 {
		// Distinct protected inodes keep a successful read from refreshing the
		// object used by a later write-first or truncate-first check.
		policyDir := fmt.Sprintf("policy-source-%d", i)
		if err := os.Mkdir(filepath.Join(repo, policyDir), 0755); err != nil {
			t.Fatal(err)
		}
		grant := fmt.Sprintf(".env.granted-%d", i)
		grants = append(grants, grant)
		for role, target := range map[string]string{"secret": fmt.Sprintf(".env.secret-%d", i), "policy": policyDir + "/.coopignore", "grant": grant} {
			body := map[string]string{"secret": "inert secret", "policy": "# inert policy\n", "grant": "inert grant"}[role]
			if err := os.WriteFile(filepath.Join(repo, target), []byte(body), 0666); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(filepath.Join(repo, target), 0666); err != nil {
				t.Fatal(err)
			}
			targets[fmt.Sprintf("%s-%d", role, i)] = target
		}
		for _, role := range []string{"secret", "policy", "grant", "public"} {
			if err := os.WriteFile(filepath.Join(repo, fmt.Sprintf("%s-%d", role, i)), []byte("public"), 0666); err != nil {
				t.Fatal(err)
			}
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	a, err := prepareFilesystemAdmission(ctx, runtime.Runtime{Name: "docker"}, []string{repo}, [][]string{grants}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := a.close(); err != nil {
			t.Error(err)
		}
	}()
	image, _, err = a.docker.Image(ctx, image)
	if err != nil {
		t.Fatal(err)
	}
	script := `import errno,os,time
for i in range(3):
 for role in ['secret','policy','grant','public']:
  p=f'{role}-{i}'
  assert open(p).read()=='public'
  with open(p,'r+') as f: f.write('public')
open('warmed','w').close()
while not os.path.exists('replaced'): time.sleep(.02)
for i in range(3):
 for role,want in [('secret',None),('policy','# inert policy\n'),('grant','inert grant'),('public','public')]:
  p=f'{role}-{i}'
  def attempt():
   if i==0: return open(p).read()
   if i==1:
    with open(p,'r+') as f: f.write('public')
   else: os.truncate(p,0)
  # The host share can briefly report a renamed pathname missing, including
  # without BPF. Absence is not proof: require the actual permission result.
  began=time.monotonic(); deadline=began+2; missing=0
  try:
   while True:
    try: data=attempt(); break
    except FileNotFoundError:
     missing+=1
     if time.monotonic()>=deadline: raise
     time.sleep(.01)
  except OSError as e:
   assert (want is None if i==0 else role!='public') and e.errno==errno.EPERM,(p,e)
  else:
   if i==0: assert want is not None and data==want,(p,data)
   else: assert role=='public',p+' writable'
  if missing: print('rename visibility:',p,missing,'missing attempts',time.monotonic()-began,'seconds')
print('all warmed aliases enforced')`
	var output bytes.Buffer
	done := make(chan error, 1)
	go func() {
		code, err := a.run(ctx, image, []string{"python3", "-c", script}, []string{"--network", "none", "--user", "1000:1000", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--workdir", "/workspace", "--mount", networkMount("bind", repo, "/workspace", false)}, nil, &output, &output, nil)
		if code != 0 && err == nil {
			err = fmt.Errorf("alias probe exited %d", code)
		}
		done <- err
	}()
	// Always join the attach before teardown or reading its output.
	defer func() {
		cancel()
		if done != nil {
			<-done
		}
	}()
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	for {
		if _, err := os.Stat(filepath.Join(repo, "warmed")); err == nil {
			break
		}
		select {
		case err := <-done:
			done = nil
			t.Fatalf("warmup: %v\n%s", err, output.String())
		case <-deadline.C:
			t.Fatal("warmup timed out")
		case <-time.After(20 * time.Millisecond):
		}
	}
	for name, target := range targets {
		tmp := filepath.Join(repo, "replacement")
		if err := os.Link(filepath.Join(repo, target), tmp); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, filepath.Join(repo, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "replaced"), nil, 0666); err != nil {
		t.Fatal(err)
	}
	err = <-done
	done = nil
	if err != nil {
		t.Fatalf("warmed alias enforcement: %v\n%s", err, output.String())
	}
	t.Log(output.String())
}
