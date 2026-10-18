package forkspace

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Every box-writable record goes through one reader. Whatever a box puts at a record's name is
// refused at once, named for what it is, by path and inside a root alike; only a regular file
// with one name, within its limit, is read.
func TestRecordReaderRefusesWhatABoxCanPlant(t *testing.T) {
	// A short folder: a socket's path must fit in 104 bytes on macOS, which the test's own temporary
	// folder there does not leave room for.
	dir, err := os.MkdirTemp("/tmp", "coop-record-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"record": "{}", "large": strings.Repeat("x", 65), "linked": "{}"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Link(filepath.Join(dir, "linked"), filepath.Join(dir, "linked-twin")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "folder"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	reads := map[string]func(string) ([]byte, error){
		"by path":     func(name string) ([]byte, error) { return ReadRecord(filepath.Join(dir, name), "the record", 64) },
		"inside root": func(name string) ([]byte, error) { return ReadRecordIn(root, name, "the record", 64) },
	}
	for how, read := range reads {
		t.Run(how, func(t *testing.T) {
			if got, err := read("record"); err != nil || string(got) != "{}" {
				t.Fatalf("a record = %q, %v", got, err)
			}
			if _, err := read("missing"); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a missing record = %v, want absent", err)
			}
			for name, want := range map[string]string{
				"link": "the record is a link", "folder": "the record is a folder", "fifo": "the record is a named pipe",
				"s": "the record is a socket", "large": "the record is larger than 64 bytes", "linked": "the record has another name",
			} {
				done := make(chan error, 1)
				go func() { _, err := read(name); done <- err }()
				select {
				case err := <-done:
					if err == nil || !strings.Contains(err.Error(), want) {
						t.Errorf("%s = %v, want %q", name, err, want)
					}
				case <-time.After(10 * time.Second):
					t.Fatalf("reading %s waited", name)
				}
			}
		})
	}
	// A git pointer may have other names; everything else is refused as for a record.
	if data, err := readRecord(byPath{}, filepath.Join(dir, "linked"), "git pointer", 64, false); err != nil || string(data) != "{}" {
		t.Fatalf("a git pointer with another name = %q, %v", data, err)
	}
}
