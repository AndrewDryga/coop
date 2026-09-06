package tasks

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/hostsurface"
)

// TaskFlagsFile records, beside a completed task, the files its commits changed that alter what
// runs on the HOST — a git hook, an editor or agent settings file, a compose file, the Makefile.
// The sandbox contains what an agent does in the box; it cannot contain what your own tools do
// with files the agent left behind, so the board raises a flag on the task until a human has
// looked and acknowledged it. Written by the trusted completion path (the loop, `coop tasks done`,
// a fork landing); cleared only by `coop tasks flags <id> --ack`.
const TaskFlagsFile = "flags.json"

type TaskFlag struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
	Commit string `json:"commit"`
}

type TaskFlags struct {
	Version        int        `json:"version"`
	HostSurfaces   []TaskFlag `json:"host_surfaces"`
	Acknowledged   bool       `json:"acknowledged"`
	AcknowledgedBy string     `json:"acknowledged_by,omitempty"`
	AcknowledgedAt time.Time  `json:"acknowledged_at,omitempty"`
}

const taskFlagsVersion = 1

// recordTaskFlags scans the commits bound to a task (its Coop-Task trailer, reachable from HEAD)
// for host-execution surfaces and writes the flags file when any is found. A repo that cannot be
// read, or a task with no bound commits, records nothing: this is a review aid, never a gate.
func recordTaskFlags(root, taskDir, id string) error {
	repo := gitOut(root, "rev-parse", "--show-toplevel")
	if repo == "" {
		return nil
	}
	var flags []TaskFlag
	seen := map[string]bool{}
	for _, sha := range CommitsForTask(repo, "HEAD", id) {
		listing := gitOut(repo, "show", "--name-status", "--format=", "--no-renames", sha)
		for _, finding := range hostsurface.Findings(listing) {
			if seen[finding.Path] {
				continue
			}
			seen[finding.Path] = true
			flags = append(flags, TaskFlag{Path: finding.Path, Reason: finding.Reason, Commit: sha})
		}
	}
	if len(flags) == 0 {
		return nil
	}
	return writeTaskFlags(taskDir, TaskFlags{Version: taskFlagsVersion, HostSurfaces: flags})
}

func writeTaskFlags(taskDir string, flags TaskFlags) error {
	data, err := json.MarshalIndent(flags, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(taskDir, TaskFlagsFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadTaskFlags returns a task's flags; ok is false when the task carries none.
func ReadTaskFlags(taskDir string) (TaskFlags, bool, error) {
	data, err := os.ReadFile(filepath.Join(taskDir, TaskFlagsFile))
	if errors.Is(err, os.ErrNotExist) {
		return TaskFlags{}, false, nil
	}
	if err != nil {
		return TaskFlags{}, false, err
	}
	var flags TaskFlags
	if err := json.Unmarshal(data, &flags); err != nil || flags.Version != taskFlagsVersion {
		return TaskFlags{}, false, fmt.Errorf("task flags file %s is malformed", filepath.Join(taskDir, TaskFlagsFile))
	}
	return flags, true, nil
}

// taskHasOpenFlags is the cheap board question: does this folder carry flags nobody acknowledged?
func taskHasOpenFlags(taskDir string) bool {
	flags, ok, err := ReadTaskFlags(taskDir)
	return err == nil && ok && !flags.Acknowledged
}

// TaskFlagsMarker is the board tag beside a task whose commits changed what runs on the host.
const TaskFlagsMarker = "⚠ changes what runs on your machine"

// tasksFolderFlags is `coop tasks flags [<id>] [--ack]`: list every task with unacknowledged
// flags (or one task's), or acknowledge a task's flags after reading them.
func tasksFolderFlags(root string, args []string) (int, error) {
	ack := false
	id := ""
	for _, a := range args {
		switch {
		case a == "--ack":
			ack = true
		case strings.HasPrefix(a, "-"):
			return 2, fmt.Errorf("coop tasks flags: unknown flag %q (only --ack)", a)
		case id != "":
			return 2, errors.New("coop tasks flags: too many arguments (one task id at most)")
		default:
			id = a
		}
	}
	if ack && id == "" {
		return 2, errors.New("usage: coop tasks flags <id> --ack")
	}
	items, err := ReadTaskTree(root)
	if err != nil {
		return -1, err
	}
	shown := 0
	for _, t := range items {
		if id != "" && t.ID != id {
			continue
		}
		flags, ok, err := ReadTaskFlags(t.Dir)
		if err != nil {
			return -1, err
		}
		if !ok || (id == "" && flags.Acknowledged) {
			continue
		}
		shown++
		state := ""
		if flags.Acknowledged {
			state = "  (acknowledged by " + flags.AcknowledgedBy + " " + flags.AcknowledgedAt.Local().Format("2006-01-02 15:04") + ")"
		}
		fmt.Printf("%s  %s%s\n", t.ID, TaskFlagsMarker, state)
		for _, flag := range flags.HostSurfaces {
			fmt.Printf("  %s  (%s)\n    %s\n", flag.Path, flag.Commit, flag.Reason)
		}
		if ack && !flags.Acknowledged {
			flags.Acknowledged = true
			flags.AcknowledgedBy = currentUserName()
			flags.AcknowledgedAt = time.Now().UTC()
			if err := writeTaskFlags(t.Dir, flags); err != nil {
				return -1, err
			}
			fmt.Printf("  acknowledged — the flag is cleared from the board\n")
		}
	}
	switch {
	case id != "" && shown == 0:
		if _, ok, err := CurrentTask(root, id); err != nil {
			return -1, err
		} else if !ok {
			return 1, fmt.Errorf("no task %s", id)
		}
		fmt.Printf("%s carries no flags — none of its commits changed what runs on your machine\n", id)
	case id == "" && shown == 0:
		fmt.Println("no open flags — no completed task changed what runs on your machine, or every flag was acknowledged")
	}
	return 0, nil
}

func currentUserName() string {
	if name := os.Getenv("USER"); name != "" {
		return name
	}
	return "human"
}
