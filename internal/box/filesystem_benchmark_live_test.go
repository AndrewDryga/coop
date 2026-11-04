//go:build filesystemruntimee2e

package box

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/runtime"
)

// This opt-in measurement keeps every admission alive during the workload, so
// the concurrency rows include the cost of all attached LSM programs. Run it with a locally built,
// trusted clients image:
//
//	COOP_FILESYSTEM_BENCHMARK=1 COOP_FILESYSTEM_CLIENT_IMAGE=<cached-coop-clients-image> \
//	go test -tags filesystemruntimee2e ./internal/box -run '^TestFilesystemPerformanceRuntime$' -count=5 -timeout 30m -v
func TestFilesystemPerformanceRuntime(t *testing.T) {
	image := os.Getenv("COOP_FILESYSTEM_CLIENT_IMAGE")
	if os.Getenv("COOP_FILESYSTEM_BENCHMARK") != "1" || image == "" {
		t.Skip("requires explicit filesystem benchmark and installed client image")
	}
	for _, fixture := range []struct{ files, concurrency int }{{256, 1}, {256, 4}, {256, 8}, {10000, 1}} {
		concurrency := fixture.concurrency
		t.Run(fmt.Sprintf("files-%d/concurrency-%d", fixture.files, concurrency), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			type sample struct {
				AdmissionMS, WorkloadMS, CleanupMS int64
				RevalidateMS                       int64
				KernelMemory                       string
				Pins                               int
				Workload                           string
				Error                              string `json:",omitempty"`
			}
			rows := make([]sample, concurrency)
			admissions := make([]*filesystemAdmission, concurrency)
			roots := make([]string, concurrency)
			for i := range roots {
				root, err := filepath.EvalSymlinks(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				roots[i] = root
				for j := range fixture.files {
					name := filepath.Join(root, "public", fmt.Sprintf("%04d.txt", j))
					if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(name, bytes.Repeat([]byte("public data\n"), 100), 0644); err != nil {
						t.Fatal(err)
					}
				}
				for j := range 128 {
					name := filepath.Join(root, "nested/a/b/c/d/e/f/g", fmt.Sprintf("secret-%04d.pem", j))
					if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(name, []byte("inert benchmark secret"), 0644); err != nil {
						t.Fatal(err)
					}
				}
			}
			parallel := func(fn func(int)) {
				var wg sync.WaitGroup
				start := make(chan struct{})
				for i := range concurrency {
					wg.Add(1)
					go func() { defer wg.Done(); <-start; fn(i) }()
				}
				close(start)
				wg.Wait()
			}
			started := time.Now()
			parallel(func(i int) {
				begin := time.Now()
				a, err := prepareFilesystemAdmission(ctx, runtime.Runtime{Name: "docker"}, []string{roots[i]}, [][]string{nil}, nil)
				rows[i].AdmissionMS = time.Since(begin).Milliseconds()
				if err != nil {
					rows[i].Error = err.Error()
					return
				}
				admissions[i] = a
				t.Cleanup(func() {
					if _, err := os.Stat(a.dir); err == nil {
						// A failed close releases its private Docker binding. Retry
						// through a fresh binding to the recorded daemon before the
						// test state root (and its recovery receipt) is removed.
						_ = a.docker.Close()
						docker, err := runtime.BindDockerForSetup(context.Background(), runtime.Runtime{Name: "docker"}, a.record.Endpoint, a.record.DaemonID)
						if err != nil {
							t.Error(err)
							return
						}
						a.docker = docker
						if err := a.close(); err != nil {
							t.Error(err)
						}
					}
				})
				rows[i].Pins = a.record.Ready.Pins
			})
			admissionMS := time.Since(started).Milliseconds()
			parallel(func(i int) {
				a := admissions[i]
				if a == nil {
					return
				}
				begin := time.Now()
				if err := a.revalidate(); err != nil {
					rows[i].Error = err.Error()
				}
				rows[i].RevalidateMS = time.Since(begin).Milliseconds()
				args := []string{"--host", a.docker.Endpoint(), "run", "--rm", "--network", "none", "--read-only", "--user", "0", "--cap-drop", "ALL", "--cap-add", "SYS_PTRACE", "--pid=container:" + a.record.Helper.ID, "--cgroupns", "host", "--mount", "type=bind,src=/sys/fs/cgroup,dst=/cg,readonly", "--entrypoint", "python3", image, "-c", filesystemBenchmarkMemory}
				data, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
				rows[i].KernelMemory = string(data)
				if err != nil {
					rows[i].Error += fmt.Sprintf(" memory: %v", err)
				}
			})
			var ids []string
			for _, a := range admissions {
				if a != nil {
					ids = append(ids, a.record.Helper.ID)
				}
			}
			var memory string
			if len(ids) > 0 {
				endpoint := ""
				for _, a := range admissions {
					if a != nil {
						endpoint = a.docker.Endpoint()
						break
					}
				}
				args := append([]string{"--host", endpoint, "stats", "--no-stream", "--format", "{{json .}}"}, ids...)
				data, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
				if err != nil {
					t.Errorf("memory observation: %v: %s", err, data)
				}
				memory = string(data)
			}
			parallel(func(i int) {
				a := admissions[i]
				if a == nil {
					return
				}
				begin := time.Now()
				var output bytes.Buffer
				options := []string{"--network", "none", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--workdir", "/workspace", "--mount", networkMount("bind", roots[i], "/workspace", false)}
				id, _, err := a.docker.Image(ctx, image)
				if err == nil {
					var code int
					code, err = a.run(ctx, id, []string{"python3", "-c", filesystemBenchmarkWorkload}, options, nil, &output, &output, nil)
					if code != 0 && err == nil {
						err = fmt.Errorf("workload exit %d", code)
					}
				}
				rows[i].WorkloadMS, rows[i].Workload = time.Since(begin).Milliseconds(), output.String()
				if err != nil {
					rows[i].Error = err.Error()
				}
			})
			parallel(func(i int) {
				a := admissions[i]
				if a == nil {
					return
				}
				begin := time.Now()
				if err := a.close(); err != nil {
					rows[i].Error += " cleanup: " + err.Error()
				}
				rows[i].CleanupMS = time.Since(begin).Milliseconds()
				if _, err := os.Stat(a.dir); !os.IsNotExist(err) {
					rows[i].Error += " admission directory retained"
				}
			})
			data, err := json.Marshal(struct {
				Files                    int
				Concurrency              int
				AdmissionWallMS, TotalMS int64
				Memory                   string
				Samples                  []sample
			}{fixture.files, concurrency, admissionMS, time.Since(started).Milliseconds(), memory, rows})
			if err != nil {
				t.Fatal(err)
			}
			t.Log(string(data))
			for _, row := range rows {
				if row.Error != "" {
					t.Error(row.Error)
				}
			}
		})
	}
}

const filesystemBenchmarkMemory = `import glob,json,pathlib
supervisor=pathlib.Path('/proc/1')
children=sorted({pid for task in (supervisor/'task').iterdir() for pid in (task/'children').read_text().split()})
assert len(children)==1,'expected one C descriptor owner'
p=pathlib.Path('/proc')/children[0]
assert (p/'comm').read_text().strip()=='coop-fs-kernel','unexpected descriptor owner'
maps={}
for name in glob.glob(str(p/'fdinfo/*')):
 fields=dict(line.split(':',1) for line in pathlib.Path(name).read_text().splitlines() if ':' in line)
 if 'map_id' in fields: maps[fields['map_id'].strip()]={k:v.strip() for k,v in fields.items() if k in ['map_id','map_type','key_size','value_size','max_entries','map_flags','memlock','frozen']}
assert maps,'no BPF map descriptors observed'
cg=(p/'cgroup').read_text().split('0::',1)[1].splitlines()[0]
cg=pathlib.Path('/cg')/cg.lstrip('/')
print(json.dumps(dict(maps=list(maps.values()),smaps=(p/'smaps_rollup').read_text(),status=(p/'status').read_text(),supervisor_status=(supervisor/'status').read_text(),memory_current=(cg/'memory.current').read_text(),memory_peak=(cg/'memory.peak').read_text(),memory_stat=(cg/'memory.stat').read_text())))
`

const filesystemBenchmarkWorkload = `import errno,json,os,pathlib,time
root=pathlib.Path('public')
start=time.monotonic()
for repeat in range(10):
 for p in root.iterdir():
  assert p.stat().st_size==len(b'public data\n'*100)
  assert p.read_bytes()==b'public data\n'*100
read=time.monotonic()-start
start=time.monotonic()
for n in range(256):
 p=root/('write-'+str(n))
 p.write_bytes(b'ordinary output')
 q=p.with_name('renamed-'+str(n))
 p.rename(q)
 os.link(q,p)
 assert p.read_bytes()==b'ordinary output'
 p.unlink();q.unlink()
write=time.monotonic()-start
for p in ['nested/a/b/c/d/e/f/g/secret-0000.pem','NESTED/A/B/C/D/E/F/G/SECRET-0000.PEM']:
 try: open(p).read()
 except OSError as e: assert e.errno==errno.EPERM,(p,e)
 else: raise AssertionError('secret exposed')
print(json.dumps(dict(read_seconds=read,write_seconds=write,reads=len(list(root.iterdir()))*10,writes=256,denials=2)))
`
