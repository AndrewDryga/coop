package forkctl

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/ui"
)

func ForkNextSteps(name string) {
	ui.Steps(
		fmt.Sprintf("coop fork review %s", name),
		fmt.Sprintf("coop fork merge %s", name),
		fmt.Sprintf("coop fork rm %s", name),
	)
}

// forkAgentFile records which agent a fork was created/last run with — inside the fork,
// but git-excluded so it never lands. Re-entry without an explicit agent reads it back.
func forkAgentFile(ws string) string { return filepath.Join(ws, ".coop", "agent") }

func ReadForkAgent(ws string) string {
	if a := readForkMeta(ws, forkAgentFile(ws)); agents.Valid(a) {
		return a
	}
	return ""
}

func SaveForkAgent(ws, agent string) error { return saveForkMeta(ws, forkAgentFile(ws), agent) }

func forkTargetFile(ws string) string { return filepath.Join(ws, ".coop", "target") }

// SaveForkTarget notes the full target (provider[:model][/effort][@account]) the fork runs when a
// launch names none: the one it was made with, or last switched to by naming another. Only a launch
// that names a target writes it; a resume reads it back and leaves it alone, so no resume can swap
// the model or account for a default. coop tasks watch shows it for a fork whose loop holds no task
// right now (tasks.forkTarget reads it).
func SaveForkTarget(ws, target string) error {
	return saveForkMeta(ws, forkTargetFile(ws), target)
}

// ReadForkTarget is the target a launch with no target runs: the saved one, else the saved agent
// alone. The agent note is rewritten first when a launch switches agents, so a target that names
// another agent is a leftover and is ignored. "" when the fork has no usable agent note.
func ReadForkTarget(ws string) string {
	agent := ReadForkAgent(ws)
	if agent == "" {
		return ""
	}
	if target := readForkMeta(ws, forkTargetFile(ws)); targetProvider(target) == agent {
		return target
	}
	return agent
}

// targetProvider is the provider slot of a target: what comes before the first ':', '/' or '@'.
func targetProvider(target string) string {
	if i := strings.IndexAny(target, ":/@"); i >= 0 {
		return target[:i]
	}
	return target
}

// forkStartErrorFile notes why a fork's first start stopped before its agent ran, so coop fork ls
// can say the fork never started instead of listing it as idle. It lives beside the agent file.
func forkStartErrorFile(ws string) string { return filepath.Join(ws, ".coop", "start-error") }

// ReadForkStartError is that note, or "" when the fork's agent has run (or the note is unreadable:
// it is a hint in a folder the agent can write, so a reader escapes it).
func ReadForkStartError(ws string) string { return readForkMeta(ws, forkStartErrorFile(ws)) }

// ClearForkStartError removes the note once a launch hands the fork to an agent.
func ClearForkStartError(ws string) error { return removeForkMeta(ws, forkStartErrorFile(ws)) }

// NoteForkStart keeps the note true after an interactive launch: a launch whose agent ran removes
// it, and one that stopped before the agent ran records why, if no agent has ever run in this fork
// (firstStart). The note is only a hint for coop fork ls and the launch already reported its own
// result, so a note that cannot be written is left out rather than replacing that result.
func NoteForkStart(ws string, firstStart, agentRan bool, err error) {
	switch {
	case agentRan:
		_ = ClearForkStartError(ws)
		_ = ClearForkLoopError(ws)
	case firstStart && err != nil:
		_ = saveForkMeta(ws, forkStartErrorFile(ws), startFailure(err))
	}
}

func forkLoopErrorFile(ws string) string { return filepath.Join(ws, ".coop", "loop-error") }

// forkLoopEndFile notes why the fork's last loop ended with nothing to take, so coop fork ls can say
// why a fork that started a loop is idle. Like loop-error it is a bounded, agent-writable display
// hint, never authority.
func forkLoopEndFile(ws string) string { return filepath.Join(ws, ".coop", "loop-end") }

// ClearForkLoopError starts a new result once this process owns the worker/controller, or an
// interactive agent has run: the failure and the note of how the loop before ended. Clear before a
// stop can hold the lock and prevent exit bookkeeping.
func ClearForkLoopError(ws string) error {
	return errors.Join(removeForkMeta(ws, forkLoopErrorFile(ws)), removeForkMeta(ws, forkLoopEndFile(ws)))
}

// NoteForkLoopEnd records why a loop that took no task ended, for coop fork ls. The loop said the same
// in its own output, which is what its starter reads, so a note that cannot be written is left out.
func NoteForkLoopEnd(ws string, identity forkspace.Identity, reason string) {
	_ = saveForkMeta(ws, forkLoopEndFile(ws), string(identity.Generation)+"\n"+startFailure(errors.New(reason)))
}

func readForkLoopEnd(ws string, generation forkspace.Generation) string {
	stored, reason, ok := strings.Cut(readForkMeta(ws, forkLoopEndFile(ws)), "\n")
	if !ok || !forkspace.ValidGeneration(generation) || stored != string(generation) {
		return ""
	}
	return startFailure(errors.New(reason))
}

// NoteForkLoopExit preserves a terminal failure after liveness records disappear. Like start-error,
// this is only a bounded, agent-writable display hint, never lifecycle or approval authority.
// Record it before releasing the controller/PID; never wait on a stop that is waiting for us.
func NoteForkLoopExit(repo string, identity forkspace.Identity, code int, err error) {
	unlock, ok := forkspace.TryLockState(repo, identity.Name)
	if !ok {
		return
	}
	defer unlock()
	if current, exists, readErr := forkspace.ReadGeneration(repo, identity.Name); readErr != nil || !exists || current != identity {
		return
	}
	if forkspace.ValidateGenerationWorkspace(repo, identity) != nil {
		return
	}
	// A second start's refusal must not overwrite the running worker's eventual result.
	if worker, readErr := forkspace.ReadWorkerState(repo, identity.Name); readErr == nil && worker.Pid != os.Getpid() {
		return
	}
	ws := forkspace.Workspace(repo, identity.Name)
	// Nil-error outcomes include work remaining (1) and a human decision (3). A review can also
	// return a reported interruption error; neither is a failed loop. A loop that ended with nothing to
	// take keeps its note of that (NoteForkLoopEnd): it is not a failure to clear.
	if err == nil || code == 130 || code == 143 {
		_ = removeForkMeta(ws, forkLoopErrorFile(ws))
		return
	}
	_ = saveForkMeta(ws, forkLoopErrorFile(ws), string(identity.Generation)+"\n"+startFailure(err))
}

func readForkLoopError(ws string, generation forkspace.Generation) string {
	stored, reason, ok := strings.Cut(readForkMeta(ws, forkLoopErrorFile(ws)), "\n")
	if !ok || !forkspace.ValidGeneration(generation) || stored != string(generation) {
		return ""
	}
	return startFailure(errors.New(reason))
}

// startFailure is what the note keeps of a failed start: a refusal block's headline, else the
// error's first line, bounded so the listing stays one line.
func startFailure(err error) string {
	msg := err.Error()
	var usage *ui.UsageError
	if errors.As(err, &usage) {
		msg = usage.Headline
	}
	msg, _, _ = strings.Cut(msg, "\n")
	msg = strings.TrimSpace(msg)
	if r := []rune(msg); len(r) > 300 {
		msg = string(r[:299]) + "…"
	}
	return msg
}

const forkMetadataFileLimit = 4 << 10

// Fork metadata is provider-writable between launches. Reads and writes reuse the task metadata
// no-follow root/file primitives so a planted .coop or file symlink cannot reach a host path.
// Reads remain best-effort hints; launch boundaries require writes so exact resume state cannot be
// lost silently.
func readForkMeta(ws, path string) string {
	meta := filepath.Join(ws, ".coop")
	if filepath.Dir(path) != meta {
		return ""
	}
	root, err := tasks.OpenTaskMetadataRoot(meta)
	if err != nil {
		return ""
	}
	defer root.Close()
	data, err := tasks.ReadTaskMetadataFile(root, filepath.Base(path))
	if err != nil || len(data) > forkMetadataFileLimit {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func saveForkMeta(ws, path, value string) error {
	meta := filepath.Join(ws, ".coop")
	if value == "" {
		return fmt.Errorf("write fork metadata %s: value is empty", path)
	}
	if len(value) > forkMetadataFileLimit {
		return fmt.Errorf("write fork metadata %s: value exceeds %d bytes", path, forkMetadataFileLimit)
	}
	if filepath.Dir(path) != meta {
		return fmt.Errorf("write fork metadata %s: path is outside %s", path, meta)
	}
	if err := os.Mkdir(meta, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create fork metadata directory %s: %w", meta, err)
	}
	root, err := tasks.OpenTaskMetadataRoot(meta)
	if err != nil {
		return fmt.Errorf("open fork metadata directory %s: %w", meta, err)
	}
	defer root.Close()
	if err := tasks.AtomicWriteTaskFile(root, filepath.Base(path), []byte(value+"\n")); err != nil {
		return fmt.Errorf("write fork metadata %s: %w", path, err)
	}
	return nil
}

// ForkSessionFile records the coop-owned session id for a fork+agent+account,
// inside the fork but git-excluded, so re-entry resumes exactly that session.
func ForkSessionFile(ws, agent, account string) string {
	return filepath.Join(ws, ".coop", "session."+agent+"."+account)
}

func ReadForkSession(ws, agent, account string) string {
	id := readForkMeta(ws, ForkSessionFile(ws, agent, account))
	if !agents.ValidSessionID(id) {
		return ""
	}
	return id
}

func SaveForkSession(ws, agent, account, id string) error {
	return saveForkMeta(ws, ForkSessionFile(ws, agent, account), id)
}

func ClearForkSession(ws, agent, account string) error {
	return removeForkMeta(ws, ForkSessionFile(ws, agent, account))
}

// removeForkMeta deletes one metadata file through the same no-follow root as reads and writes; a
// file that is already gone is not an error.
func removeForkMeta(ws, path string) error {
	meta := filepath.Join(ws, ".coop")
	root, err := tasks.OpenTaskMetadataRoot(meta)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open fork metadata directory %s: %w", meta, err)
	}
	defer root.Close()
	if err := root.Remove(filepath.Base(path)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear fork metadata %s: %w", path, err)
	}
	return nil
}
