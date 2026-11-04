package cli

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/acpctl"
)

const procTCPHeader = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"

func procTCPRow(local, state, inode string) string {
	return "   0: " + local + " 00000000:0000 " + state + " 00000000:00000000 00:00000000 00000000  1000        0 " + inode + " 1 0000 100 0 0 10 0\n"
}

// A proc folder with one server in it, written as the kernel would.
func fakeProc(t *testing.T, inode, command string) string {
	t.Helper()
	proc := t.TempDir()
	if err := os.MkdirAll(filepath.Join(proc, "net"), 0o755); err != nil {
		t.Fatal(err)
	}
	fd := filepath.Join(proc, "321", "fd")
	if err := os.MkdirAll(fd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("socket:["+inode+"]", filepath.Join(fd, "7")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proc, "321", "cmdline"), []byte(strings.ReplaceAll(command, " ", "\x00")+"\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	return proc
}

// The script itself, on the shell a box has, against a fake proc: what it says when a server starts,
// that it says it once, that a restart counts as a new start, and that closing stdin ends it.
func TestServeWatchScriptReportsEachServerStartOnce(t *testing.T) {
	proc := fakeProc(t, "111", "mix phx.server")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(proc, "net", "tcp"), []byte(procTCPHeader+body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(procTCPRow("00000000:2328", "0A", "999")) // port 9000: not a serve port
	cmd := exec.Command("sh", "-c", serveWatchScript, "coop-serve-watch", "4000", "9091")
	cmd.Env = append(os.Environ(), "COOP_SERVE_WATCH_PROC="+proc)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	lines := make(chan string, 16)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()
	next := func(want string) {
		t.Helper()
		select {
		case got := <-lines:
			if strings.TrimRight(got, " ") != want { // a command line ends in the space its last NUL became
				t.Fatalf("watcher said %q, want %q", got, want)
			}
		case <-time.After(15 * time.Second):
			t.Fatalf("watcher said nothing, want %q", want)
		}
	}
	write(procTCPRow("00000000:0FA0", "0A", "111")) // 0.0.0.0:4000
	next("up\t4000\tany\tmix phx.server")
	select {
	case got := <-lines:
		t.Fatalf("the watcher repeated itself: %q", got)
	case <-time.After(2500 * time.Millisecond):
	}
	write("") // it stops
	next("down\t4000")
	write(procTCPRow("0100007F:0FA0", "0A", "222")) // restarted on 127.0.0.1
	next("up\t4000\tloopback\t")
	write(procTCPRow("00000000:0FA0", "01", "333")) // an established connection is no listener
	next("down\t4000")
	_ = stdin.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("closing stdin did not end the watcher")
	}
}

func TestParseServeEventIsStrict(t *testing.T) {
	if got, ok := parseServeEvent("up\t4000\tany\tmix phx.server"); !ok || got != (serveEvent{port: 4000, up: true, command: "mix phx.server"}) {
		t.Errorf("up = %+v, %t", got, ok)
	}
	if got, ok := parseServeEvent("up\t4000\tloopback\t"); !ok || !got.loopback || got.command != "" {
		t.Errorf("loopback = %+v, %t", got, ok)
	}
	if got, ok := parseServeEvent("down\t4000"); !ok || got.up || got.port != 4000 {
		t.Errorf("down = %+v, %t", got, ok)
	}
	for _, bad := range []string{"", "up", "up\tx\tany\tc", "up\t0\tany\tc", "up\t70000\tany\tc", "up\t4000\tsomewhere\tc", "up\t4000\tany", "down\t4000\textra", "ignore\t4000"} {
		if _, ok := parseServeEvent(bad); ok {
			t.Errorf("parseServeEvent(%q) accepted it", bad)
		}
	}
}

func TestServeAnnouncerSaysEachStartOnce(t *testing.T) {
	var said []string
	at := time.Unix(1000, 0)
	announcer := serveAnnouncer{
		ports:  func() []acpctl.ServePort { return []acpctl.ServePort{{Box: 4000, Host: 43659}} },
		notify: func(text string) { said = append(said, text) },
		now:    func() time.Time { return at },
	}
	announcer.handle(serveEvent{port: 9999, up: true, command: "not ours"})
	if len(said) != 0 {
		t.Fatalf("a port that is not a serve port was announced: %v", said)
	}
	announcer.handle(serveEvent{port: 4000, up: true, loopback: true, command: "vite"})
	if len(said) != 1 || !strings.HasPrefix(said[0], "⚠") || !strings.Contains(said[0], "0.0.0.0") {
		t.Fatalf("loopback = %v", said)
	}
	// Restarted on every address: it is reachable now, so it says so.
	at = at.Add(3 * time.Second)
	announcer.handle(serveEvent{port: 4000})
	announcer.handle(serveEvent{port: 4000, up: true, command: "vite --host"})
	if len(said) != 2 || !strings.Contains(said[1], "[localhost:43659](http://localhost:43659)") {
		t.Fatalf("up = %v", said)
	}
	// A box writing lines of its own gets no flood.
	for range 5 {
		announcer.handle(serveEvent{port: 4000})
		announcer.handle(serveEvent{port: 4000, up: true, command: "again"})
	}
	if len(said) != 2 {
		t.Fatalf("flood reached the thread: %v", said)
	}
}
