package box

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The first coop on a host makes its id, owner-only, and every later one reads the same id back.
func TestHostIDIsMadeOnceAndKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "coop", hostIDFile)
	id, err := readOrCreateHostID(path)
	if err != nil || len(id) != 32 || !lowerHex(id) {
		t.Fatalf("readOrCreateHostID = %q, %v; want a new 32-digit hex id", id, err)
	}
	again, err := readOrCreateHostID(path)
	if err != nil || again != id {
		t.Fatalf("second read = %q, %v; want %q", again, err, id)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("host id file = %v, %v; want an owner-only file", info, err)
	}
	if entries, err := os.ReadDir(filepath.Dir(path)); err != nil || len(entries) != 1 {
		t.Fatalf("state folder holds %v, %v; want only the host id, no staging file", entries, err)
	}
}

// Coops that start together on a fresh host agree on one id: two would make each read the other's
// labels as another host's.
func TestHostIDRaceSettlesOnOneID(t *testing.T) {
	path := filepath.Join(t.TempDir(), hostIDFile)
	ids := make([]string, 8)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Go(func() {
			id, err := readOrCreateHostID(path)
			if err != nil {
				t.Error(err)
			}
			ids[i] = id
		})
	}
	wg.Wait()
	if ids[0] == "" || slices.ContainsFunc(ids, func(id string) bool { return id != ids[0] }) {
		t.Fatalf("racing coops read ids %v, want one", ids)
	}
	if entries, err := os.ReadDir(filepath.Dir(path)); err != nil || len(entries) != 1 {
		t.Fatalf("state folder holds %v, %v; want only the host id", entries, err)
	}
}

// A file that holds no id fails closed, and is left as it is: a running coop may still label boxes
// with what it once held. The error names the file and tells a person to delete it.
func TestHostIDRefusesAFileWithoutAnID(t *testing.T) {
	for _, body := range []string{"", "not json\n", `{"version":2,"id":"0123456789abcdef0123456789abcdef"}`, `{"version":1,"id":"short"}`} {
		path := filepath.Join(t.TempDir(), hostIDFile)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		id, err := readOrCreateHostID(path)
		if id != "" || !deleteTheFile(err, path) {
			t.Errorf("host id file %q read as %q, %v; want an error that says to delete it", body, id, err)
		}
		if data, err := os.ReadFile(path); err != nil || string(data) != body {
			t.Errorf("host id file %q became %q, %v; want it untouched", body, data, err)
		}
	}
}

// Anything but a small regular file where the id belongs fails at once and is left as it is: a
// link, even to a valid id beside it, which os.Root alone would follow; a FIFO, whose open would
// block every launch; a folder; and a file too large to be an id.
func TestHostIDRefusesWhatIsNotAPlainFile(t *testing.T) {
	valid := `{"version":1,"id":"0123456789abcdef0123456789abcdef"}` + "\n"
	for _, tc := range []struct {
		name  string
		plant func(path string) error
	}{
		{"link", func(path string) error {
			if err := os.WriteFile(filepath.Join(filepath.Dir(path), "real.json"), []byte(valid), 0o600); err != nil {
				return err
			}
			return os.Symlink("real.json", path)
		}},
		{"fifo", func(path string) error { return syscall.Mkfifo(path, 0o600) }},
		{"folder", func(path string) error { return os.Mkdir(path, 0o700) }},
		{"oversized", func(path string) error {
			return os.WriteFile(path, []byte(valid+strings.Repeat(" ", hostIDLimit)), 0o600)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), hostIDFile)
			if err := tc.plant(path); err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			type result struct {
				id  string
				err error
			}
			done := make(chan result, 1)
			go func() {
				id, err := readOrCreateHostID(path)
				done <- result{id, err}
			}()
			select {
			case got := <-done:
				if got.id != "" || !deleteTheFile(got.err, path) {
					t.Fatalf("readOrCreateHostID = %q, %v; want an error that says to delete the file", got.id, got.err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("readOrCreateHostID blocked on what sits in place of the host id")
			}
			after, err := os.Lstat(path)
			if err != nil || after.Mode() != before.Mode() || after.Size() != before.Size() {
				t.Fatalf("%s became %v, %v; want it untouched", tc.name, after, err)
			}
		})
	}
}

// A host id coop cannot create names the folder to fix, not a file to delete.
func TestHostIDNamesTheFolderItCannotCreate(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(blocked, "coop", hostIDFile)
	id, err := readOrCreateHostID(path)
	var problem *HostIDError
	if id != "" || !errors.As(err, &problem) || !strings.Contains(problem.Error(), path) ||
		!strings.Contains(problem.Fix, "Make "+filepath.Dir(path)+" a folder you can write to") {
		t.Fatalf("readOrCreateHostID under a file = %q, %v; want an error naming the folder to fix", id, err)
	}
}

// deleteTheFile reports whether err is a HostIDError that names the file at path and tells a person
// to delete it, which is all doctor can offer for a file coop never replaces.
func deleteTheFile(err error, path string) bool {
	var problem *HostIDError
	return errors.As(err, &problem) && strings.Contains(problem.Error(), path) && strings.Contains(problem.Fix, "delete "+path+".")
}

// The label's host field changes with the host id and with the PID namespace, so containers sharing
// one coop state folder still tell their pids apart.
func TestHostFieldNamesTheHostAndTheNamespace(t *testing.T) {
	id, other := "0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210"
	field := hostField(id, "pid:[4026531836]")
	if !isHostField(field) || isHostField(field[1:]) || isHostField("not-a-host-field-at-all!") {
		t.Fatalf("hostField = %q, want 24 hex digits", field)
	}
	for _, differs := range []string{hostField(other, "pid:[4026531836]"), hostField(id, "pid:[4026532201]"), hostField(id, "")} {
		if differs == field {
			t.Fatalf("hostField %q did not change with its host or namespace", field)
		}
	}
	host, err := thisHost()
	if again, _ := thisHost(); err != nil || !isHostField(host) || again != host {
		t.Fatalf("thisHost = %q, %v; want one stable 24-digit host field", host, err)
	}
}
