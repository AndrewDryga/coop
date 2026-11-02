package runtime

import (
	"bytes"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDockerCopyFileStreamsAndRejectsInvalidArchives(t *testing.T) {
	for _, mode := range []string{"regular", "symlink", "directory", "two-entries", "short", "missing", "over-limit", "write-error"} {
		t.Run(mode, func(t *testing.T) {
			body := strings.Repeat("x", 5<<20)
			container := dockerFixtureContainer()
			container.State = DockerContainerState{Status: "running", Running: true}
			rt, _ := fixtureDocker(t, dockerFixture{Container: container, Copy: mode, CopyBody: body})
			d, err := BindDocker(t.Context(), rt, fixtureSock(), "")
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			var out bytes.Buffer
			var dst io.Writer = &out
			limit := int64(6 << 20)
			if mode == "over-limit" {
				limit = 4 << 20
			}
			if mode == "write-error" {
				dst = filesystemFailingWriter{}
			}
			err = d.CopyFile(t.Context(), dockerFixtureRef(), "/usr/local/bin/guard", dst, limit)
			if mode != "regular" {
				if err == nil {
					t.Fatal("accepted invalid or incomplete copy")
				}
				return
			}
			if err != nil || out.String() != body {
				t.Fatalf("large regular file copy: %v (%d bytes)", err, out.Len())
			}
			ref := dockerFixtureRef()
			ref.ID = strings.Repeat("b", 64)
			if err := d.CopyFile(t.Context(), ref, "/guard", io.Discard, limit); err == nil {
				t.Fatal("copied from a replacement container")
			}
		})
	}
}

type filesystemFailingWriter struct{}

func (filesystemFailingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestDockerImageExecutionPreservesDefaultsAndRequiresIdentity(t *testing.T) {
	want := DockerImageExecution{ID: "sha256:" + strings.Repeat("d", 64), Entrypoint: []string{"/entry"}, Cmd: []string{"serve", "$literal"}, Shell: []string{"/bin/bash", "-c"}, Healthcheck: &DockerHealthcheck{Test: []string{"CMD", "ready"}, Interval: time.Second, Timeout: 2 * time.Second, StartPeriod: 3 * time.Second, StartInterval: 4 * time.Second, Retries: 5}, Volumes: map[string]struct{}{"/data": {}}}
	rt, file := fixtureDocker(t, dockerFixture{Execution: &want})
	d, err := BindDocker(t.Context(), rt, fixtureSock(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	got, err := d.ImageExecution(t.Context(), want.ID)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("startup defaults lost: %#v %v", got, err)
	}
	if _, err := d.ImageExecution(t.Context(), "mutable:tag"); err == nil {
		t.Fatal("accepted a mutable image")
	}
	changed := want
	changed.ID = "sha256:" + strings.Repeat("e", 64)
	for _, fixture := range []dockerFixture{{Execution: &changed}, {Mode: "execution-malformed"}, {ID: "replacement-daemon", Execution: &want}} {
		writeDockerFixture(t, file, fixture)
		if _, err := d.ImageExecution(t.Context(), want.ID); err == nil {
			t.Fatal("accepted an unproven startup observation")
		}
	}
}

func TestDockerCgroupParentRetainsStoppedConsumers(t *testing.T) {
	parent := "/coop-fs-" + strings.Repeat("a", 32)
	container := dockerFixtureContainer()
	container.State = DockerContainerState{Status: "exited"}
	rt, file := fixtureDocker(t, dockerFixture{Container: container})
	d, err := BindDocker(t.Context(), rt, fixtureSock(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, spelling := range []string{parent, strings.TrimPrefix(parent, "/")} {
		container.CgroupParent = spelling
		writeDockerFixture(t, file, dockerFixture{Container: container})
		if err := d.CgroupParentUnused(t.Context(), parent); err == nil {
			t.Fatal("released a stopped consumer's parent")
		}
	}
	container.CgroupParent = "/unrelated"
	writeDockerFixture(t, file, dockerFixture{Container: container})
	if err := d.CgroupParentUnused(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"list-fail", "inspect-fail"} {
		writeDockerFixture(t, file, dockerFixture{Container: container, Mode: mode})
		if err := d.CgroupParentUnused(t.Context(), parent); err == nil {
			t.Fatal("uncertain inventory permitted release")
		}
	}
}

func TestDockerHelperLogsRemainBoundedAndOwned(t *testing.T) {
	container := dockerFixtureContainer()
	container.State = DockerContainerState{Status: "exited"}
	rt, file := fixtureDocker(t, dockerFixture{Container: container, Logs: "admission refused\n"})
	d, err := BindDocker(t.Context(), rt, fixtureSock(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	data, err := d.ContainerLogs(t.Context(), dockerFixtureRef(), 10, 1024)
	if err != nil || string(data) != "admission refused\n" {
		t.Fatalf("lost exited helper error: %q %v", data, err)
	}
	for _, ref := range []DockerRef{{Name: "wrong", ID: container.ID, Labels: dockerFixtureRef().Labels}, {Name: "coop-test", ID: strings.Repeat("b", 64), Labels: dockerFixtureRef().Labels}, {Name: "coop-test", ID: container.ID, Labels: map[string]string{"coop.network.run": "other"}}} {
		if _, err := d.ContainerLogs(t.Context(), ref, 10, 1024); err == nil {
			t.Fatal("read unrelated helper logs")
		}
	}
	writeDockerFixture(t, file, dockerFixture{Container: container, Logs: strings.Repeat("x", 2048)})
	if _, err := d.ContainerLogs(t.Context(), dockerFixtureRef(), 10, 1024); err == nil {
		t.Fatalf("unbounded logs: %v", err)
	}
	writeDockerFixture(t, file, dockerFixture{})
	if _, err := d.ContainerLogs(t.Context(), dockerFixtureRef(), 10, 1024); err == nil {
		t.Fatal("observed absent helper")
	}
}
