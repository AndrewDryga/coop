package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/testutil/procharness"
)

type loopScenario struct {
	TaskID   string        `json:"task_id"`
	Attempts []loopAttempt `json:"attempts"`
}

type loopAttempt struct {
	Target string `json:"target"`
	Result string `json:"result"`
}

type loopCursor struct {
	Index int `json:"index"`
}

type loopLeaseMetadata struct {
	Version       int       `json:"version"`
	RunID         string    `json:"run_id"`
	ControllerPID int       `json:"controller_pid"`
	Provider      string    `json:"provider"`
	Target        string    `json:"target"`
	AcquiredAt    time.Time `json:"acquired_at"`
	HeartbeatAt   time.Time `json:"heartbeat_at"`
}

func validateLoopScenario(provider string, homes map[string]bool, plan loopScenario) error {
	if !safeLoopTaskID(plan.TaskID) {
		return fmt.Errorf("unsafe loop task id %q", plan.TaskID)
	}
	if len(plan.Attempts) == 0 || len(plan.Attempts) > 16 {
		return errors.New("loop scenario requires 1 to 16 attempts")
	}
	first, err := loopAttemptTarget(0, plan.Attempts[0])
	if err != nil {
		return err
	}
	if first.Provider != provider {
		return fmt.Errorf("loop scenario provider %q does not match first attempt %q", provider, first.Provider)
	}
	for i, attempt := range plan.Attempts {
		target, err := loopAttemptTarget(i, attempt)
		if err != nil {
			return err
		}
		if !homes[target.Provider] {
			return fmt.Errorf("loop attempt %d provider %q has no projected home", i, target.Provider)
		}
		if err := validateLoopResult(i, attempt.Result); err != nil {
			return err
		}
	}
	return nil
}

func loopAttemptTarget(index int, attempt loopAttempt) (agents.Target, error) {
	target, err := agents.ParseTarget(attempt.Target)
	if err != nil || target.Model == "" || len(target.Accounts) != 1 || target.Account() == "" || target.String() != attempt.Target {
		return agents.Target{}, fmt.Errorf("loop attempt %d target %q is not one canonical provider/model/account target", index, attempt.Target)
	}
	return target, nil
}

func validateLoopResult(index int, result string) error {
	switch result {
	case "complete", "complete-wait", "unbound", "unbound-wait", "unbound-log-symlink", "unbound-state-symlink", "repair-binding",
		"rate-limit", "output-limit", "authentication", "ordinary", "ambiguous-limit-prose", "ambiguous-auth-prose", "malformed", "truncated", "wait":
		return nil
	default:
		return fmt.Errorf("loop attempt %d result %q is unsupported", index, result)
	}
}

func safeLoopTaskID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for i, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			continue
		}
		if r != '-' || i == 0 || i == len(id)-1 {
			return false
		}
	}
	return true
}

func consumeLoopAttempt(root, provider string, plan loopScenario) (loopAttempt, error) {
	f, err := procharness.OpenRegularFile(root, filepath.Join(root, "state", "loop-cursor.json"), os.O_RDWR)
	if err != nil {
		return loopAttempt{}, err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return loopAttempt{}, err
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	var cursor loopCursor
	decoder := json.NewDecoder(io.LimitReader(f, 4<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil || cursor.Index < 0 || cursor.Index >= len(plan.Attempts) {
		return loopAttempt{}, errors.New("loop cursor is invalid or exhausted")
	}
	index := cursor.Index
	attempt := plan.Attempts[index]
	target, err := loopAttemptTarget(index, attempt)
	if err != nil {
		return loopAttempt{}, err
	}
	if target.Provider != provider {
		return loopAttempt{}, fmt.Errorf("loop attempt %d expected provider %q, got %q", index, target.Provider, provider)
	}
	cursor.Index++
	if err := f.Truncate(0); err != nil {
		return loopAttempt{}, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return loopAttempt{}, err
	}
	if err := json.NewEncoder(f).Encode(cursor); err != nil {
		return loopAttempt{}, err
	}
	if err := f.Sync(); err != nil {
		return loopAttempt{}, err
	}
	return attempt, nil
}

func serveLoopAttempt(root, trace, provider string, plan loopScenario) (int, string, error) {
	attempt, err := consumeLoopAttempt(root, provider, plan)
	if err != nil {
		return 1, "", err
	}
	switch attempt.Result {
	case "complete", "complete-wait", "unbound", "unbound-wait", "unbound-log-symlink", "unbound-state-symlink", "repair-binding":
		outcome := attempt.Result
		if outcome == "complete" || outcome == "complete-wait" {
			outcome = ""
		} else if outcome == "unbound-wait" {
			outcome = "unbound"
		}
		if err := serveLoopWorker(root, provider, plan.TaskID, attempt.Target, outcome); err != nil {
			return 1, "", err
		}
		fmt.Fprintln(os.Stdout, "fixture-loop-complete-"+provider)
		if strings.HasSuffix(attempt.Result, "-wait") {
			return waitLoopSignal(root, trace)
		}
		return 0, "", nil
	case "rate-limit":
		fmt.Fprintln(os.Stderr, "usage limit reached")
		fmt.Fprintln(os.Stderr, "retry-after: 3600")
	case "output-limit":
		fmt.Fprintln(os.Stderr, "maximum output length")
	case "authentication":
		fmt.Fprintln(os.Stderr, loopAuthenticationFailure(provider))
	case "ordinary":
		fmt.Fprintln(os.Stdout, "fixture ordinary provider failure")
	case "ambiguous-limit-prose":
		fmt.Fprintln(os.Stdout, "error: rate limited")
	case "ambiguous-auth-prose":
		fmt.Fprintln(os.Stdout, loopAuthenticationFailure(provider))
	case "malformed":
		fmt.Fprintln(os.Stdout, `{"type":not-json}`)
	case "truncated":
		fmt.Fprint(os.Stdout, `{"type":"result"`)
	case "wait":
		return waitLoopSignal(root, trace)
	}
	return 23, "", nil
}

func waitLoopSignal(root, trace string) (int, string, error) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	if err := record(root, trace, traceRecord{Source: "provider", Event: "ready", PID: os.Getpid()}); err != nil {
		return 1, "", err
	}
	got := <-signals
	sig, _ := got.(syscall.Signal)
	return 128 + int(sig), got.String(), nil
}

func loopAuthenticationFailure(provider string) string {
	switch provider {
	case "claude":
		return "not logged in"
	case "codex":
		return "authentication required"
	case "gemini":
		return "manual authorization is required"
	case "grok":
		return "not signed in"
	default:
		return "authentication failed"
	}
}

func serveLoopWorker(root, provider, taskID, target, outcome string) error {
	repo, err := os.Getwd()
	if err != nil {
		return err
	}
	repo, err = procharness.CanonicalUnderRoot(root, repo)
	if err != nil {
		return fmt.Errorf("loop repo: %w", err)
	}
	tasks := filepath.Join(repo, ".agent", "tasks")
	inProgress, err := procharness.CanonicalUnderRoot(root, filepath.Join(tasks, "10_in_progress"))
	if err != nil {
		return fmt.Errorf("loop in-progress queue: %w", err)
	}
	done, err := procharness.CanonicalUnderRoot(root, filepath.Join(tasks, "99_done"))
	if err != nil {
		return fmt.Errorf("loop done queue: %w", err)
	}
	entries, err := os.ReadDir(inProgress)
	if err != nil {
		return err
	}
	if len(entries) != 1 || entries[0].Name() != taskID || !entries[0].IsDir() {
		return fmt.Errorf("loop assignment is not exactly task %q", taskID)
	}
	taskDir, err := procharness.CanonicalUnderRoot(root, filepath.Join(inProgress, taskID))
	if err != nil {
		return fmt.Errorf("loop task: %w", err)
	}
	if err := verifyLoopLease(root, taskDir, provider, target); err != nil {
		return err
	}

	change := "loop-" + provider + ".txt"
	if outcome == "repair-binding" {
		f, err := procharness.OpenRegularFile(root, filepath.Join(repo, change), os.O_RDONLY)
		if err != nil {
			return fmt.Errorf("read existing loop change: %w", err)
		}
		data, readErr := io.ReadAll(io.LimitReader(f, 1<<10))
		closeErr := f.Close()
		if readErr != nil || closeErr != nil || string(data) != "completed by "+provider+"\n" {
			return errors.Join(readErr, closeErr, errors.New("existing loop change mismatch"))
		}
		commitArgs := []string{"commit", "--amend", "--no-edit", "--trailer", "Coop-Task: " + taskID}
		bound, err := loopHeadBoundToTask(repo, taskID)
		if err != nil {
			return err
		}
		if bound {
			commitArgs = append(commitArgs, "--trailer", fmt.Sprintf("Coop-Recovery: fixture-%d", time.Now().UnixNano()))
		}
		if err := runLoopGit(repo, commitArgs...); err != nil {
			return err
		}
	} else {
		repoRoot, err := os.OpenRoot(repo)
		if err != nil {
			return err
		}
		f, err := repoRoot.OpenFile(change, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_, err = fmt.Fprintf(f, "completed by %s\n", provider)
			if closeErr := f.Close(); err == nil {
				err = closeErr
			}
		}
		repoRoot.Close()
		if err != nil {
			return fmt.Errorf("write loop change: %w", err)
		}
		if err := runLoopGit(repo, "add", "--", change); err != nil {
			return err
		}
		commitArgs := []string{"commit", "-q", "-m", "fixture: complete " + provider + " loop task"}
		if outcome == "" {
			commitArgs = append(commitArgs, "-m", "Coop-Task: "+taskID)
		}
		if err := runLoopGit(repo, commitArgs...); err != nil {
			return err
		}
	}

	state := fmt.Sprintf("# State - %s\n\n**Status:** complete\n**Done so far:** fixture completed the %s loop lifecycle\n**Next action:** none\n**Traps:** none\n", taskID, provider)
	if err := writeLoopTaskFile(root, filepath.Join(taskDir, "state.md"), state, false); err != nil {
		return err
	}
	logLine := fmt.Sprintf("\n## Fixture completion\n- %s completed the closed loop lifecycle.\n", provider)
	if err := writeLoopTaskFile(root, filepath.Join(taskDir, "log.md"), logLine, true); err != nil {
		return err
	}
	name := ""
	switch outcome {
	case "unbound-log-symlink":
		name = "log.md"
	case "unbound-state-symlink":
		name = "state.md"
	}
	if name != "" {
		path := filepath.Join(taskDir, name)
		if err := os.Remove(path); err != nil {
			return err
		}
		if err := os.Symlink(filepath.Join(root, "state", "recovery-sentinel"), path); err != nil {
			return err
		}
	}
	dest := filepath.Join(done, taskID)
	if _, err := os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return fmt.Errorf("loop done task %q already exists", taskID)
		}
		return err
	}
	if err := os.Rename(taskDir, dest); err != nil {
		return fmt.Errorf("complete loop task: %w", err)
	}
	return nil
}

func verifyLoopLease(root, taskDir, provider, target string) error {
	lock, err := procharness.OpenRegularFile(root, filepath.Join(taskDir, "tmp", "lease.lock"), os.O_RDWR)
	if err != nil {
		return fmt.Errorf("loop lease lock: %w", err)
	}
	locked := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if locked == nil {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		lock.Close()
		return errors.New("loop task lease is not held by the controller")
	}
	lock.Close()
	if !errors.Is(locked, syscall.EWOULDBLOCK) && !errors.Is(locked, syscall.EAGAIN) {
		return fmt.Errorf("probe loop task lease: %w", locked)
	}
	metaFile, err := procharness.OpenRegularFile(root, filepath.Join(taskDir, "tmp", "lease.json"), os.O_RDONLY)
	if err != nil {
		return fmt.Errorf("loop lease metadata: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(metaFile, 8<<10))
	closeErr := metaFile.Close()
	if readErr != nil || closeErr != nil {
		return errors.Join(readErr, closeErr)
	}
	var meta loopLeaseMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return fmt.Errorf("decode loop lease metadata: %w", err)
	}
	if meta.Version != 1 || meta.RunID == "" || meta.ControllerPID <= 1 || meta.Provider != provider || meta.Target != target || meta.AcquiredAt.IsZero() || meta.HeartbeatAt.IsZero() {
		return fmt.Errorf("loop lease metadata does not identify %s on %s", provider, target)
	}
	return nil
}

func writeLoopTaskFile(root, path, body string, appendOnly bool) error {
	flag := os.O_WRONLY | os.O_TRUNC
	if appendOnly {
		flag = os.O_WRONLY | os.O_APPEND
	}
	f, err := procharness.OpenRegularFile(root, path, flag)
	if err != nil {
		return err
	}
	_, writeErr := io.WriteString(f, body)
	return errors.Join(writeErr, f.Close())
}

func runLoopGit(repo string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func loopHeadBoundToTask(repo, taskID string) (bool, error) {
	format := "%(trailers:key=Coop-Task,valueonly,separator=%x1f)"
	cmd := exec.Command("git", "log", "-1", "--format="+format)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("inspect task binding: %w: %s", err, strings.TrimSpace(string(out)))
	}
	values := strings.Split(strings.TrimSpace(string(out)), "\x1f")
	return len(values) == 1 && strings.TrimSpace(values[0]) == taskID, nil
}
