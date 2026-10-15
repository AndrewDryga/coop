package box

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/safefile"
	"golang.org/x/sys/unix"
)

// A stamp records that one retained source, in the state its fingerprint describes, was fully
// imported under one ownership scope. Planning reads and hashes every retained transcript, so
// without it every launch repeats an import that finished long ago. Stamps sit with the receipts,
// outside the mounted home, so a box can neither forge a skip nor erase one.
type nativeHistoryStamp struct {
	Version     int    `json:"version"`
	Source      string `json:"source"`
	Scope       string `json:"scope"`
	Fingerprint string `json:"fingerprint"`
}

// historyStore is one retained store a fingerprint covers. A flat store contributes only its own
// files: the retained binding store shares its folder with the current per-repository stores. keep,
// when set, picks which of a flat store's entries count.
type historyStore struct {
	path string
	flat bool
	keep func(name string) bool
}

// forkRecordsStore covers the project's fork generation records and their private anchors. Which
// retained transcripts belong to a project depends on them (HistoryKey resolves a fork's folder
// through its record, and the workspace marker is a hard link to the anchor, so the anchor's ctime
// moves with it), so a fork recorded after an import must reopen it. Logs, pids and locks there
// change constantly and decide nothing.
func forkRecordsStore(project string) historyStore {
	return historyStore{path: forkspace.StateDir(project), flat: true, keep: func(name string) bool {
		return strings.HasSuffix(name, ".generation.json") || strings.HasSuffix(name, ".anchor")
	}}
}

func nativeHistoryStampName(source, scope string) string {
	key := sha256.Sum256([]byte("complete\x00" + source + "\x00" + scope))
	return "complete-" + hex.EncodeToString(key[:])
}

func nativeHistoryStamped(home, source, scope, fingerprint string) bool {
	root, err := safefile.OpenRoot(filepath.Join(filepath.Dir(home), "imports"))
	if err != nil {
		return false
	}
	defer root.Close()
	data, err := safefile.ReadRegular(root, nativeHistoryStampName(source, scope), 16<<10)
	var stamp nativeHistoryStamp
	return err == nil && json.Unmarshal(data, &stamp) == nil &&
		stamp == nativeHistoryStamp{Version: 1, Source: source, Scope: scope, Fingerprint: fingerprint}
}

// Call only after the import completed. The stamp just spares later launches the planning, so
// failing to record it leaves the next launch to plan again instead of failing this one.
func stampNativeHistory(ctx context.Context, home, source, scope, fingerprint string) {
	root, err := openPrivateAccountTree(filepath.Dir(home), "imports")
	if err != nil {
		return
	}
	defer root.close()
	lock, err := root.lock(ctx)
	if err != nil {
		return
	}
	defer lock.Close()
	_ = writeNativeHistoryReceipt(root, lock, nativeHistoryStampName(source, scope),
		nativeHistoryStamp{Version: 1, Source: source, Scope: scope, Fingerprint: fingerprint}, false)
}

// nativeHistoryFingerprint describes retained stores without reading their contents: each entry's
// path, type, size, inode, mtime and ctime. A write, rename, link or permission change moves one of
// them, and no process can set a ctime back, so a matching fingerprint means planning would read the
// same bytes again. Taken before planning, it never certifies a change the plan did not see.
func nativeHistoryFingerprint(stores ...historyStore) (string, error) {
	hash := sha256.New()
	for _, store := range stores {
		root, err := safefile.OpenRoot(store.path)
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(hash, "absent %q\n", store.path)
			continue
		}
		if err != nil {
			return "", err
		}
		fmt.Fprintf(hash, "store %q\n", store.path)
		fingerprintHistoryDir(hash, root, "", store.flat, store.keep, 0)
		_ = root.Close()
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func fingerprintHistoryDir(w io.Writer, dir *os.File, rel string, flat bool, keep func(string) bool, depth int) {
	names, err := dir.Readdirnames(-1)
	if err != nil {
		fmt.Fprintf(w, "unreadable %q %v\n", rel, err)
		return
	}
	slices.Sort(names)
	for _, name := range names {
		if keep != nil && !keep(name) {
			continue
		}
		path := filepath.Join(rel, name)
		var stat unix.Stat_t
		if err := unix.Fstatat(int(dir.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			fmt.Fprintf(w, "unreadable %q %v\n", path, err)
			continue
		}
		directory := stat.Mode&unix.S_IFMT == unix.S_IFDIR
		if directory && flat {
			continue
		}
		fmt.Fprintf(w, "%q %o %d %d %d.%09d %d.%09d\n", path, stat.Mode, stat.Size, stat.Ino,
			stat.Mtim.Sec, stat.Mtim.Nsec, stat.Ctim.Sec, stat.Ctim.Nsec)
		// Planning stops 16 levels below a history root, well inside this bound.
		if !directory || depth >= 32 {
			continue
		}
		child, err := safefile.OpenDir(dir, name)
		if err != nil {
			fmt.Fprintf(w, "unreadable %q %v\n", path, err)
			continue
		}
		fingerprintHistoryDir(w, child, path, false, nil, depth+1)
		_ = child.Close()
	}
}
