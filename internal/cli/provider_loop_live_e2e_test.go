//go:build providerlivee2e

package cli

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/testutil/liveprovider"
	"github.com/AndrewDryga/coop/internal/testutil/procharness"
)

const providerLoopLiveFileLimit = 64 << 10

type providerLoopLiveBaseline struct {
	Repository liveprovider.RepositorySnapshot
	Admin      [32]byte
	Reflogs    map[string]string
}

func TestProviderLoopLiveCompatibility(t *testing.T) {
	testProviderLiveCompatibility(t, liveWorkflowLoop)
}

func providerLoopLiveTaskID(provider string) string { return "provider-live-loop-" + provider }

func providerLoopLiveFile(provider string) string { return "live-loop-" + provider + ".txt" }

func providerLoopLiveTaskBody(provider, marker string) string {
	taskID := providerLoopLiveTaskID(provider)
	file := providerLoopLiveFile(provider)
	return fmt.Sprintf("# Live %s loop task\n\n**Context:** Exercise one real provider against Coop's task-completion contract.\n\n**Acceptance criteria:** Create `%s` containing exactly `%s` followed by one newline. Make no other repository change. Run `git diff --check`, commit the file with subject `test: complete live loop task` and an exact `Coop-Task: %s` trailer, update state.md and log.md, then move this task folder to 99_done/ as the final action.\n\n**Approach:** Perform only the mechanical steps above.\n", provider, file, marker, taskID)
}

func prepareProviderLoopLiveRepository(layout procharness.Layout, target agents.Target, marker string) error {
	provider := target.Provider
	contract := "# Live provider loop fixture\n\nWork only the assigned task. The task's stated gate is the complete gate for this disposable repository. Do not create other tasks or change Git configuration. Do not create task tmp/artifacts or unrelated queue entries; if a tool creates scratch, remove it before the final task move.\n"
	if err := os.WriteFile(filepath.Join(layout.Repo, "AGENTS.md"), []byte(contract), 0o600); err != nil {
		return err
	}
	if _, err := runProviderLoopLiveGit(layout, "add", "--", "AGENTS.md"); err != nil {
		return err
	}
	if _, err := runProviderLoopLiveGit(layout, "commit", "-qm", "test: add live loop contract"); err != nil {
		return err
	}

	root := filepath.Join(layout.Repo, tasksRoot)
	for _, state := range []string{stateTodo, stateInProgress, stateBlocked, stateDone} {
		if err := os.MkdirAll(filepath.Join(root, state), 0o755); err != nil {
			return err
		}
	}
	taskID := providerLoopLiveTaskID(provider)
	task := filepath.Join(root, stateInProgress, taskID)
	if err := os.Mkdir(task, 0o755); err != nil {
		return err
	}
	files := map[string]string{
		"task.md":  providerLoopLiveTaskBody(provider, marker),
		"state.md": "# State - Live provider loop task\n\n**Status:** in progress\n**Done so far:** task claimed by the live harness\n**Next action:** create the exact marker file\n**Traps:** make no other repository changes\n",
		"log.md":   "# Log - Live provider loop task\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(task, name), []byte(body), 0o600); err != nil {
			return err
		}
	}
	return nil
}

func providerLoopLivePrompt(repo, provider string) string {
	return loopWorkPrompt(repo, []string{tasksRoot}, providerLoopLiveTaskID(provider), provider, nil, nil)
}

func snapshotProviderLoopLiveBaseline(layout procharness.Layout, repository liveprovider.RepositorySnapshot) (providerLoopLiveBaseline, error) {
	admin, err := providerLoopLiveAdminDigest(layout)
	if err != nil {
		return providerLoopLiveBaseline{}, err
	}
	reflogs, err := snapshotProviderLoopLiveReflogs(layout)
	if err != nil {
		return providerLoopLiveBaseline{}, err
	}
	return providerLoopLiveBaseline{Repository: repository, Admin: admin, Reflogs: reflogs}, nil
}

func verifyProviderLoopLiveRepository(layout procharness.Layout, before providerLoopLiveBaseline, target agents.Target, marker string) error {
	provider := target.Provider
	taskID := providerLoopLiveTaskID(provider)
	file := providerLoopLiveFile(provider)
	admin, err := providerLoopLiveAdminDigest(layout)
	if err != nil || admin != before.Admin {
		return fmt.Errorf("live loop changed Git administrative state")
	}
	wantEntries := []string{
		".agent/", ".agent/tasks/",
		filepath.Join(tasksRoot, stateTodo) + string(filepath.Separator),
		filepath.Join(tasksRoot, stateInProgress) + string(filepath.Separator),
		filepath.Join(tasksRoot, stateBlocked) + string(filepath.Separator),
		filepath.Join(tasksRoot, stateDone) + string(filepath.Separator),
		filepath.Join(tasksRoot, stateDone, taskID) + string(filepath.Separator),
		filepath.Join(tasksRoot, stateDone, taskID, "log.md"),
		filepath.Join(tasksRoot, stateDone, taskID, "state.md"),
		filepath.Join(tasksRoot, stateDone, taskID, "task.md"),
		".gitignore", "AGENTS.md", "README.md", file,
	}
	slices.Sort(wantEntries)
	gotEntries, err := providerLoopLiveEntries(layout.Repo)
	if err != nil || !slices.Equal(gotEntries, wantEntries) {
		return fmt.Errorf("live loop repository entries mismatch")
	}
	content, err := readProviderLoopLiveFile(layout, filepath.Join(layout.Repo, file))
	if err != nil || string(content) != marker+"\n" {
		return fmt.Errorf("live loop marker file mismatch")
	}
	done := filepath.Join(layout.Repo, tasksRoot, stateDone, taskID)
	state, err := readProviderLoopLiveFile(layout, filepath.Join(done, "state.md"))
	if err != nil || !strings.Contains(string(state), "**Status:** complete") || !strings.Contains(string(state), "**Next action:** none") {
		return fmt.Errorf("live loop task state mismatch")
	}
	log, err := readProviderLoopLiveFile(layout, filepath.Join(done, "log.md"))
	if err != nil || len(strings.TrimSpace(string(log))) <= len("# Log - Live provider loop task") {
		return fmt.Errorf("live loop task log mismatch")
	}
	taskBody, err := readProviderLoopLiveFile(layout, filepath.Join(done, "task.md"))
	if err != nil || string(taskBody) != providerLoopLiveTaskBody(provider, marker) {
		return fmt.Errorf("live loop task contract changed")
	}
	commitEdit, err := readProviderLoopLiveFile(layout, filepath.Join(layout.Repo, ".git", "COMMIT_EDITMSG"))
	wantCommitEdit := "test: complete live loop task\n\nCoop-Task: " + taskID + "\n"
	if err != nil || string(commitEdit) != wantCommitEdit {
		return fmt.Errorf("live loop Git commit message state mismatch")
	}

	headOutput, err := runProviderLoopLiveGit(layout, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	head := strings.TrimSpace(string(headOutput))
	refs, err := runProviderLoopLiveGit(layout, "for-each-ref", "--format=%(refname)%00%(objectname)")
	if err != nil || head == strings.TrimSpace(before.Repository.Head) || string(refs) != strings.Replace(before.Repository.Refs, strings.TrimSpace(before.Repository.Head), head, 1) {
		return fmt.Errorf("live loop changed unexpected refs")
	}
	parent, err := runProviderLoopLiveGit(layout, "rev-parse", "HEAD^")
	if err != nil || strings.TrimSpace(string(parent)) != strings.TrimSpace(before.Repository.Head) {
		return fmt.Errorf("live loop did not create exactly one commit")
	}
	paths, err := runProviderLoopLiveGit(layout, "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD")
	if err != nil || strings.TrimSpace(string(paths)) != file {
		return fmt.Errorf("live loop commit changed unexpected paths")
	}
	subject, err := runProviderLoopLiveGit(layout, "log", "-1", "--format=%s")
	if err != nil || strings.TrimSpace(string(subject)) != "test: complete live loop task" {
		return fmt.Errorf("live loop commit subject mismatch")
	}
	trailer, err := runProviderLoopLiveGit(layout, "log", "-1", "--format=%(trailers:key=Coop-Task,valueonly)")
	if err != nil || strings.TrimSpace(string(trailer)) != taskID {
		return fmt.Errorf("live loop commit trailer mismatch")
	}
	message, err := runProviderLoopLiveGit(layout, "log", "-1", "--format=%B")
	wantMessage := "test: complete live loop task\n\nCoop-Task: " + taskID
	if err != nil || strings.TrimSpace(string(message)) != wantMessage {
		return fmt.Errorf("live loop commit message mismatch")
	}
	if err := verifyProviderLoopLiveReflogs(layout, before.Reflogs, strings.TrimSpace(before.Repository.Head), head); err != nil {
		return err
	}
	if err := verifyProviderLoopLiveIndex(layout); err != nil {
		return err
	}
	status, err := runProviderLoopLiveGit(layout, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil || strings.TrimSpace(string(status)) != "" {
		return fmt.Errorf("live loop repository is dirty")
	}
	unreachable, err := runProviderLoopLiveGit(layout, "fsck", "--no-reflogs", "--unreachable", "--no-progress")
	if err != nil || strings.TrimSpace(string(unreachable)) != "" {
		return fmt.Errorf("live loop retained unreachable Git objects")
	}
	return nil
}

func providerLoopLiveEntries(repo string) ([]string, error) {
	var entries []string
	count := 0
	err := filepath.WalkDir(repo, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(repo, path)
		if err != nil {
			return err
		}
		if rel == ".git" {
			if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("live loop Git directory is not a real directory")
			}
			return filepath.SkipDir
		}
		if rel == "." {
			return nil
		}
		count++
		if count > 64 || entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("live loop repository has unsafe entries")
		}
		if entry.IsDir() {
			entries = append(entries, rel+string(filepath.Separator))
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("live loop repository contains a special file")
		}
		entries = append(entries, rel)
		return nil
	})
	slices.Sort(entries)
	return entries, err
}

func readProviderLoopLiveFile(layout procharness.Layout, path string) ([]byte, error) {
	file, err := procharness.OpenRegularFile(layout.Root, path, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() < 0 || info.Size() > providerLoopLiveFileLimit {
		return nil, fmt.Errorf("live loop file is oversized or unreadable")
	}
	data, err := io.ReadAll(io.LimitReader(file, providerLoopLiveFileLimit+1))
	if err != nil || len(data) > providerLoopLiveFileLimit {
		return nil, fmt.Errorf("read bounded live loop file")
	}
	return data, nil
}

func providerLoopLiveAdminDigest(layout procharness.Layout) ([32]byte, error) {
	gitDir, err := procharness.CanonicalUnderRoot(layout.Root, filepath.Join(layout.Repo, ".git"))
	if err != nil {
		return [32]byte{}, err
	}
	hash := sha256.New()
	entries, bytes := 0, int64(0)
	err = filepath.WalkDir(gitDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		entries++
		if entries > 2048 || entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe live loop Git administrative tree")
		}
		rel, err := filepath.Rel(gitDir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !entry.IsDir() && !entry.Type().IsRegular() {
			return fmt.Errorf("unsafe live loop Git administrative file")
		}
		if !entry.IsDir() {
			if info.Size() < 0 || info.Size() > providerLoopLiveFileLimit {
				return fmt.Errorf("unsafe live loop Git administrative file")
			}
			bytes += info.Size()
			if bytes > 2<<20 {
				return fmt.Errorf("oversized live loop Git administrative tree")
			}
		}

		mutableContent, err := providerLoopLiveMutableAdminPath(rel, entry.IsDir())
		if err != nil {
			return err
		}
		// Names and modes still bind refs, logs, and the index into the digest. Only their
		// contents may change during the one expected commit. Loose object names are derived
		// from content, so fsck and the exact history checks validate them after this no-follow pass.
		if !strings.HasPrefix(rel, "objects/") || !mutableContent {
			fmt.Fprintf(hash, "%s\x00%d\x00", rel, info.Mode())
		}
		if entry.IsDir() || mutableContent {
			return nil
		}
		file, err := procharness.OpenRegularFile(layout.Root, path, os.O_RDONLY)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(hash, io.LimitReader(file, providerLoopLiveFileLimit+1))
		closeErr := file.Close()
		return errors.Join(copyErr, closeErr)
	})
	if err != nil {
		return [32]byte{}, err
	}
	var digest [32]byte
	copy(digest[:], hash.Sum(nil))
	return digest, nil
}

func providerLoopLiveMutableAdminPath(rel string, directory bool) (bool, error) {
	if rel == "index" || rel == "COMMIT_EDITMSG" || rel == "logs" || strings.HasPrefix(rel, "logs/") || rel == "refs" || strings.HasPrefix(rel, "refs/") {
		return true, nil
	}
	parts := strings.Split(rel, "/")
	if len(parts) == 2 && parts[0] == "objects" && directory && providerLoopLiveLowerHex(parts[1], 2) {
		return true, nil
	}
	if len(parts) == 3 && parts[0] == "objects" && !directory && providerLoopLiveLowerHex(parts[1], 2) && providerLoopLiveLowerHex(parts[2], 38) {
		return true, nil
	}
	if strings.HasPrefix(rel, "objects/") && rel != "objects/info" && !strings.HasPrefix(rel, "objects/info/") && rel != "objects/pack" && !strings.HasPrefix(rel, "objects/pack/") {
		return false, fmt.Errorf("unsafe loose Git object path %q", rel)
	}
	return false, nil
}

func providerLoopLiveLowerHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func snapshotProviderLoopLiveReflogs(layout procharness.Layout) (map[string]string, error) {
	gitDir := filepath.Join(layout.Repo, ".git")
	logsDir := filepath.Join(gitDir, "logs")
	result := map[string]string{}
	err := filepath.WalkDir(logsDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := readProviderLoopLiveFile(layout, path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(gitDir, path)
		if err != nil {
			return err
		}
		result[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	return result, err
}

func verifyProviderLoopLiveReflogs(layout procharness.Layout, before map[string]string, oldHead, head string) error {
	after, err := snapshotProviderLoopLiveReflogs(layout)
	if err != nil || len(after) != len(before) {
		return fmt.Errorf("live loop raw reflog inventory changed")
	}
	for path, baseline := range before {
		current, ok := after[path]
		if !ok || !strings.HasPrefix(current, baseline) || !providerLoopLiveReflogAppend(current[len(baseline):], oldHead, head) {
			return fmt.Errorf("live loop raw reflog %q contains unexpected history", path)
		}
	}
	return nil
}

func providerLoopLiveReflogAppend(appendix, oldHead, head string) bool {
	if strings.Count(appendix, "\n") != 1 || !strings.HasSuffix(appendix, "\n") {
		return false
	}
	parts := strings.Split(strings.TrimSuffix(appendix, "\n"), "\t")
	if len(parts) != 2 || parts[1] != "commit: test: complete live loop task" {
		return false
	}
	prefix := oldHead + " " + head + " Coop Live Test <coop-live@example.invalid> "
	if !strings.HasPrefix(parts[0], prefix) {
		return false
	}
	clock := strings.Fields(strings.TrimPrefix(parts[0], prefix))
	if len(clock) != 2 || clock[1] != "+0000" {
		return false
	}
	seconds, err := strconv.ParseInt(clock[0], 10, 64)
	return err == nil && seconds > 0
}

func verifyProviderLoopLiveIndex(layout procharness.Layout) error {
	actual, err := readProviderLoopLiveFile(layout, filepath.Join(layout.Repo, ".git", "index"))
	if err != nil {
		return fmt.Errorf("read live loop Git index: %w", err)
	}
	expected, err := os.CreateTemp(layout.Root, "provider-loop-index-expected-*")
	if err != nil {
		return err
	}
	expectedPath := expected.Name()
	if err := expected.Close(); err != nil {
		return err
	}
	defer os.Remove(expectedPath)
	if err := os.Remove(expectedPath); err != nil {
		return err
	}
	env := []string{"GIT_INDEX_FILE=" + expectedPath}
	if _, err := runProviderLoopLiveGitEnv(layout, env, "read-tree", "HEAD"); err != nil {
		return err
	}
	if _, err := runProviderLoopLiveGitEnv(layout, env, "update-index", "--really-refresh"); err != nil {
		return err
	}
	expectedData, expectedErr := readProviderLoopLiveFile(layout, expectedPath)
	normalizedActual, actualErr := normalizeProviderLoopLiveIndex(actual)
	normalizedExpected, normalizeExpectedErr := normalizeProviderLoopLiveIndex(expectedData)
	if expectedErr != nil || actualErr != nil || normalizeExpectedErr != nil || !slices.Equal(normalizedActual, normalizedExpected) {
		return fmt.Errorf("live loop Git index is not canonical")
	}
	return nil
}

func normalizeProviderLoopLiveIndex(data []byte) ([]byte, error) {
	const headerSize, entryFixedSize, checksumSize = 12, 62, sha1.Size
	if len(data) < headerSize+checksumSize || string(data[:4]) != "DIRC" || binary.BigEndian.Uint32(data[4:8]) != 2 {
		return nil, errors.New("unsupported live loop Git index")
	}
	wantChecksum := sha1.Sum(data[:len(data)-checksumSize])
	if !slices.Equal(wantChecksum[:], data[len(data)-checksumSize:]) {
		return nil, errors.New("invalid live loop Git index checksum")
	}
	normalized := slices.Clone(data)
	offset := headerSize
	entries := int(binary.BigEndian.Uint32(data[8:12]))
	for range entries {
		if offset+entryFixedSize >= len(data)-checksumSize {
			return nil, errors.New("truncated live loop Git index entry")
		}
		entryStart := offset
		flags := binary.BigEndian.Uint16(data[offset+60 : offset+62])
		nameLength := int(flags & 0x0fff)
		nameStart := offset + entryFixedSize
		if nameLength == 0x0fff {
			end := slices.Index(data[nameStart:len(data)-checksumSize], byte(0))
			if end < 0 {
				return nil, errors.New("unterminated live loop Git index path")
			}
			nameLength = end
		}
		nameEnd := nameStart + nameLength
		if nameEnd >= len(data)-checksumSize || data[nameEnd] != 0 {
			return nil, errors.New("invalid live loop Git index path")
		}
		offset = entryStart + ((entryFixedSize + nameLength + 1 + 7) &^ 7)
		if offset > len(data)-checksumSize {
			return nil, errors.New("truncated live loop Git index padding")
		}
		for _, value := range data[nameEnd:offset] {
			if value != 0 {
				return nil, errors.New("nonzero live loop Git index padding")
			}
		}
		clear(normalized[entryStart : entryStart+24])
		clear(normalized[entryStart+28 : entryStart+40])
	}
	clear(normalized[len(normalized)-checksumSize:])
	return normalized, nil
}

func runProviderLoopLiveGit(layout procharness.Layout, args ...string) ([]byte, error) {
	return runProviderLoopLiveGitEnv(layout, nil, args...)
}

func runProviderLoopLiveGitEnv(layout procharness.Layout, extraEnv []string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	env := []string{
		"HOME=" + layout.Home, "PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_GLOBAL=" + layout.GitConfig, "GIT_CONFIG_NOSYSTEM=1",
		"LANG=C", "LC_ALL=C", "TZ=UTC",
	}
	env = append(env, extraEnv...)
	result := procharness.Run(ctx, procharness.Command{
		Path: "git", Args: append([]string{"-C", layout.Repo}, args...), Dir: layout.Repo,
		MaxOutput: providerLoopLiveFileLimit, KillGrace: 500 * time.Millisecond,
		Env: env,
	})
	if result.Err != nil || result.ExitCode != 0 || result.StdoutTruncated || result.StderrTruncated || strings.TrimSpace(result.Stderr) != "" {
		return nil, fmt.Errorf("git live loop command failed: exit %d: %v (%s)", result.ExitCode, result.Err, strings.TrimSpace(result.Stderr))
	}
	return []byte(result.Stdout), nil
}

func TestProviderLoopLiveContract(t *testing.T) {
	newCompleted := func(t *testing.T) (procharness.Layout, providerLoopLiveBaseline, agents.Target, string) {
		t.Helper()
		layout, err := procharness.NewLayout(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if err := liveprovider.InitRepository(layout); err != nil {
			t.Fatal(err)
		}
		target := agents.Target{Provider: "codex", Model: "model", Accounts: []string{"work"}}
		marker := "COOP_LIVE_MARKER_contract"
		if err := prepareProviderLoopLiveRepository(layout, target, marker); err != nil {
			t.Fatal(err)
		}
		repository, err := liveprovider.SnapshotRepository(layout)
		if err != nil {
			t.Fatal(err)
		}
		before, err := snapshotProviderLoopLiveBaseline(layout, repository)
		if err != nil {
			t.Fatal(err)
		}
		file := providerLoopLiveFile(target.Provider)
		if err := os.WriteFile(filepath.Join(layout.Repo, file), []byte(marker+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := runProviderLoopLiveGit(layout, "add", "--", file); err != nil {
			t.Fatal(err)
		}
		if _, err := runProviderLoopLiveGit(layout, "commit", "-qm", "test: complete live loop task", "-m", "Coop-Task: "+providerLoopLiveTaskID(target.Provider)); err != nil {
			t.Fatal(err)
		}
		task := filepath.Join(layout.Repo, tasksRoot, stateInProgress, providerLoopLiveTaskID(target.Provider))
		if err := os.WriteFile(filepath.Join(task, "state.md"), []byte("# State\n\n**Status:** complete\n**Done so far:** contract completed\n**Next action:** none\n**Traps:** none\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(task, "log.md"), []byte("# Log - Live provider loop task\n\n- contract completed\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(task, filepath.Join(layout.Repo, tasksRoot, stateDone, providerLoopLiveTaskID(target.Provider))); err != nil {
			t.Fatal(err)
		}
		return layout, before, target, marker
	}

	t.Run("accepts exact completion", func(t *testing.T) {
		layout, before, target, marker := newCompleted(t)
		if err := verifyProviderLoopLiveRepository(layout, before, target, marker); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("index comparison ignores only filesystem stat fields", func(t *testing.T) {
		layout, _, _, _ := newCompleted(t)
		original, err := readProviderLoopLiveFile(layout, filepath.Join(layout.Repo, ".git", "index"))
		if err != nil {
			t.Fatal(err)
		}
		rechecksum := func(data []byte) {
			digest := sha1.Sum(data[:len(data)-sha1.Size])
			copy(data[len(data)-sha1.Size:], digest[:])
		}
		statChanged := slices.Clone(original)
		statChanged[12] ^= 1
		rechecksum(statChanged)
		want, err := normalizeProviderLoopLiveIndex(original)
		if err != nil {
			t.Fatal(err)
		}
		got, err := normalizeProviderLoopLiveIndex(statChanged)
		if err != nil || !slices.Equal(got, want) {
			t.Fatalf("filesystem-only index metadata was not normalized: %v", err)
		}
		objectChanged := slices.Clone(original)
		objectChanged[12+40] ^= 1
		rechecksum(objectChanged)
		got, err = normalizeProviderLoopLiveIndex(objectChanged)
		if err != nil || slices.Equal(got, want) {
			t.Fatalf("semantic index mutation was normalized away: %v", err)
		}
		modeChanged := slices.Clone(original)
		modeChanged[12+24] ^= 1
		rechecksum(modeChanged)
		got, err = normalizeProviderLoopLiveIndex(modeChanged)
		if err != nil || slices.Equal(got, want) {
			t.Fatalf("index mode mutation was normalized away: %v", err)
		}
		hidden := append(slices.Clone(original[:len(original)-sha1.Size]), []byte("SECRET")...)
		digest := sha1.Sum(hidden)
		hidden = append(hidden, digest[:]...)
		got, err = normalizeProviderLoopLiveIndex(hidden)
		if err != nil || slices.Equal(got, want) {
			t.Fatalf("valid-checksummed hidden index bytes were normalized away: %v", err)
		}
	})
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, procharness.Layout, agents.Target)
	}{
		{"missing trailer", func(t *testing.T, layout procharness.Layout, _ agents.Target) {
			if _, err := runProviderLoopLiveGit(layout, "commit", "--amend", "-qm", "test: complete live loop task"); err != nil {
				t.Fatal(err)
			}
		}},
		{"extra ignored file", func(t *testing.T, layout procharness.Layout, _ agents.Target) {
			if err := os.WriteFile(filepath.Join(layout.Repo, tasksRoot, "unexpected"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"task left in progress", func(t *testing.T, layout procharness.Layout, target agents.Target) {
			id := providerLoopLiveTaskID(target.Provider)
			if err := os.Rename(filepath.Join(layout.Repo, tasksRoot, stateDone, id), filepath.Join(layout.Repo, tasksRoot, stateInProgress, id)); err != nil {
				t.Fatal(err)
			}
		}},
		{"retained tmp", func(t *testing.T, layout procharness.Layout, target agents.Target) {
			path := filepath.Join(layout.Repo, tasksRoot, stateDone, providerLoopLiveTaskID(target.Provider), "tmp")
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{"extra empty directory", func(t *testing.T, layout procharness.Layout, _ agents.Target) {
			if err := os.Mkdir(filepath.Join(layout.Repo, tasksRoot, "unexpected-dir"), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlinked marker", func(t *testing.T, layout procharness.Layout, target agents.Target) {
			path := filepath.Join(layout.Repo, providerLoopLiveFile(target.Provider))
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("/dev/zero", path); err != nil {
				t.Fatal(err)
			}
		}},
		{"oversized log", func(t *testing.T, layout procharness.Layout, target agents.Target) {
			path := filepath.Join(layout.Repo, tasksRoot, stateDone, providerLoopLiveTaskID(target.Provider), "log.md")
			if err := os.WriteFile(path, []byte(strings.Repeat("x", providerLoopLiveFileLimit+1)), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"special file", func(t *testing.T, layout procharness.Layout, _ agents.Target) {
			if err := syscall.Mkfifo(filepath.Join(layout.Repo, tasksRoot, "unexpected-fifo"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"hidden commit", func(t *testing.T, layout procharness.Layout, _ agents.Target) {
			head, err := runProviderLoopLiveGit(layout, "rev-parse", "HEAD")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(layout.Repo, "hidden-secret"), []byte("secret\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := runProviderLoopLiveGit(layout, "add", "--", "hidden-secret"); err != nil {
				t.Fatal(err)
			}
			if _, err := runProviderLoopLiveGit(layout, "commit", "-qm", "hidden"); err != nil {
				t.Fatal(err)
			}
			if _, err := runProviderLoopLiveGit(layout, "reset", "--hard", strings.TrimSpace(string(head))); err != nil {
				t.Fatal(err)
			}
		}},
		{"changed task contract", func(t *testing.T, layout procharness.Layout, target agents.Target) {
			path := filepath.Join(layout.Repo, tasksRoot, stateDone, providerLoopLiveTaskID(target.Provider), "task.md")
			if err := os.WriteFile(path, []byte("changed\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"unknown Git admin file", func(t *testing.T, layout procharness.Layout, _ agents.Target) {
			if err := os.WriteFile(filepath.Join(layout.Repo, ".git", "provider-secret"), []byte("secret\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlinked Git ref", func(t *testing.T, layout procharness.Layout, _ agents.Target) {
			if err := os.Symlink("../../config", filepath.Join(layout.Repo, ".git", "refs", "provider-ref")); err != nil {
				t.Fatal(err)
			}
		}},
		{"special Git ref", func(t *testing.T, layout procharness.Layout, _ agents.Target) {
			if err := syscall.Mkfifo(filepath.Join(layout.Repo, ".git", "refs", "provider-fifo"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"changed Git commit message state", func(t *testing.T, layout procharness.Layout, _ agents.Target) {
			if err := os.WriteFile(filepath.Join(layout.Repo, ".git", "COMMIT_EDITMSG"), []byte("secret\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"malformed raw Git reflog", func(t *testing.T, layout procharness.Layout, _ agents.Target) {
			f, err := os.OpenFile(filepath.Join(layout.Repo, ".git", "logs", "HEAD"), os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteString("SECRET\n"); err != nil {
				f.Close()
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
		}},
		{"noncanonical Git index", func(t *testing.T, layout procharness.Layout, _ agents.Target) {
			path := filepath.Join(layout.Repo, ".git", "index")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			body := append(slices.Clone(data[:len(data)-sha1.Size]), []byte("SECRET")...)
			digest := sha1.Sum(body)
			body = append(body, digest[:]...)
			if err := os.WriteFile(path, body, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			layout, before, target, marker := newCompleted(t)
			test.mutate(t, layout, target)
			if err := verifyProviderLoopLiveRepository(layout, before, target, marker); err == nil {
				t.Fatal("unsafe live loop result accepted")
			}
		})
	}

	t.Run("rejects poisoned Git config without execution", func(t *testing.T) {
		layout, before, target, marker := newCompleted(t)
		gitDir := filepath.Join(layout.Repo, ".git")
		configPath := filepath.Join(gitDir, "config")
		config, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatal(err)
		}
		sentinel := filepath.Join(gitDir, "fsmonitor-ran")
		hook := filepath.Join(gitDir, "poisoned-fsmonitor")
		if err := os.WriteFile(hook, []byte("#!/bin/sh\n: >"+sentinel+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		poisoned := append(config, []byte("\n[core]\n\tfsmonitor = "+hook+"\n")...)
		if err := os.WriteFile(configPath, poisoned, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := verifyProviderLoopLiveRepository(layout, before, target, marker); err == nil {
			t.Fatal("poisoned Git config accepted")
		}
		if _, err := os.Lstat(sentinel); !os.IsNotExist(err) {
			t.Fatalf("live verifier executed poisoned fsmonitor: %v", err)
		}
	})

	t.Run("rejects redirected common Git directory without execution", func(t *testing.T) {
		layout, before, target, marker := newCompleted(t)
		common := filepath.Join(layout.Root, "provider-common-git")
		if err := os.MkdirAll(common, 0o700); err != nil {
			t.Fatal(err)
		}
		sentinel := filepath.Join(common, "fsmonitor-ran")
		hook := filepath.Join(common, "poisoned-fsmonitor")
		if err := os.WriteFile(hook, []byte("#!/bin/sh\n: >"+sentinel+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(common, "config"), []byte("[core]\n\tfsmonitor = "+hook+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(layout.Repo, ".git", "commondir"), []byte("../../provider-common-git\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := verifyProviderLoopLiveRepository(layout, before, target, marker); err == nil {
			t.Fatal("redirected common Git directory accepted")
		}
		if _, err := os.Lstat(sentinel); !os.IsNotExist(err) {
			t.Fatalf("live verifier executed redirected fsmonitor: %v", err)
		}
	})
}
