package tasks

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

func TestTaskLogRefusesFIFOReplacedAfterInspection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log.md")
	if err := os.WriteFile(path, []byte("log"), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	before, err := root.Lstat("log.md")
	if err != nil {
		t.Fatal(err)
	}
	if err = root.Remove("log.md"); err != nil {
		t.Fatal(err)
	}
	if err = syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		f, _, err := openTaskLogForAppend(root, before)
		if f != nil {
			_ = f.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("replaced FIFO accepted")
		}
	case <-time.After(wait.Deadline):
		release, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
		if err != nil {
			t.Fatal(err)
		}
		<-done
		_ = release.Close()
		t.Fatal("task log waited for a substituted FIFO reader")
	}
}

func TestBlockedForkOwnerRollbackRestoresExactRecord(t *testing.T) {
	root := t.TempDir()
	item := taskForLease(t, root, StateBlocked, "rollback")
	instance, err := EnsureTaskInstance(root, item)
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Date(2026, 10, 28, 1, 2, 3, 0, time.UTC)
	owner := TaskOwnerRecord{Version: taskOwnershipRecordVersion, TaskID: item.ID, Kind: TaskOwnerFork, Task: &instance, Fork: &ForkTaskOwner{
		Fork: forkspace.Identity{Name: "fork", Generation: forkspace.Generation(strings.Repeat("a", 32))}, AssignmentID: strings.Repeat("b", 32), Phase: ForkAssignmentBlocked,
		Projection: filepath.Join(t.TempDir(), "projection"), BaselineHead: strings.Repeat("c", 40), ProjectionDigest: strings.Repeat("d", 64), CandidateID: strings.Repeat("e", 32), AssignedAt: stamp, UpdatedAt: stamp,
	}}
	if err = writeTaskOwnerRecord(root, owner); err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(root, StateTodo), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, StateTodo, item.ID), []byte("obstruction"), 0600); err != nil {
		t.Fatal(err)
	}
	err = moveBlockedAuditUnblock(root, item, nil)
	var stage *unblockStageError
	if !errors.As(err, &stage) || stage.stage != "task folder move" {
		t.Fatalf("wrong failure: %v", err)
	}
	got, ok, err := ReadTaskOwnerRecord(root, item.ID)
	if err != nil || !ok {
		t.Fatalf("rollback owner: %v %v", ok, err)
	}
	actual, _ := json.Marshal(got)
	expected, _ := json.Marshal(owner)
	if string(actual) != string(expected) {
		t.Fatalf("rollback changed owner:\n%s\nwant:\n%s", actual, expected)
	}
}

func TestProjectionCopyPhaseBoundsEntriesAcrossDirectories(t *testing.T) {
	for _, extra := range []int{0, 1} {
		t.Run(strconv.Itoa(extra), func(t *testing.T) {
			srcPath, dstPath := t.TempDir(), t.TempDir()
			for i, name := range []string{"a", "b"} {
				dir := filepath.Join(srcPath, name)
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				count := (projectionMaxFiles - 2) / 2
				if i == 1 {
					count += extra
				}
				for n := 0; n < count; n++ {
					if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%04d", n)), nil, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			src, err := os.OpenRoot(srcPath)
			if err != nil {
				t.Fatal(err)
			}
			defer src.Close()
			dst, err := os.OpenRoot(dstPath)
			if err != nil {
				t.Fatal(err)
			}
			defer dst.Close()
			err = copyOpenedProjectionEntries(src, dst)
			if extra == 0 && err != nil {
				t.Fatalf("exact limit: %v", err)
			}
			if extra == 1 && (err == nil || !strings.Contains(err.Error(), "entry limit")) {
				t.Fatalf("copy exceeded budget: %v", err)
			}
			copied := 0
			if err := filepath.WalkDir(dstPath, func(path string, _ os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if path != dstPath {
					copied++
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if copied != projectionMaxFiles {
				t.Fatalf("copied %d entries, want %d", copied, projectionMaxFiles)
			}
		})
	}
}
