package sessionsvc

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha1"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

type checkpointObjectMember struct{ kind, oid, content string }

func checkpointObjectArchive(t *testing.T, members []checkpointObjectMember) *bytes.Buffer {
	t.Helper()
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	for _, member := range members {
		if err := checkpointTarHeader(writer, "objects/"+member.kind+"/"+member.oid, 0644, int64(len(member.content))); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(member.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return &archive
}

func checkpointBlobMember(content string) checkpointObjectMember {
	return checkpointObjectMember{"blob", fmt.Sprintf("%x", sha1.Sum([]byte(fmt.Sprintf("blob %d\x00%s", len(content), content)))), content}
}

// Restoring a checkpoint used to start one git process per object while its session was fenced.
// The objects of one type now go through one hash-object, with every id still checked.
func TestCheckpointObjectsRestoreThroughOneGitProcessPerType(t *testing.T) {
	ctx := context.Background()
	var members []checkpointObjectMember
	for i := range 300 {
		members = append(members, checkpointBlobMember(fmt.Sprintf("file %d\n", i)))
	}
	repository, git := gitrepo.New(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	gitProcesses := countGitProcesses(t)
	if err := readCheckpointRepository(ctx, repository, checkpointObjectArchive(t, members)); err != nil {
		t.Fatal(err)
	}
	// The shared helpers read a new view's config once; that git is not per object.
	if processes, writers := gitProcesses(""), gitProcesses("hash-object"); writers != 1 || processes > 2 {
		t.Fatalf("300 blobs started %d git processes, %d of them hash-object; want one hash-object", processes, writers)
	}
	for _, member := range []checkpointObjectMember{members[0], members[299]} {
		if got := gitOut(repository, "cat-file", "blob", member.oid); got != member.content[:len(member.content)-1] {
			t.Fatalf("object %s = %q", member.oid, got)
		}
		if info, err := os.Stat(filepath.Join(repository, ".git", "checkpoint-seen", member.oid)); err != nil || info.Size() != 0 {
			t.Fatalf("staged copy kept: %v", err)
		}
	}

	refused := map[string][]checkpointObjectMember{
		"duplicate checkpoint Git object":             {members[0], members[0]},
		"checkpoint Git object digest does not match": {members[0], {"blob", members[1].oid, "not the named content\n"}},
	}
	for want, archive := range refused {
		repository, git := gitrepo.New(t)
		git("commit", "-q", "--allow-empty", "-m", "base")
		if err := readCheckpointRepository(ctx, repository, checkpointObjectArchive(t, archive)); err == nil || err.Error() != want {
			t.Fatalf("refusal = %v, want %q", err, want)
		}
	}
}

// countGitProcesses puts a git on PATH that records each start before it runs the real one, and
// returns a func reporting how many started since this call whose arguments contain the given
// text ("" counts every one, the shared helpers' own included).
func countGitProcesses(t *testing.T) func(string) int {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	started := filepath.Join(bin, "started")
	script := fmt.Sprintf("#!/bin/sh\necho \"$*\" >> %q\nexec %q \"$@\"\n", started, realGit)
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func(text string) int {
		data, err := os.ReadFile(started)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		count := 0
		for line := range strings.Lines(string(data)) {
			if strings.Contains(line, text) {
				count++
			}
		}
		return count
	}
}
