package acpproxy

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// resetTrace clears the package-level trace state so each trace test is isolated (and restores it
// after). White-box: the tracer is package-global, like warnOut.
func resetTrace(t *testing.T) {
	t.Helper()
	clear := func() {
		traceMu.Lock()
		if c, ok := traceOut.(io.Closer); ok {
			c.Close()
		}
		traceOut, traceGaveUp, traceLastAt, traceWritten = nil, false, time.Time{}, 0
		traceMu.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

// TestTraceWritesWhenEnabled: with COOP_ACP_TRACE set, the wire + events land in
// <config>/acp-trace-<pid>.log.
func TestTraceWritesWhenEnabled(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir) // traces land under <dir>/coop, not the real config
	t.Setenv("COOP_ACP_TRACE", "1")
	resetTrace(t)

	traceLine("editor→box", []byte(`{"jsonrpc":"2.0","id":1,"method":"session/new"}`+"\n"))
	Trace("restart requested")

	path := filepath.Join(dir, "coop", fmt.Sprintf("acp-trace-%d.log", os.Getpid()))
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("trace file not written: %v", err)
	}
	for _, want := range []string{"editor→box", "session/new", "restart requested"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("trace missing %q:\n%s", want, b)
		}
	}
}

func TestTraceTightensSensitiveLogPermissions(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("COOP_ACP_TRACE", "1")
	resetTrace(t)
	cdir := filepath.Join(dir, "coop")
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cdir, fmt.Sprintf("acp-trace-%d.log", os.Getpid()))
	if err := os.WriteFile(path, []byte("old sensitive trace\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil { // ignore a restrictive test-process umask
		t.Fatal(err)
	}
	legacy := []string{
		path + ".1",
		filepath.Join(cdir, "acp-trace-2000000001.log"),
		filepath.Join(cdir, "acp-trace-2000000001.log.1"),
	}
	for _, retained := range legacy {
		if err := os.WriteFile(retained, []byte("older sensitive trace\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(retained, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	Trace("new sensitive trace")
	for _, secured := range append([]string{path}, legacy...) {
		info, err := os.Stat(secured)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("trace %s permissions = %04o, want 0600", filepath.Base(secured), got)
		}
	}
}

// TestTraceEnabledBySentinel: the sentinel file turns tracing on with no env var (so it works on an
// already-running server).
func TestTraceEnabledBySentinel(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("COOP_ACP_TRACE", "") // env OFF; only the sentinel enables it
	if err := os.MkdirAll(filepath.Join(dir, "coop"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "coop", traceSentinel), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	resetTrace(t)

	Trace("via sentinel")
	path := filepath.Join(dir, "coop", fmt.Sprintf("acp-trace-%d.log", os.Getpid()))
	if b, err := os.ReadFile(path); err != nil || !strings.Contains(string(b), "via sentinel") {
		t.Fatalf("sentinel did not enable tracing (err=%v): %s", err, b)
	}
}

// TestTraceOffByDefault: with neither the env var nor the sentinel, tracing writes nothing.
func TestTraceOffByDefault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir) // fresh config dir, no sentinel
	t.Setenv("COOP_ACP_TRACE", "")
	resetTrace(t)

	traceLine("editor→box", []byte("{}\n"))
	Trace("nope")

	if got, _ := filepath.Glob(filepath.Join(dir, "coop", "acp-trace-*.log")); len(got) > 0 {
		t.Errorf("tracing wrote while off: %v", got)
	}
}

// TestTraceRotatesAtCap: the primary log never exceeds the byte cap; overflow rolls into a .1 backup,
// so a long-running server's trace stays bounded (~2× the cap).
func TestTraceRotatesAtCap(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("COOP_ACP_TRACE", "1")
	resetTrace(t)
	old := traceMaxBytes
	traceMaxBytes = 300
	defer func() { traceMaxBytes = old }()

	for i := 0; i < 80; i++ {
		Trace("line %02d — filler to push past the tiny test cap xxxxxxxxxx", i)
	}
	base := filepath.Join(dir, "coop", fmt.Sprintf("acp-trace-%d.log", os.Getpid()))
	fi, err := os.Stat(base)
	if err != nil {
		t.Fatalf("primary log: %v", err)
	}
	if fi.Size() > traceMaxBytes {
		t.Errorf("primary log %d bytes exceeds cap %d — rotation didn't bound it", fi.Size(), traceMaxBytes)
	}
	if _, err := os.Stat(base + ".1"); err != nil {
		t.Errorf("expected a .1 backup after rotation: %v", err)
	}
}

// TestTracePrunesOldFiles: opening a new trace prunes old per-pid logs to the newest traceKeepFiles,
// but never removes a log whose pid is still running.
func TestTracePrunesOldFiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("COOP_ACP_TRACE", "1")
	cdir := filepath.Join(dir, "coop")
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Five stale logs from dead pids (huge, non-existent), oldest→newest by mtime, each with a .1.
	base := time.Now().Add(-time.Hour)
	stale := []int{2000000001, 2000000002, 2000000003, 2000000004, 2000000005}
	for i, pid := range stale {
		p := filepath.Join(cdir, fmt.Sprintf("acp-trace-%d.log", pid))
		if err := os.WriteFile(p, []byte("old\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p+".1", []byte("older\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		mt := base.Add(time.Duration(i) * time.Minute)
		os.Chtimes(p, mt, mt)
	}
	orphan := filepath.Join(cdir, "acp-trace-2000000099.log.1")
	if err := os.WriteFile(orphan, []byte("orphaned sensitive trace\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := traceKeepFiles
	traceKeepFiles = 3
	defer func() { traceKeepFiles = old }()
	resetTrace(t)

	Trace("hi") // opens our log and prunes
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan trace backup survived prune: %v", err)
	}

	if _, err := os.Stat(filepath.Join(cdir, fmt.Sprintf("acp-trace-%d.log", os.Getpid()))); err != nil {
		t.Fatalf("our own log was removed: %v", err)
	}
	remaining := 0
	for _, pid := range stale {
		if _, err := os.Stat(filepath.Join(cdir, fmt.Sprintf("acp-trace-%d.log", pid))); err == nil {
			remaining++
		}
	}
	if remaining != traceKeepFiles-1 {
		t.Errorf("kept %d stale logs, want %d (newest, minus our own slot)", remaining, traceKeepFiles-1)
	}
	// The oldest three (and their .1) should be gone; the newest two survive.
	for _, pid := range stale[:3] {
		if _, err := os.Stat(filepath.Join(cdir, fmt.Sprintf("acp-trace-%d.log.1", pid))); err == nil {
			t.Errorf("stale backup for pid %d not pruned", pid)
		}
	}
}
