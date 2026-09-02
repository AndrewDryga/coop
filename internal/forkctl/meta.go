package forkctl

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
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
	meta := filepath.Join(ws, ".coop")
	root, err := tasks.OpenTaskMetadataRoot(meta)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open fork metadata directory %s: %w", meta, err)
	}
	defer root.Close()
	path := ForkSessionFile(ws, agent, account)
	if err := root.Remove(filepath.Base(path)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear fork session metadata %s: %w", path, err)
	}
	return nil
}
