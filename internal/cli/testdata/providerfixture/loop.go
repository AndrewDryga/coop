package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/testutil/procharness"
)

type loopScenario struct {
	TaskID  string `json:"task_id"`
	Target  string `json:"target"`
	Outcome string `json:"outcome,omitempty"`
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

func validateLoopScenario(provider string, plan loopScenario) error {
	if !safeLoopTaskID(plan.TaskID) {
		return fmt.Errorf("unsafe loop task id %q", plan.TaskID)
	}
	target, err := agents.ParseTarget(plan.Target)
	if err != nil || target.Provider != provider || target.Model == "" || len(target.Accounts) != 1 || target.Account() == "" || target.String() != plan.Target {
		return fmt.Errorf("loop target %q is not one canonical provider/model/account target for %q", plan.Target, provider)
	}
	if plan.Outcome != "" && plan.Outcome != "unbound" && plan.Outcome != "unbound-log-symlink" && plan.Outcome != "unbound-state-symlink" && plan.Outcome != "repair-binding" {
		return fmt.Errorf("loop outcome %q is unsupported", plan.Outcome)
	}
	return nil
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

func serveLoopWorker(root, provider string, plan loopScenario) error {
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
	if len(entries) != 1 || entries[0].Name() != plan.TaskID || !entries[0].IsDir() {
		return fmt.Errorf("loop assignment is not exactly task %q", plan.TaskID)
	}
	taskDir, err := procharness.CanonicalUnderRoot(root, filepath.Join(inProgress, plan.TaskID))
	if err != nil {
		return fmt.Errorf("loop task: %w", err)
	}
	if err := verifyLoopLease(root, taskDir, provider, plan.Target); err != nil {
		return err
	}

	change := "loop-" + provider + ".txt"
	if plan.Outcome == "repair-binding" {
		f, err := procharness.OpenRegularFile(root, filepath.Join(repo, change), os.O_RDONLY)
		if err != nil {
			return fmt.Errorf("read existing loop change: %w", err)
		}
		data, readErr := io.ReadAll(io.LimitReader(f, 1<<10))
		closeErr := f.Close()
		if readErr != nil || closeErr != nil || string(data) != "completed by "+provider+"\n" {
			return errors.Join(readErr, closeErr, errors.New("existing loop change mismatch"))
		}
		if err := runLoopGit(repo, "commit", "--amend", "--no-edit", "--trailer", "Coop-Task: "+plan.TaskID); err != nil {
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
		if plan.Outcome == "" {
			commitArgs = append(commitArgs, "-m", "Coop-Task: "+plan.TaskID)
		}
		if err := runLoopGit(repo, commitArgs...); err != nil {
			return err
		}
	}

	state := fmt.Sprintf("# State - %s\n\n**Status:** complete\n**Done so far:** fixture completed the %s loop lifecycle\n**Next action:** none\n**Traps:** none\n", plan.TaskID, provider)
	if err := writeLoopTaskFile(root, filepath.Join(taskDir, "state.md"), state, false); err != nil {
		return err
	}
	logLine := fmt.Sprintf("\n## Fixture completion\n- %s completed the closed loop lifecycle.\n", provider)
	if err := writeLoopTaskFile(root, filepath.Join(taskDir, "log.md"), logLine, true); err != nil {
		return err
	}
	name := ""
	switch plan.Outcome {
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
	dest := filepath.Join(done, plan.TaskID)
	if _, err := os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return fmt.Errorf("loop done task %q already exists", plan.TaskID)
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
