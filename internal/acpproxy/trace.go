package acpproxy

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ACP wire tracing — an opt-in diagnostic for when an editor's ACP session misbehaves (a lost
// session, a bad switch, an unexpected restart). It is OFF by default and costs nothing then.
//
// Turn it on either way:
//   - set COOP_ACP_TRACE (to any non-empty value) in the agent server's env, or
//   - create the sentinel file <config>/acp-debug  (e.g. ~/.config/coop/acp-debug).
//
// Then every `coop acp` process appends the editor↔box wire, plus restart/spawn events, to
// <config>/acp-trace-<pid>.log. The sentinel is re-checked while tracing is off (throttled to once a
// second), so you can switch tracing ON for an ALREADY-running server — Zed keeps one coop process
// alive across threads — without restarting it; `rm` the sentinel to stop new processes tracing.
//
// The log is bounded: each file rotates to a single .log.1 backup once it passes traceMaxBytes
// (≈2×traceMaxBytes per server), and on startup old logs from past processes are pruned to the newest
// traceKeepFiles (a still-running server's log is never pruned). The log holds raw ACP — prompts,
// tool output, file contents — so treat it as sensitive and don't share it raw.

const (
	traceEnvVar     = "COOP_ACP_TRACE"
	traceSentinel   = "acp-debug" // <config>/acp-debug toggles tracing on
	traceMaxLineLen = 2000        // cap a wire line (ACP carries file contents) so the log stays readable
)

// Vars, not consts, so tests can shrink them.
var (
	traceMaxBytes  int64 = 25 << 20 // rotate a trace file to .log.1 once it passes this (~2x this per server)
	traceKeepFiles       = 5        // on startup, keep this many newest acp-trace logs (plus any still-running)
)

var (
	traceMu      sync.Mutex
	traceOut     io.Writer // the open log, set once tracing turns on
	traceWritten int64     // bytes in the current (unrotated) file, for the size cap
	traceGaveUp  bool      // enabled but the log couldn't be opened — stop retrying
	traceLastAt  time.Time // last gate check while off, to throttle the sentinel stat
)

// traceLog returns the trace destination, or nil when tracing is off. Caller must hold traceMu.
func traceLog() io.Writer {
	if traceOut != nil || traceGaveUp {
		return traceOut
	}
	now := time.Now()
	if !traceLastAt.IsZero() && now.Sub(traceLastAt) < time.Second {
		return nil // checked recently, still off — don't stat the sentinel on every wire line
	}
	traceLastAt = now
	if os.Getenv(traceEnvVar) == "" && !traceSentinelExists() {
		return nil
	}
	path := tracePath()
	_ = os.MkdirAll(filepath.Dir(path), 0o755) // config dir normally exists; create it if not
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "coop acp: trace enabled but couldn't open %s: %v\n", path, err)
		traceGaveUp = true
		return nil
	}
	fmt.Fprintf(os.Stderr, "coop acp: tracing the ACP wire to %s\n", path)
	traceOut = f
	if fi, err := f.Stat(); err == nil {
		traceWritten = fi.Size() // account for anything already there (re-opened same-pid file)
	}
	pruneOldTraces(path) // once per process: bound the number of leftover logs
	return f
}

// writeLocked appends one line (no trailing newline) to the trace, rotating to .log.1 first if the
// line would push the current file past the cap. Caller holds traceMu and has confirmed tracing is on.
func writeLocked(s string) {
	line := []byte(s + "\n")
	w := traceOut
	if traceWritten+int64(len(line)) > traceMaxBytes {
		if w = rotateLocked(); w == nil {
			return
		}
	}
	n, _ := w.Write(line)
	traceWritten += int64(n)
}

// rotateLocked renames the current log to <path>.1 (replacing any prior backup) and reopens a fresh
// one, so a long-lived server's trace never exceeds ~2×traceMaxBytes. Caller holds traceMu.
func rotateLocked() io.Writer {
	f, ok := traceOut.(*os.File)
	if !ok {
		return traceOut // not a real file (shouldn't happen) — skip rotation
	}
	path := f.Name()
	_ = f.Close()
	_ = os.Rename(path, path+".1")
	nf, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "coop acp: trace rotate failed: %v\n", err)
		traceOut, traceGaveUp = nil, true
		return nil
	}
	traceOut, traceWritten = nf, 0
	return nf
}

// pruneOldTraces bounds the directory: keep our own new log plus the newest traceKeepFiles-1 others,
// deleting older ones (and their .1). A log whose pid is still running is never deleted.
func pruneOldTraces(keep string) {
	dir := traceConfigDir()
	if dir == "" {
		return
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "acp-trace-*.log"))
	type ent struct {
		path string
		mod  time.Time
		pid  int
	}
	var ents []ent
	for _, m := range matches {
		fi, err := os.Stat(m)
		if err != nil {
			continue
		}
		ents = append(ents, ent{m, fi.ModTime(), tracePID(m)})
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].mod.After(ents[j].mod) })
	kept := 1 // our own new log
	for _, e := range ents {
		if e.path == keep || (e.pid > 0 && pidAlive(e.pid)) {
			continue // never prune our log or a still-running server's
		}
		if kept < traceKeepFiles {
			kept++
			continue
		}
		_ = os.Remove(e.path)
		_ = os.Remove(e.path + ".1")
	}
}

// tracePID extracts <pid> from an acp-trace-<pid>.log path, or 0 if it doesn't parse.
func tracePID(path string) int {
	name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "acp-trace-"), ".log")
	n, err := strconv.Atoi(name)
	if err != nil {
		return 0
	}
	return n
}

// pidAlive reports whether a process with pid exists (signal 0 probes without delivering a signal).
func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM) // EPERM: exists but not ours to signal
}

// traceConfigDir mirrors coop's config base (XDG_CONFIG_HOME else ~/.config, + /coop) so traces land
// beside the config. Empty only if the home dir can't be resolved.
func traceConfigDir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "coop")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "coop")
}

func traceSentinelExists() bool {
	dir := traceConfigDir()
	if dir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, traceSentinel))
	return err == nil
}

// tracePath is <config>/acp-trace-<pid>.log — per-pid so concurrent `coop acp` servers (a claude, a
// codex, …) don't interleave into one file. Falls back to the temp dir if config can't be resolved.
func tracePath() string {
	dir := traceConfigDir()
	if dir == "" {
		dir = os.TempDir()
	}
	return filepath.Join(dir, fmt.Sprintf("acp-trace-%d.log", os.Getpid()))
}

// Trace records a formatted diagnostic event (a restart, a box spawn). Exported so the CLI factory
// can note which credential/preset each (re)spawn runs on. A no-op when tracing is off.
func Trace(format string, args ...any) {
	traceMu.Lock()
	defer traceMu.Unlock()
	if traceLog() == nil {
		return
	}
	writeLocked(fmt.Sprintf("%s | %s", time.Now().Format("15:04:05.000"), fmt.Sprintf(format, args...)))
}

// traceLine records one wire line with a direction tag, dropping the trailing newline and capping
// very long lines (file contents / prompts) so the log stays readable.
func traceLine(tag string, line []byte) {
	traceMu.Lock()
	defer traceMu.Unlock()
	if traceLog() == nil {
		return
	}
	s := line
	if n := len(s); n > 0 && s[n-1] == '\n' {
		s = s[:n-1]
	}
	ts := time.Now().Format("15:04:05.000")
	if len(s) > traceMaxLineLen {
		writeLocked(fmt.Sprintf("%s | %s %s…(%d bytes)", ts, tag, s[:traceMaxLineLen], len(s)))
		return
	}
	writeLocked(fmt.Sprintf("%s | %s %s", ts, tag, s))
}
