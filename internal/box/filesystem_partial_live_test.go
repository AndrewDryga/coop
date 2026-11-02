//go:build filesystemruntimee2e

package box

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/fsfilter"
	"github.com/AndrewDryga/coop/internal/fsimage"
	"github.com/AndrewDryga/coop/internal/randid"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// The fixture changes only the test image: stop the loader after its first
// durable pin. Production has no fault-injection environment or runtime knob.
func partialFilesystemImage(t *testing.T, docker *runtime.Docker) string {
	t.Helper()
	source, err := fsimage.Context()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	w := tar.NewWriter(&out)
	r := tar.NewReader(bytes.NewReader(source))
	changed := false
	for {
		h, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		if h.Name == "internal/fsfilter/assets/loader.c" {
			marker := `if (snprintf(path, PATH_MAX, "%s/%s", pin_directory, stage) >= PATH_MAX || bpf_link__pin(link, path)) goto fail;`
			if bytes.Count(data, []byte(marker)) != 1 {
				t.Fatal("partial-attach injection point changed")
			}
			injection := `
        if (nattached == 1) {
            if (getenv("COOP_TEST_UNKNOWN_PIN")) {
                char unexpected[PATH_MAX];
                if (snprintf(unexpected, sizeof(unexpected), "%s/unexpected", pin_directory) >= PATH_MAX || bpf_obj_pin(bpf_link__fd(link), unexpected)) goto fail;
            }
            int marker = open("/state/partial-ready", O_CREAT|O_EXCL|O_WRONLY|O_CLOEXEC, 0600);
            if (marker < 0 || close(marker)) goto fail;
            for (;;) pause();
        }`
			data = bytes.Replace(data, []byte(marker), []byte(marker+injection), 1)
			changed = true
		}
		h.Size = int64(len(data))
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if !changed {
		t.Fatal("missing loader source")
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(filepath.Join(t.TempDir(), "build.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	image, err := docker.BuildImage(t.Context(), runtime.DockerBuild{Tag: "coop-filesystem-test:" + randid.Hex(16), Platform: "linux/" + runtime.Architecture(docker.Info().Architecture), Labels: map[string]string{"coop.filesystem.test": "partial-attachment"}}, out.Bytes(), log, log)
	if err != nil {
		data, _ := os.ReadFile(log.Name())
		if len(data) > 8000 {
			data = data[len(data)-8000:]
		}
		t.Fatalf("fixture image: %v\n%s", err, data)
	}
	binary, endpoint := docker.Binary(), docker.Endpoint()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if data, err := exec.CommandContext(ctx, binary, "--host", endpoint, "image", "rm", image.ID).CombinedOutput(); err != nil {
			t.Errorf("fixture image cleanup: %v %s", err, data)
		}
	})
	return image.ID
}

func TestFilesystemPartialAttachmentRecoveryRuntime(t *testing.T) {
	if os.Getenv("COOP_FILESYSTEM_RUNTIME_TEST") != "1" || os.Getenv("COOP_FILESYSTEM_CLIENT_IMAGE") == "" {
		t.Skip("requires explicit local Docker qualification")
	}
	docker, err := runtime.BindDocker(t.Context(), runtime.Runtime{Name: "docker"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer docker.Close()
	image := partialFilesystemImage(t, docker)
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "partial", true: "unexpected-pin"}[unknown], func(t *testing.T) {
			repo, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("inert fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			parent := "coop-fs-" + randid.Hex(16)
			binding, err := runtime.BindDockerForSetup(t.Context(), runtime.Runtime{Name: "docker"}, docker.Endpoint(), docker.Info().ID)
			if err != nil {
				t.Fatal(err)
			}
			a := &filesystemAdmission{docker: binding, dir: dir, record: filesystemRecord{Version: 1, Parent: parent, Endpoint: binding.Endpoint(), DaemonID: binding.Info().ID, Image: image, Helper: filesystemRef(parent, "owner"), Workspace: repo, Supervisor: supervisorLabelValue(workspaceScope(repo), os.Getpid())}}
			a.lease, err = lockFilesystemAdmission(dir, true)
			if err != nil {
				t.Fatal(err)
			}
			if err := a.persist(); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if _, err := os.Stat(dir); os.IsNotExist(err) {
					return
				}
				// Retry through a fresh binding after the deliberate uncertain cleanup.
				next, err := runtime.BindDockerForSetup(context.Background(), runtime.Runtime{Name: "docker"}, a.record.Endpoint, a.record.DaemonID)
				if err != nil {
					t.Error(err)
					return
				}
				a.docker = next
				if err := a.close(); err != nil {
					t.Error(err)
				}
			}()
			policy := fsfilter.Admission{Roots: []fsfilter.Policy{{Profile: fsfilter.Profile{Fold: true, Normalize: true}, Entries: []fsfilter.Entry{{Path: ".env", Flags: fsfilter.Secret}}}}}
			if err := filesystemJSON(filepath.Join(dir, "policy.json"), policy); err != nil {
				t.Fatal(err)
			}
			options := append(a.helperOptions(false), "--mount", networkMount("bind", repo, "/sources/1", true))
			if unknown {
				options = append(options, "-e", "COOP_TEST_UNKNOWN_PIN=1")
			}
			if _, err := a.create(t.Context(), runtime.DockerCreate{Ref: a.record.Helper, Image: image, Options: options, Command: []string{"admit", "/sources", "/state/policy.json", "/host-cgroup/" + parent}}); err != nil {
				t.Fatal(err)
			}
			if err := a.docker.StartContainer(t.Context(), a.record.Helper); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(time.Minute)
			for {
				if _, err := os.Stat(filepath.Join(dir, "partial-ready")); err == nil {
					break
				}
				if time.Now().After(deadline) {
					log, _ := a.docker.ContainerLogs(t.Context(), a.record.Helper, 30, 1<<20)
					t.Fatalf("partial-attach barrier not reached: %s", log)
				}
				time.Sleep(25 * time.Millisecond)
			}
			var birth fsfilter.Birth
			data, err := os.ReadFile(filepath.Join(dir, "birth.json"))
			if err != nil || json.Unmarshal(data, &birth) != nil || birth.Validate(parent) != nil {
				t.Fatalf("missing complete birth before first pin: %s %v", data, err)
			}
			log, err := a.docker.ContainerLogs(t.Context(), a.record.Helper, 30, 1<<20)
			if err != nil || bytes.Contains(log, []byte(`"status":"ready"`)) {
				t.Fatalf("fixture reached READY: %s %v", log, err)
			}
			if err := a.docker.StopContainer(t.Context(), a.record.Helper, 0); err != nil {
				t.Fatal(err)
			}
			err = a.close()
			if !unknown {
				if err != nil {
					t.Fatal(err)
				}
				t.Log("killed loader after first durable pin; birth receipt reconciled partial attachment")
				return
			}
			if err == nil {
				t.Fatal("unexpected pin was removed without authority")
			}
			if _, err := os.Stat(filepath.Join(dir, "record.json")); err != nil {
				t.Fatal("lost custody after uncertain cleanup", err)
			}
			// Remove only the extra pin this fixture created; the normal recovery helper
			// must then reconcile its recorded attachment without any forced detach.
			args := []string{"--host", docker.Endpoint(), "run", "--rm", "--user", "0", "--network", "none", "--cap-drop", "ALL", "--cap-add", "SYS_ADMIN", "--security-opt", "seccomp=unconfined", "--mount", networkMount("bind", "/sys/fs/bpf/"+parent, "/pins", false), "--entrypoint", "python3", os.Getenv("COOP_FILESYSTEM_CLIENT_IMAGE"), "-c", "import os; os.unlink('/pins/unexpected')"}
			data, err = exec.CommandContext(t.Context(), docker.Binary(), args...).CombinedOutput()
			if err != nil {
				t.Fatalf("remove fixture's unexpected pin: %v %s", err, data)
			}
			t.Log("unexpected pin refused retirement and retained custody; retry removes only recorded attachments")
		})
	}
}
