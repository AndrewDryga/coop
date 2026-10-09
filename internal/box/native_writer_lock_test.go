package box

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/AndrewDryga/coop/internal/safefile"
	"golang.org/x/sys/unix"
)

func writerFenceFixture(t *testing.T) *legacyAccountFence {
	t.Helper()
	home := t.TempDir()
	root, err := safefile.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	fence := &legacyAccountFence{home: home, root: root}
	t.Cleanup(func() {
		for _, lock := range fence.locks {
			_ = lock.Close()
		}
		_ = root.Close()
	})
	return fence
}

func TestLegacyWriterLockTightensSameInode(t *testing.T) {
	for _, mode := range []os.FileMode{0o644, 0o600, 0} {
		t.Run(mode.String(), func(t *testing.T) {
			fence := writerFenceFixture(t)
			path := filepath.Join(fence.home, "auth.json.lock")
			var before os.FileInfo
			original := []byte("inert writer PID")
			if mode != 0 {
				if err := os.WriteFile(path, original, mode); err != nil {
					t.Fatal(err)
				}
				var err error
				before, err = os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := fence.addWriter("auth.json.lock"); err != nil {
				t.Fatal(err)
			}
			after, err := os.Stat(path)
			if err != nil || after.Mode().Perm() != 0o600 || before != nil && !os.SameFile(before, after) {
				t.Fatal("lock identity or permissions changed incorrectly", err)
			}
			data, err := os.ReadFile(path)
			if err != nil || mode != 0 && !bytes.Equal(data, original) || mode == 0 && len(data) != 0 {
				t.Fatal("writer lock bytes changed", err)
			}
		})
	}
}

func TestLegacyWriterLockRefusesUnsafeAndBusyWithoutChmod(t *testing.T) {
	for _, kind := range []string{"busy", "hardlink", "symlink", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			fence := writerFenceFixture(t)
			path := filepath.Join(fence.home, "auth.json.lock")
			original := filepath.Join(fence.home, "original")
			if err := os.WriteFile(original, []byte("inert writer PID"), 0o644); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "hardlink":
				if err := os.Link(original, path); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(original, path); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := unix.Mkfifo(path, 0o644); err != nil {
					t.Fatal(err)
				}
			case "busy":
				if err := os.Rename(original, path); err != nil {
					t.Fatal(err)
				}
				writer, err := os.OpenFile(path, os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer writer.Close()
				if err := unix.Flock(int(writer.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
					t.Fatal(err)
				}
			}
			if err := fence.addWriter("auth.json.lock"); err == nil {
				t.Fatal("unsafe or busy writer lock accepted")
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0o644 {
				t.Fatal("refused writer lock was modified", err)
			}
		})
	}
}

type writerOwnershipInfo struct {
	os.FileInfo
	stat syscall.Stat_t
}

func (i writerOwnershipInfo) Sys() any { return &i.stat }

func TestLegacyWriterLockRefusesForeignOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat := *info.Sys().(*syscall.Stat_t)
	stat.Uid++
	if legacyWriterLockOwned(writerOwnershipInfo{FileInfo: info, stat: stat}) {
		t.Fatal("foreign writer lock eligible for chmod")
	}
}

func TestLegacyWriterLockReplacementRefusesBeforeChmod(t *testing.T) {
	fence := writerFenceFixture(t)
	path := filepath.Join(fence.home, "auth.json.lock")
	if err := os.WriteFile(path, []byte("old PID"), 0o644); err != nil {
		t.Fatal(err)
	}
	lock, err := safefile.OpenRegular(fence.root, "auth.json.lock")
	if err != nil {
		t.Fatal(err)
	}
	fence.locks = append(fence.locks, lock)
	if err := os.Rename(path, path+".retained"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("new PID"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fence.lockWriter(lock); err == nil {
		t.Fatal("replaced writer lock accepted")
	}
	for _, name := range []string{path, path + ".retained"} {
		info, err := os.Stat(name)
		if err != nil || info.Mode().Perm() != 0o644 {
			t.Fatal("replaced lock was modified", err)
		}
	}
}
