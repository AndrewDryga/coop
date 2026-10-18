package box

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/AndrewDryga/coop/internal/processidentity"
	"github.com/AndrewDryga/coop/internal/randid"
	"github.com/AndrewDryga/coop/internal/safefile"
)

// hostIDFile holds this host's coop id, in coop's host-owned state folder (serviceStateRoot). It is
// versioned from its first write (.agent/kb/rules/one-upgrade-for-old-state.md). hostIDLimit bounds
// its read: the record is under a hundred bytes, and a FIFO or a huge file put in its place must
// fail at once instead of blocking every launch or filling memory.
const (
	hostIDFile    = "host-id.json"
	hostIDVersion = 1
	hostIDLimit   = 4 << 10
)

type hostIDRecord struct {
	Version int    `json:"version"`
	ID      string `json:"id"` // 32 lowercase hex digits
}

// HostIDError is why this coop cannot name its host; every error thisHost returns is one. Until it
// can, it labels its boxes and services with an empty supervisor and judges no v2 label, so it
// removes nothing its own runs leave behind. Nothing else says so: `coop doctor` shows the error and
// its fix.
type HostIDError struct {
	Err error  // what failed, naming the file or folder involved
	Fix string // what a person can do about it, as one sentence
}

func (e *HostIDError) Error() string { return e.Err.Error() }
func (e *HostIDError) Unwrap() error { return e.Err }

// thisHost is the host field of every supervisor label this process writes, and the one a v2 label
// must carry before this process judges its pid. A pid means something only in the PID namespace it
// was read in: a coop on another host, or in a container, that shares the Docker daemon at the same
// repository path sees a live supervisor's pid as gone or reused, and would remove that run's boxes
// and service volumes. So the field hashes this host's coop id with its PID namespace, which macOS
// does not have. When either cannot be read the field is empty and the error says why; then this
// process writes an empty label and judges no v2 label.
//
// The id is a random one coop keeps, not /etc/machine-id or macOS's IOPlatformUUID: it reads the
// same way on both systems without IOKit or a subprocess, and container images often lack a machine
// id or share one baked into the image. It sits beside the task leases, whose recorded pids already
// mean something only on this host. No box can mount that folder (authorityProtection), and the id
// need not be secret: every label shows its hash, which only has to differ between hosts.
var thisHost = sync.OnceValues(func() (string, error) {
	namespace, ok := processidentity.PIDNamespace()
	if !ok {
		return "", &HostIDError{Err: errors.New("could not read this process's PID namespace from /proc/self/ns/pid"),
			Fix: "Run Coop where /proc is mounted."}
	}
	path, err := serviceStateRoot(hostIDFile)
	if err != nil {
		return "", &HostIDError{Err: fmt.Errorf("could not find Coop's state folder: %w", err),
			Fix: "Set HOME to your home folder, then run Coop again."}
	}
	id, err := readOrCreateHostID(path)
	if err != nil {
		return "", err
	}
	return hostField(id, namespace), nil
})

// hostField is a label's host field: 24 hex digits, like the workspace scope beside it.
func hostField(id, namespace string) string {
	sum := sha256.Sum256([]byte(id + "\n" + namespace))
	return hex.EncodeToString(sum[:12])
}

// isHostField reports whether field has hostField's shape.
func isHostField(field string) bool { return len(field) == 24 && lowerHex(field) }

// readOrCreateHostID returns the id in the file at path, creating the file the first time. A new id
// is written in full under a private name, then linked into place. The link fails when another coop
// got there first, and then both use that coop's id: with two ids on one host, each coop would read
// the other's labels as another host's, and neither could remove what the other left behind.
func readOrCreateHostID(path string) (string, error) {
	dir, name := filepath.Split(path)
	cannotCreate := func(err error) (string, error) {
		return "", &HostIDError{Err: fmt.Errorf("could not create %s: %w", path, err),
			Fix: "Make " + filepath.Clean(dir) + " a folder you can write to, then run Coop again."}
	}
	unusable := func(err error) (string, error) {
		return "", &HostIDError{Err: err,
			Fix: "Once no Coop runs on this computer, delete " + path + ". Coop then makes a new id."}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return cannotCreate(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return cannotCreate(err)
	}
	defer root.Close()
	id, err := readHostID(root, name)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return unusable(err)
	}
	fresh := randid.Hex(16)
	body, err := json.Marshal(hostIDRecord{Version: hostIDVersion, ID: fresh})
	if err != nil {
		return cannotCreate(err)
	}
	staging := "." + name + "-" + fresh
	f, err := root.OpenFile(staging, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return cannotCreate(err)
	}
	defer root.Remove(staging)
	// Synced before it is linked, so a crash cannot leave an empty file under the final name.
	_, err = f.Write(append(body, '\n'))
	if err = errors.Join(err, f.Sync(), f.Close()); err != nil {
		return cannotCreate(err)
	}
	if err := root.Link(staging, name); err != nil && !errors.Is(err, fs.ErrExist) {
		return cannotCreate(err)
	}
	if id, err = readHostID(root, name); err != nil {
		return unusable(err)
	}
	return id, nil
}

// readHostID reads the id in name, reading at most hostIDLimit bytes from a regular file and never
// following a link (os.Root follows one that stays inside the folder). A file without a valid id is
// an error and is never replaced: a running coop may still label boxes with the id it held, or a
// newer coop wrote a format this one cannot read.
func readHostID(root *os.Root, name string) (string, error) {
	path := filepath.Join(root.Name(), name)
	dir, err := root.Open(".")
	if err != nil {
		return "", fmt.Errorf("could not read %s: %w", path, err)
	}
	defer dir.Close()
	data, err := safefile.ReadRegular(dir, name, hostIDLimit)
	if errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if err != nil {
		return "", fmt.Errorf("could not read %s: %w", path, err)
	}
	var record hostIDRecord
	if err := json.Unmarshal(data, &record); err != nil || record.Version != hostIDVersion ||
		len(record.ID) != 32 || !lowerHex(record.ID) {
		return "", fmt.Errorf("%s holds no host id this Coop can read", path)
	}
	return record.ID, nil
}
