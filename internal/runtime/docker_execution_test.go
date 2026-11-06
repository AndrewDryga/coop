package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	rt, file := fixtureDocker(t, dockerFixture{Container: container, ErrorLogs: "admission refused\n"})
	d, err := BindDocker(t.Context(), rt, fixtureSock(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	state, data, err := d.ObserveContainerLogs(t.Context(), dockerFixtureRef(), 10, 1024)
	if err != nil || string(data) != "admission refused\n" || state.State.Status != "exited" {
		t.Fatalf("lost exited helper error: %q %v", data, err)
	}
	for _, ref := range []DockerRef{{Name: "wrong", ID: container.ID, Labels: dockerFixtureRef().Labels}, {Name: "coop-test", ID: strings.Repeat("b", 64), Labels: dockerFixtureRef().Labels}, {Name: "coop-test", ID: container.ID, Labels: map[string]string{"coop.network.run": "other"}}} {
		if _, err := d.ContainerLogs(t.Context(), ref, 10, 1024); err == nil {
			t.Fatal("read unrelated helper logs")
		}
	}
	writeDockerFixture(t, file, dockerFixture{Container: container, Logs: strings.Repeat("x", 512), ErrorLogs: strings.Repeat("x", 1024)})
	if _, err := d.ContainerLogs(t.Context(), dockerFixtureRef(), 10, 1024); err == nil {
		t.Fatalf("unbounded logs: %v", err)
	}
	writeDockerFixture(t, file, dockerFixture{})
	if _, err := d.ContainerLogs(t.Context(), dockerFixtureRef(), 10, 1024); err == nil {
		t.Fatal("observed absent helper")
	}
}

func TestDockerCgroupBatchRequiresCompleteExactInventory(t *testing.T) {
	ids := []string{strings.Repeat("a", 64), strings.Repeat("b", 64)}
	parent := "/coop-fs-" + strings.Repeat("c", 32)
	first := `{"ID":"` + ids[0] + `","Parent":""}`
	second := `{"ID":"` + ids[1] + `","Parent":"/unrelated"}`
	if err := checkCgroupParents([]byte(second+"\n"+first), ids, parent); err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{
		first, first + first, first + `{"ID":"foreign","Parent":""}`,
		first + `{"ID":"` + ids[1] + `"}`, first + `{"ID":"` + ids[1] + `","Parent":null}`,
		first + `{"ID":"` + ids[1] + `","Parent":42}`, first + second + "broken",
		first + `{"ID":"` + ids[1] + `","Parent":"` + parent + `"}`,
		first + `{"ID":"` + ids[1] + `","Parent":"` + strings.TrimPrefix(parent, "/") + `"}`,
	} {
		if err := checkCgroupParents([]byte(data), ids, parent); err == nil {
			t.Fatalf("accepted uncertain/occupied inventory: %s", data)
		}
	}
}

func TestDockerCgroupInventorySpansBoundedBatches(t *testing.T) {
	parents := map[string]string{}
	for i := range dockerCgroupBatch + 1 {
		parents[fmt.Sprintf("%064x", i+1)] = "/unrelated"
	}
	rt, file := fixtureDocker(t, dockerFixture{CgroupParents: parents})
	d, err := BindDocker(t.Context(), rt, fixtureSock(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	parent := "/coop-fs-" + strings.Repeat("a", 32)
	if err := d.CgroupParentUnused(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	// Every ID is inspected, including the member beyond a complete batch.
	parents[fmt.Sprintf("%064x", dockerCgroupBatch+1)] = parent
	writeDockerFixture(t, file, dockerFixture{CgroupParents: parents})
	if err := d.CgroupParentUnused(t.Context(), parent); err == nil {
		t.Fatal("last-batch consumer lost custody")
	}
}

func TestDockerCgroupInventoryReconcilesOnlyProvenDisappearance(t *testing.T) {
	parent := "/coop-fs-" + strings.Repeat("c", 32)
	for _, mode := range []string{"cgroup-disappear", "cgroup-new-consumer", "cgroup-add-only", "cgroup-stable-fail", "cgroup-relist-fail", "cgroup-daemon-replaced", "cgroup-churn"} {
		t.Run(mode, func(t *testing.T) {
			parents := map[string]string{strings.Repeat("a", 64): "/unrelated", strings.Repeat("b", 64): "/unrelated"}
			rt, _ := fixtureDocker(t, dockerFixture{CgroupParents: parents, Mode: mode})
			d, err := BindDocker(t.Context(), rt, fixtureSock(), "")
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			ctx := t.Context()
			if mode == "cgroup-churn" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 300*time.Millisecond)
				defer cancel()
			}
			err = d.CgroupParentUnused(ctx, parent)
			if mode == "cgroup-new-consumer" && (err == nil || !strings.Contains(err.Error(), "restartable Docker container")) {
				t.Fatalf("fresh stopped consumer was not inspected: %v", err)
			}
			if mode == "cgroup-churn" && ctx.Err() == nil {
				t.Fatalf("churn did not reach its bounded deadline: %v", err)
			}
			// A busy host's teardown printed the deadline twice ("inspect Docker filesystem consumers:
			// context deadline exceeded", then a bare "context deadline exceeded") and failed the run.
			// The deadline is now the one unfinished inventory, and nothing else may be read as it.
			if unfinished := errors.Is(err, ErrConsumerInventoryUnfinished); unfinished != (mode == "cgroup-churn") ||
				unfinished && err.Error() != ErrConsumerInventoryUnfinished.Error() {
				t.Fatalf("%s: inventory error %q, unfinished=%v", mode, err, unfinished)
			}
			if mode == "cgroup-disappear" {
				if err != nil {
					t.Fatalf("unrelated disappeared container retained custody: %v", err)
				}
			} else if err == nil {
				t.Fatal("uncertain or occupied parent released")
			}
		})
	}
}

func TestFilesystemCgroupParentIncludesNestedConsumers(t *testing.T) {
	root := "/coop-fs-controller-" + strings.Repeat("a", 32)
	run := root + "/coop-fs-" + strings.Repeat("b", 32)
	for _, parent := range []string{root, run, "/coop-fs-" + strings.Repeat("c", 32)} {
		if !validFilesystemCgroupParent(parent) {
			t.Fatalf("rejected owned parent %q", parent)
		}
	}
	for _, parent := range []string{root + "/", run + "/extra", root + "/../other", strings.TrimPrefix(root, "/"), "/coop-fs-short"} {
		if validFilesystemCgroupParent(parent) {
			t.Fatalf("accepted invalid parent %q", parent)
		}
	}
	id := strings.Repeat("d", 64)
	for _, observed := range []string{run, run + "/deeper", strings.TrimPrefix(run, "/")} {
		data := []byte(fmt.Sprintf(`{"ID":%q,"Parent":%q}`, id, observed))
		if checkCgroupParents(data, []string{id}, root) == nil {
			t.Fatalf("released root containing %s", observed)
		}
		if checkCgroupParents(data, []string{id}, run) == nil {
			t.Fatalf("released admission containing %s", observed)
		}
	}
	data := []byte(fmt.Sprintf(`{"ID":%q,"Parent":%q}`, id, run+"0"))
	if err := checkCgroupParents(data, []string{id}, run); err != nil {
		t.Fatal("matched sibling prefix", err)
	}
}
