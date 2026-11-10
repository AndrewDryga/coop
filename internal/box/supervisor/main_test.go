package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func fixtureProcess(t *testing.T, root string, p process) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(p.pid))
	if err := os.MkdirAll(filepath.Join(dir, "fd"), 0755); err != nil {
		t.Fatal(err)
	}
	fields := []string{p.state, strconv.Itoa(p.parent), "0", strconv.Itoa(p.session)}
	for len(fields) < 19 {
		fields = append(fields, "0")
	}
	fields = append(fields, p.start)
	data := fmt.Sprintf("%d (%s) %s\n", p.pid, p.name, strings.Join(fields, " "))
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestJobsKeepDetachedWorkAndAuthenticateForwarders(t *testing.T) {
	fs := procFS{root: t.TempDir(), self: 10, forwarders: map[int]string{20: "200", 30: "old"}}
	for _, p := range []process{
		{identity{1, "1"}, 0, 1, "S", "init"},
		{identity{10, "10"}, 1, 10, "S", "supervisor"},
		{identity{11, "11"}, 10, 10, "S", "transient child"},
		{identity{20, "200"}, 10, 20, "S", "forwarder"},
		{identity{21, "210"}, 20, 20, "S", "forwarder child"},
		{identity{30, "new"}, 1, 30, "S", "reused forwarder pid"},
		{identity{31, "310"}, 30, 30, "S", "reused session"},
		{identity{40, "400"}, 1, 40, "S", "double ) detached ( job"},
		{identity{41, "410"}, 1, 40, "Z", "zombie"},
	} {
		fixtureProcess(t, fs.root, p)
	}
	jobs, err := fs.jobs()
	if err != nil {
		t.Fatal(err)
	}
	var ids []int
	for _, job := range jobs {
		ids = append(ids, job.pid)
	}
	if !slices.Equal(ids, []int{30, 31, 40}) {
		t.Fatalf("visible work = %v", ids)
	}
	if jobs[2].name != "double ) detached ( job" || jobs[2].start != "400" {
		t.Fatalf("stat parsed incorrectly: %+v", jobs[2])
	}
	if err := os.RemoveAll(filepath.Join(fs.root, "20")); err != nil {
		t.Fatal(err)
	}
	jobs, err = fs.jobs()
	if err != nil || len(jobs) != 4 || jobs[0].pid != 21 {
		t.Fatalf("dead leader still exempts work: %+v, %v", jobs, err)
	}
}

func TestClassifyConsultAndDaemonWithoutHidingRealWork(t *testing.T) {
	fs := procFS{root: t.TempDir()}
	p := process{identity{42, "420"}, 1, 42, "S", "worker"}
	fixtureProcess(t, fs.root, p)
	for fd := 0; fd < 3; fd++ {
		if err := os.Symlink("/dev/null", fs.path(p.pid, "fd/"+strconv.Itoa(fd))); err != nil {
			t.Fatal(err)
		}
	}
	if !fs.daemon(p, 50) || fs.daemon(p, 42) {
		t.Fatal("detached quiet daemon and provider session work conflated")
	}
	if err := os.Remove(fs.path(p.pid, "fd/1")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("pipe:[123]", fs.path(p.pid, "fd/1")); err != nil {
		t.Fatal(err)
	}
	if fs.daemon(p, 50) {
		t.Fatal("detached work with output classified as daemon")
	}
	p.name = "epmd"
	if !fs.daemon(p, 42) {
		t.Fatal("known daemon not identified")
	}
	for value, want := range map[string]bool{"OTHER_COOP_CONSULT_OWNED=1\x00": false, "COOP_CONSULT_OWNED=10\x00": false, "A=b\x00COOP_CONSULT_OWNED=1\x00": true} {
		if err := os.WriteFile(fs.path(p.pid, "environ"), []byte(value), 0644); err != nil {
			t.Fatal(err)
		}
		if fs.owned(p) != want {
			t.Fatalf("ownership for %q", value)
		}
	}
}

func TestDrainFailsOnUnreadableInventoryAndPreservesTimeout(t *testing.T) {
	fs := procFS{root: filepath.Join(t.TempDir(), "absent")}
	if _, err := fs.drain(context.Background(), 50, 0, io.Discard); err == nil {
		t.Fatal("missing process inventory passed")
	}
	fs.root = t.TempDir()
	if code, err := fs.drain(context.Background(), 50, 0, io.Discard); code != 0 || err != nil {
		t.Fatalf("empty: %d, %v", code, err)
	}
	fixtureProcess(t, fs.root, process{identity{42, "420"}, 1, 50, "S", "work"})
	// A zero-window timeout returns its decision only; no host process is signaled.
	if code, err := fs.drain(context.Background(), 50, 0, io.Discard); code != timeoutExit || err != nil {
		t.Fatalf("live work: %d, %v", code, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code, err := fs.drain(ctx, 50, time.Minute, io.Discard); code != 143 || err != nil {
		t.Fatalf("cancelled drain: %d, %v", code, err)
	}
}

func TestTimeoutAndProviderExitStatus(t *testing.T) {
	for _, value := range []string{"", "-1", "bad", "999999999999999999999999"} {
		if descendantTimeout(value) != 900*time.Second {
			t.Fatalf("invalid timeout %q did not use bounded default", value)
		}
	}
	if descendantTimeout("0") != 0 || descendantTimeout("30") != 30*time.Second {
		t.Fatal("valid timeout changed")
	}
	for command, want := range map[string]int{"exit 0": 0, "exit 7": 7, "exit 190": 1, "exit 191": 1, "kill -TERM $$": 143} {
		if got := exitCode(exec.Command("sh", "-c", command).Run()); got != want {
			t.Fatalf("%q: %d, want %d", command, got, want)
		}
	}
}
