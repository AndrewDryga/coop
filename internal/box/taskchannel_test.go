package box

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/taskchannel"
)

// echoServer stands in for taskmcp's server (box cannot import it: tasks imports box): it answers
// each line with {"echo":<line>} until the client hangs up, which is all the channel's lifecycle
// needs to prove; the tools themselves are proven in internal/taskmcp.
type echoServer struct{}

func (echoServer) Serve(ctx context.Context, rw io.ReadWriter) error {
	reader := bufio.NewReader(rw)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			reply, _ := json.Marshal(map[string]any{"echo": strings.TrimSpace(string(line))})
			if _, werr := rw.Write(append(reply, '\n')); werr != nil {
				return werr
			}
		}
		if err != nil {
			return nil
		}
	}
}

func TestTaskChannelVolumeIsDistinctPerRun(t *testing.T) {
	a, err := taskChannelVolume()
	if err != nil {
		t.Fatal(err)
	}
	b, err := taskChannelVolume()
	if err != nil {
		t.Fatal(err)
	}
	if a == b || !strings.HasPrefix(a, "coop-tasks-") || len(a) != len("coop-tasks-")+16 {
		t.Fatalf("volumes = %q %q", a, b)
	}
	if got := taskChannelMount(a); got != a+":/coop/tasks:ro" {
		t.Fatalf("mount = %q", got)
	}
}

// The box mounts the channel's volume read-only at the socket dir only when a run chose one;
// every other run's argument list is byte-identical to before.
func TestAssembleArgsMountsTheTaskChannelReadOnly(t *testing.T) {
	cfg := &config.Config{HomeInBox: "/home/node", ConfigDir: t.TempDir(), Egress: "open"}
	mounts := []Mount{{Kind: Bind, Source: "/repo", Target: "/workspace"}}
	t.Setenv("TZ", "America/Merida")
	plain := assembleArgs(cfg, true, RunSpec{Image: "i", Repo: "/repo"}, mounts, "/d", "/dd", "/workspace", ttyNone, false, nil, nil, nil, nil, nil, "", "")
	withChannel := assembleArgs(cfg, true, RunSpec{Image: "i", Repo: "/repo", taskVolume: "coop-tasks-0123"}, mounts, "/d", "/dd", "/workspace", ttyNone, false, nil, nil, nil, nil, nil, "", "")
	if slices.Contains(plain, "coop-tasks-0123:/coop/tasks:ro") {
		t.Fatalf("a run without a channel mounted one: %v", plain)
	}
	at := slices.Index(withChannel, "coop-tasks-0123:/coop/tasks:ro")
	if at < 1 || withChannel[at-1] != "-v" {
		t.Fatalf("channel mount missing or malformed: %v", withChannel)
	}
	// Only the mount differs; nothing else about the box changed.
	without := slices.Delete(slices.Clone(withChannel), at-1, at+1)
	if !slices.Equal(without, plain) {
		t.Fatalf("channel changed more than its mount:\n got %v\nwant %v", without, plain)
	}
	// Under --network none too: the channel needs no network.
	offline := assembleArgs(&config.Config{HomeInBox: "/home/node", ConfigDir: t.TempDir(), Egress: "none"}, true, RunSpec{Image: "i", Repo: "/repo", taskVolume: "coop-tasks-0123"}, mounts, "/d", "/dd", "/workspace", ttyNone, false, nil, nil, nil, nil, nil, "", "")
	if !slices.Contains(offline, "coop-tasks-0123:/coop/tasks:ro") || !slices.Contains(offline, "none") {
		t.Fatalf("offline args = %v", offline)
	}
}

func TestWriteTaskChannelScriptIsWorldReadable(t *testing.T) {
	artifacts := defaultCompositionArtifactOps()
	artifacts.parent = t.TempDir()
	path, err := writeTaskChannelScript(artifacts)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("script mode = %v (%v), want 0644", info.Mode(), err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != taskchannel.MuxScript {
		t.Fatal("script content differs from the embedded multiplexer")
	}
}

// fakeChannelRuntime is a container runtime for the channel's lifecycle: `volume create`/`volume rm` are
// recorded, and `run` executes the real multiplexer script on the host's node with the socket
// redirected to a host path — so the test proves the channel exactly as the box would use it,
// minus the container boundary the live test covers.
func fakeChannelRuntime(t *testing.T, sock string) (runtime.Runtime, string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	script := filepath.Join(dir, "fake-runtime")
	body := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + log + "\n" +
		"case \"$1 $2\" in\n" +
		"  'volume create'|'volume rm') exit 0 ;;\n" +
		"  'run '*)\n" +
		"    mux=''; for a in \"$@\"; do case \"$a\" in *:/coop/mux.js:ro) mux=${a%%:*} ;; esac; done\n" +
		"    exec " + node + " \"$mux\" " + sock + "\n" +
		"    ;;\n" +
		"esac\n" +
		"echo \"fake runtime: unexpected $*\" >&2; exit 1\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return runtime.Runtime{Name: script}, log
}

func TestTaskChannelServesTheBoxAndTearsDownAfterTheRun(t *testing.T) {
	sockDir, err := os.MkdirTemp("/tmp", "coop-chan-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)
	sock := filepath.Join(sockDir, "mcp.sock")
	rt, log := fakeChannelRuntime(t, sock)
	artifacts := defaultCompositionArtifactOps()
	artifacts.parent = t.TempDir()
	script, err := writeTaskChannelScript(artifacts)
	if err != nil {
		t.Fatal(err)
	}
	volume, _ := taskChannelVolume()
	channel, err := startTaskChannel(context.Background(), rt, "coop-box", volume, "run-1", echoServer{}, script)
	if err != nil {
		t.Fatalf("startTaskChannel: %v", err)
	}
	calls, _ := os.ReadFile(log)
	if !strings.Contains(string(calls), "volume create --label coop=task-channel "+volume+"\n") {
		t.Fatalf("volume not created: %s", calls)
	}
	runLine := ""
	for _, line := range strings.Split(string(calls), "\n") {
		if strings.HasPrefix(line, "run ") {
			runLine = line
		}
	}
	for _, want := range []string{"--rm -i --network none --user root --name " + volume, "--label coop=task-channel", "--label coop.run=run-1", "-v " + volume + ":/coop/tasks", ":/coop/mux.js:ro --entrypoint node coop-box /coop/mux.js /coop/tasks/mcp.sock"} {
		if !strings.Contains(runLine, want) {
			t.Fatalf("helper run line lacks %q: %s", want, runLine)
		}
	}
	// The box side: two clients on the socket, each served by its own session, replies routed
	// back to the right one.
	dial := func() (net.Conn, *bufio.Reader) {
		conn, err := net.DialTimeout("unix", sock, 5*time.Second)
		if err != nil {
			t.Fatalf("dial the channel: %v", err)
		}
		return conn, bufio.NewReader(conn)
	}
	ask := func(conn net.Conn, reader *bufio.Reader, req string) string {
		t.Helper()
		if _, err := io.WriteString(conn, req+"\n"); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		var reply map[string]any
		if err := json.Unmarshal(line, &reply); err != nil {
			t.Fatal(err)
		}
		return reply["echo"].(string)
	}
	c1, r1 := dial()
	c2, r2 := dial()
	if got := ask(c2, r2, `{"id":"peer"}`); got != `{"id":"peer"}` {
		t.Fatalf("peer reply = %s", got)
	}
	if got := ask(c1, r1, `{"id":"lead"}`); got != `{"id":"lead"}` {
		t.Fatalf("lead reply = %s", got)
	}
	c1.Close()
	c2.Close()
	// After the run: the helper is stopped, the socket is gone with it, and the volume removed.
	if err := channel.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := net.DialTimeout("unix", sock, time.Second); err == nil {
		t.Fatal("the channel still accepts after the run ended")
	}
	calls, _ = os.ReadFile(log)
	if !strings.HasSuffix(strings.TrimSpace(string(calls)), "volume rm "+volume) {
		t.Fatalf("volume not removed last: %s", calls)
	}
	if err := channel.close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestTaskChannelFailsClosedWhenTheHelperDies(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-runtime")
	body := "#!/bin/sh\ncase \"$1 $2\" in 'volume create'|'volume rm') exit 0 ;; esac\necho 'no such image' >&2; exit 125\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := startTaskChannel(context.Background(), runtime.Runtime{Name: script}, "img", "coop-tasks-dead", "", echoServer{}, filepath.Join(dir, "mux.js"))
	if err == nil || !strings.Contains(err.Error(), "helper exited before its socket was ready") || !strings.Contains(err.Error(), "no such image") {
		t.Fatalf("err = %v", err)
	}
}
