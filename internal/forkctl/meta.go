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

func SaveForkAgent(ws, agent string) { SaveForkMeta(ws, forkAgentFile(ws), agent) }

const forkMetadataFileLimit = 4 << 10

// Fork metadata is provider-writable between launches. Reads and writes reuse the task metadata
// no-follow root/file primitives so a planted .coop or file symlink cannot reach a host path.
// Errors remain best-effort because these hints can be re-derived or re-prompted next run.
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

func SaveForkMeta(ws, path, value string) {
	meta := filepath.Join(ws, ".coop")
	if value == "" || len(value) > forkMetadataFileLimit || filepath.Dir(path) != meta {
		return
	}
	if err := os.Mkdir(meta, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return
	}
	root, err := tasks.OpenTaskMetadataRoot(meta)
	if err != nil {
		return
	}
	defer root.Close()
	_ = tasks.AtomicWriteTaskFile(root, filepath.Base(path), []byte(value+"\n"))
}

// ForkSessionFile records the coop-owned session id for a fork+agent+account,
// inside the fork but git-excluded, so re-entry resumes exactly that session.
func ForkSessionFile(ws, agent, account string) string {
	return filepath.Join(ws, ".coop", "session."+agent+"."+account)
}

func LegacyForkSessionFile(ws, agent string) string {
	return filepath.Join(ws, ".coop", "session."+agent)
}

func ReadForkSession(ws, agent, account string) string {
	id := readForkMeta(ws, ForkSessionFile(ws, agent, account))
	if !agents.ValidSessionID(id) {
		return ""
	}
	return id
}

func ReadLegacyForkSession(ws, agent string) string {
	id := readForkMeta(ws, LegacyForkSessionFile(ws, agent))
	if !agents.ValidSessionID(id) {
		return ""
	}
	return id
}

func SaveForkSession(ws, agent, account, id string) {
	SaveForkMeta(ws, ForkSessionFile(ws, agent, account), id)
}

func ClearForkSession(ws, agent, account string) {
	meta := filepath.Join(ws, ".coop")
	root, err := tasks.OpenTaskMetadataRoot(meta)
	if err != nil {
		return
	}
	defer root.Close()
	_ = root.Remove(filepath.Base(ForkSessionFile(ws, agent, account)))
}
