// Package procharness provides isolated, bounded external-process helpers for Coop's
// scripted end-to-end tests. It is test infrastructure only; it does not emulate a
// container boundary.
package procharness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Layout is the complete host state owned by one scripted process test.
type Layout struct {
	Root      string
	Bin       string
	Home      string
	XDGConfig string
	XDGCache  string
	XDGState  string
	Tmp       string
	Config    string
	Repo      string
	Plans     string
	State     string
	Trace     string
	GitConfig string
}

// NewLayout creates an isolated state tree beneath root. Root must already exist.
func NewLayout(root string) (Layout, error) {
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return Layout{}, fmt.Errorf("resolve process-test root: %w", err)
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return Layout{}, fmt.Errorf("make process-test root absolute: %w", err)
	}
	l := Layout{
		Root: canonical, Bin: filepath.Join(canonical, "bin"), Home: filepath.Join(canonical, "home"),
		XDGConfig: filepath.Join(canonical, "xdg", "config"), XDGCache: filepath.Join(canonical, "xdg", "cache"),
		XDGState: filepath.Join(canonical, "xdg", "state"), Tmp: filepath.Join(canonical, "tmp"),
		Config: filepath.Join(canonical, "config"), Repo: filepath.Join(canonical, "repo"),
		Plans: filepath.Join(canonical, "plans"), State: filepath.Join(canonical, "state"),
	}
	l.Trace = filepath.Join(l.State, "trace.jsonl")
	l.GitConfig = filepath.Join(l.State, "gitconfig")
	for _, dir := range []string{l.Root, l.Bin, l.Home, l.XDGConfig, l.XDGCache, l.XDGState, l.Tmp, l.Config, l.Repo, l.Plans, l.State} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return Layout{}, fmt.Errorf("create process-test state %s: %w", dir, err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return Layout{}, fmt.Errorf("secure process-test state %s: %w", dir, err)
		}
	}
	for _, path := range []string{l.Trace, l.GitConfig} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			return Layout{}, fmt.Errorf("create process-test file %s: %w", path, err)
		}
	}
	return l, nil
}

// Environment builds an allowlist-only child environment. It never reads the ambient
// process environment; callers add explicit test-owned values through extra.
func Environment(layout Layout, extra map[string]string) ([]string, error) {
	values := map[string]string{
		"HOME": layout.Home, "XDG_CONFIG_HOME": layout.XDGConfig, "XDG_CACHE_HOME": layout.XDGCache,
		"XDG_STATE_HOME": layout.XDGState, "TMPDIR": layout.Tmp,
		"GIT_CONFIG_GLOBAL": layout.GitConfig, "GIT_CONFIG_NOSYSTEM": "1",
		"LANG": "C", "LC_ALL": "C", "TERM": "dumb", "TZ": "UTC",
		"COOP_CONF": filepath.Join(layout.Config, "missing.conf"), "COOP_CONFIG_DIR": layout.Config,
		"COOP_MCP_FILE": filepath.Join(layout.Config, "missing-mcp.json"), "COOP_REPO": layout.Repo,
		"COOP_HOMES": "0", "COOP_NETWORK": "0", "COOP_AUTO_UP": "0", "COOP_CACHE": "0",
		"COOP_CAFFEINATE": "0", "COOP_EGRESS": "none", "COOP_NO_UPDATE_CHECK": "1",
		"COOP_RUN_ARGS": "", "COOP_MEMORY": "", "COOP_CPUS": "",
	}
	allowed := map[string]bool{
		"PATH": true, "TERM": true, "TZ": true,
		"COOP_RUNTIME": true, "COOP_IMAGE": true, "COOP_HOMES": true,
		"COOP_PROVIDER_FIXTURE_ROOT": true, "COOP_PROVIDER_FIXTURE_IMAGE": true,
		"COOP_PROVIDER_FIXTURE_TRACE": true, "COOP_PROVIDER_FIXTURE_SCENARIO": true,
	}
	for key, value := range extra {
		if key == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, '\x00') {
			return nil, fmt.Errorf("invalid environment entry %q", key)
		}
		if !allowed[key] {
			return nil, fmt.Errorf("environment key %s is not allowlisted for scripted process tests", key)
		}
		values[key] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}
	return env, nil
}

// CanonicalUnderRoot verifies that an existing path is an absolute, non-symlinked
// descendant of root and returns its canonical spelling.
func CanonicalUnderRoot(root, path string) (string, error) {
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve root: %w", err)
	}
	canonicalRoot, err = filepath.Abs(canonicalRoot)
	if err != nil {
		return "", fmt.Errorf("make root absolute: %w", err)
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("path %q is not absolute", path)
	}
	clean := filepath.Clean(path)
	rel, err := filepath.Rel(canonicalRoot, clean)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes fixture root %q", path, canonicalRoot)
	}
	current := canonicalRoot
	if rel != "." {
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			if part == "" || part == "." || part == ".." {
				return "", fmt.Errorf("path %q has unsafe component %q", path, part)
			}
			current = filepath.Join(current, part)
			info, statErr := os.Lstat(current)
			if statErr != nil {
				return "", fmt.Errorf("inspect path %q: %w", current, statErr)
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return "", fmt.Errorf("path %q contains symlink %q", path, current)
			}
		}
	}
	return clean, nil
}

// OpenRegularFile opens an existing root-owned control file without following its
// final symlink and refuses hardlinks. It is for scenarios, env files, and traces;
// mount directories use CanonicalUnderRoot instead.
func OpenRegularFile(root, path string, flag int) (*os.File, error) {
	clean, err := CanonicalUnderRoot(root, path)
	if err != nil {
		return nil, err
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve root: %w", err)
	}
	rel, err := filepath.Rel(canonicalRoot, clean)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("control file %q is not beneath root %q", path, canonicalRoot)
	}
	r, err := os.OpenRoot(canonicalRoot)
	if err != nil {
		return nil, fmt.Errorf("open fixture root: %w", err)
	}
	defer r.Close()
	before, err := r.Lstat(rel)
	if err != nil {
		return nil, fmt.Errorf("inspect control file %q: %w", path, err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, fmt.Errorf("control file %q is not a regular non-symlink", path)
	}
	f, err := r.OpenFile(rel, flag|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open control file %q: %w", path, err)
	}
	after, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("inspect opened control file %q: %w", path, err)
	}
	if !os.SameFile(before, after) || !after.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("control file %q changed while opening", path)
	}
	stat, ok := after.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 {
		f.Close()
		return nil, fmt.Errorf("control file %q must have exactly one hardlink", path)
	}
	return f, nil
}

// Command describes one managed external process.
type Command struct {
	Path       string
	Args       []string
	Dir        string
	Env        []string
	ExtraFiles []*os.File
	MaxOutput  int
	KillGrace  time.Duration
	// BeforeCancel runs after the deadline is observed but before the first signal. Live tests use
	// it to revoke projected credentials before any independently executing producer is stopped.
	BeforeCancel func() error
}

// Result is the bounded output and terminal state of a managed process.
type Result struct {
	PID                              int
	ExitCode                         int
	Err                              error
	Stdout, Stderr                   string
	StdoutTruncated, StderrTruncated bool
}

// Process is one started command whose process group remains owned by the harness until Wait.
// Tests use the split lifecycle only when they must synchronize on an external ready event before
// delivering a signal; ordinary tests should use Run.
type Process struct {
	cmd            *exec.Cmd
	done           <-chan error
	stdout, stderr *boundedBuffer
	grace          time.Duration
	beforeCancel   func() error
	waitOnce       sync.Once
	result         Result
}

// Start launches a managed command in its own process group. The caller should immediately defer
// Cleanup, then call Wait when it wants the result. Both are idempotent and share one drain.
func Start(spec Command) (*Process, error) {
	max := spec.MaxOutput
	if max <= 0 {
		max = 1 << 20
	}
	grace := spec.KillGrace
	if grace <= 0 {
		grace = 500 * time.Millisecond
	}
	stdout := newBoundedBuffer(max)
	stderr := newBoundedBuffer(max)
	cmd := exec.Command(spec.Path, spec.Args...)
	cmd.Dir, cmd.Env = spec.Dir, append([]string{}, spec.Env...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.ExtraFiles = append([]*os.File(nil), spec.ExtraFiles...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = grace
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", spec.Path, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	return &Process{
		cmd: cmd, done: done, stdout: stdout, stderr: stderr, grace: grace,
		beforeCancel: spec.BeforeCancel,
	}, nil
}

// PID returns the process-group leader's pid.
func (p *Process) PID() int { return p.cmd.Process.Pid }

// Output returns the bounded output captured so far. It is safe while the process is running and
// lets tests wait on a controller diagnostic without adding timing sleeps.
func (p *Process) Output() (stdout, stderr string) {
	return p.stdout.String(), p.stderr.String()
}

// SignalGroup delivers sig to the process's owned group. It does not wait or clean up; Wait owns
// both, so a test can signal a foreground-style cancellation and then collect its exact result.
func (p *Process) SignalGroup(sig syscall.Signal) error {
	err := syscall.Kill(-p.PID(), sig)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

// Wait drains a started process. Context cancellation performs the same TERM/grace/KILL group
// cleanup as Run. A normal leader exit with surviving descendants is a test failure and is cleaned.
func (p *Process) Wait(ctx context.Context) Result {
	p.waitOnce.Do(func() { p.result = p.wait(ctx) })
	return p.result
}

func (p *Process) wait(ctx context.Context) Result {
	result := Result{PID: p.PID(), ExitCode: -1}
	select {
	case err := <-p.done:
		result.ExitCode, result.Err = processExit(err)
		if processGroupAlive(result.PID) {
			var beforeErr error
			if p.beforeCancel != nil {
				beforeErr = p.beforeCancel()
			}
			signalGroup(result.PID, syscall.SIGKILL)
			cleanupErr := waitGroupGone(result.PID, 2*time.Second)
			survivorErr := fmt.Errorf("process group %d survived leader exit", result.PID)
			result.Err = errors.Join(result.Err, beforeErr, survivorErr, cleanupErr)
		}
	case <-ctx.Done():
		var beforeErr error
		if p.beforeCancel != nil {
			beforeErr = p.beforeCancel()
		}
		result.Err = errors.Join(ctx.Err(), beforeErr, cancelProcessGroup(result.PID, p.done, p.grace))
	}
	return finishResult(result, p.stdout, p.stderr)
}

// Cleanup immediately cancels and drains a process that has not already been waited. It is safe
// to defer immediately after Start; after a normal Wait it simply returns the cached result.
func (p *Process) Cleanup() Result {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return p.Wait(ctx)
}

// Run starts a command in its own process group and always drains Wait. Context
// cancellation terminates the whole group, then escalates to SIGKILL after KillGrace.
func Run(ctx context.Context, spec Command) Result {
	process, err := Start(spec)
	if err != nil {
		return Result{ExitCode: -1, Err: err}
	}
	return process.Wait(ctx)
}

func processExit(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return -1, err
}

func finishResult(result Result, stdout, stderr *boundedBuffer) Result {
	result.Stdout, result.Stderr = stdout.String(), stderr.String()
	result.StdoutTruncated, result.StderrTruncated = stdout.Truncated(), stderr.Truncated()
	return result
}

func signalGroup(pid int, sig syscall.Signal) {
	if syscall.Kill(-pid, sig) != nil {
		_ = syscall.Kill(pid, sig)
	}
}

func cancelProcessGroup(pid int, done <-chan error, grace time.Duration) error {
	signalGroup(pid, syscall.SIGTERM)
	leaderDone := false
	timer := time.NewTimer(grace)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if leaderDone && !processGroupAlive(pid) {
			return nil
		}
		select {
		case <-done:
			leaderDone = true
		case <-ticker.C:
		case <-timer.C:
			if processGroupAlive(pid) {
				signalGroup(pid, syscall.SIGKILL)
			}
			if !leaderDone {
				select {
				case <-done:
					leaderDone = true
				case <-time.After(2 * time.Second):
					return fmt.Errorf("process group leader %d did not reap", pid)
				}
			}
			if err := waitGroupGone(pid, 2*time.Second); err != nil {
				return err
			}
			return nil
		}
	}
}

func waitGroupGone(pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for processGroupAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if processGroupAlive(pid) {
		return fmt.Errorf("process group %d survived cleanup", pid)
	}
	return nil
}

func processGroupAlive(pid int) bool {
	err := syscall.Kill(-pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// ProcessAlive reports whether pid still exists. Tests use it to prove fixture
// descendants recorded in the trace did not survive a managed run.
func ProcessAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

type boundedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func newBoundedBuffer(limit int) *boundedBuffer { return &boundedBuffer{limit: limit} }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	original := len(p)
	remaining := b.limit - b.buf.Len()
	if remaining <= 0 {
		b.truncated = b.truncated || original > 0
		return original, nil
	}
	if len(p) > remaining {
		_, _ = b.buf.Write(p[:remaining])
		b.truncated = true
		return original, nil
	}
	_, _ = b.buf.Write(p)
	return original, nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *boundedBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}
