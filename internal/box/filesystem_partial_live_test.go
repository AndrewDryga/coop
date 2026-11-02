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
	"strings"
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
	changed := false
	image := filesystemFixtureImage(t, docker, "partial-attachment", func(name string, data []byte) []byte {
		if name != "internal/fsfilter/assets/shared.h" {
			return data
		}
		marker := `        nattached++;`
		if bytes.Count(data, []byte(marker)) != 1 {
			t.Fatal("partial attachment injection point changed")
		}
		injection := `
        if(nattached==1){
            if(getenv("COOP_TEST_UNKNOWN_PIN")){
                char extra[PATH_MAX];if(path_at(extra,pin_directory,"unexpected")||bpf_obj_pin(bpf_link__fd(link),extra))return -1;
            }
            if(getenv("COOP_TEST_SUBSTITUTED_PIN")){
                if(unlink(link_paths[0])||bpf_obj_pin(map_fd("scope"),link_paths[0]))return -1;
            }
            int marker=open("/state/partial-ready",O_CREAT|O_EXCL|O_WRONLY|O_CLOEXEC,0600);
            if(marker<0||close(marker))return -1;
            for(;;)pause();
        }`
		changed = true
		return bytes.Replace(data, []byte(marker), []byte(marker+injection), 1)
	})
	if !changed {
		t.Fatal("missing controller source")
	}
	return image
}

func filesystemFixtureImage(t *testing.T, docker *runtime.Docker, purpose string, alter func(string, []byte) []byte) string {
	t.Helper()
	source, err := fsimage.Context()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	w := tar.NewWriter(&out)
	r := tar.NewReader(bytes.NewReader(source))
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
		data = alter(h.Name, data)
		h.Size = int64(len(data))
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(filepath.Join(t.TempDir(), "build.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	image, err := docker.BuildImage(t.Context(), runtime.DockerBuild{Tag: "coop-filesystem-test:" + randid.Hex(16), Platform: "linux/" + runtime.Architecture(docker.Info().Architecture), Labels: map[string]string{"coop.filesystem.test": purpose}}, out.Bytes(), log, log)
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
		if err := cleanupTestFilesystemControllerImage(image.ID); err != nil {
			t.Error(err)
			return
		}
		if data, err := exec.CommandContext(ctx, binary, "--host", endpoint, "image", "rm", image.ID).CombinedOutput(); err != nil {
			t.Errorf("fixture image cleanup: %v %s", err, data)
		}
	})
	return image.ID
}

func TestFilesystemPartialAttachmentRecoveryRuntime(t *testing.T) {
	if os.Getenv("COOP_FILESYSTEM_RUNTIME_TEST") != "1" || os.Getenv("COOP_FILESYSTEM_CLIENT_IMAGE") == "" {
		t.Skip("requires local Docker qualification")
	}
	docker, err := runtime.BindDocker(t.Context(), runtime.Runtime{Name: "docker"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer docker.Close()
	image := partialFilesystemImage(t, docker)
	for _, fault := range []string{"partial", "unexpected-pin", "substituted-link"} {
		t.Run(fault, func(t *testing.T) {
			var env []string
			if fault == "unexpected-pin" {
				env = []string{"COOP_TEST_UNKNOWN_PIN=1"}
			} else if fault == "substituted-link" {
				env = []string{"COOP_TEST_SUBSTITUTED_PIN=1"}
			}
			c := fixtureFilesystemController(t, docker, image, env, "partial-ready", false)
			var birth fsfilter.ControllerBirth
			data, err := os.ReadFile(filepath.Join(c.dir, "birth.json"))
			if err != nil || json.Unmarshal(data, &birth) != nil || birth.Validate() != nil {
				t.Fatalf("missing complete controller birth before pin: %s %v", data, err)
			}
			log, err := docker.ContainerLogs(t.Context(), c.record.Container, 30, 1<<20)
			if err != nil || bytes.Contains(log, []byte(`"status":"controller-ready"`)) {
				t.Fatalf("partial controller reached READY: %s %v", log, err)
			}
			if err = c.remove(t.Context(), docker, c.record.Container); err != nil {
				t.Fatal(err)
			}
			c.closeLease()
			removed, err := recoverFilesystemController(t.Context(), runtime.Runtime{Name: "docker"}, c.dir)
			if fault == "partial" {
				if err != nil || !removed {
					t.Fatal("partial controller recovery", err)
				}
				return
			}
			if err == nil || removed {
				t.Fatal("unexpected pin removed without authority")
			}
			if _, err = os.Stat(filepath.Join(c.dir, "record.json")); err != nil {
				t.Fatal("lost custody", err)
			}
			pin := "unexpected"
			if fault == "substituted-link" {
				for _, program := range birth.Programs {
					if strings.HasPrefix(program.Name, "guard_") {
						pin = program.Name
						break
					}
				}
			}
			args := []string{"--host", docker.Endpoint(), "run", "--rm", "--user", "0", "--network", "none", "--cap-drop", "ALL", "--cap-add", "SYS_ADMIN", "--security-opt", "seccomp=unconfined", "--mount", networkMount("bind", "/sys/fs/bpf/"+c.record.Name, "/pins", false), "--entrypoint", "python3", os.Getenv("COOP_FILESYSTEM_CLIENT_IMAGE"), "-c", "import os;os.unlink('/pins/" + pin + "')"}
			if out, err := exec.CommandContext(t.Context(), docker.Binary(), args...).CombinedOutput(); err != nil {
				t.Fatalf("remove fixture pin: %v %s", err, out)
			}
			if removed, err = recoverFilesystemController(t.Context(), runtime.Runtime{Name: "docker"}, c.dir); err != nil || !removed {
				t.Fatal("retry partial controller recovery", err)
			}
			t.Log("unknown pin retained custody; exact receipt reconciled after fixture removed its extra pin")
		})
	}
}
