package agent

import (
	"os"
	"slices"
	"syscall"
)

// A walk calls visit while the history root is open. It stops on the first true
// result, so exact lookups and the exit footer can share the same file discovery.
type sessionFileVisitor func(root *os.Root, path string, info os.FileInfo) bool

// watchSessionFiles reads IDs only from new or updated files. Status, contents
// and persistence are provider-owned, so identify can decline a candidate.
func watchSessionFiles(walk func(sessionFileVisitor) bool, identify func(*os.Root, string) string) func() []string {
	before := map[string]os.FileInfo{}
	walk(func(_ *os.Root, path string, info os.FileInfo) bool {
		before[path] = info
		return false
	})
	return func() []string {
		written := map[string]bool{}
		walk(func(root *os.Root, path string, info os.FileInfo) bool {
			prev := before[path]
			if prev != nil && prev.Size() == info.Size() && prev.ModTime().Equal(info.ModTime()) && os.SameFile(prev, info) {
				return false
			}
			if id := identify(root, path); ValidSessionID(id) {
				written[id] = true
			}
			return false
		})
		ids := make([]string, 0, len(written))
		for id := range written {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		return ids
	}
}

// History may change between visiting a directory and opening an entry. Keep a
// replaced symlink or FIFO from redirecting or blocking the host reader.
func openSessionMetadata(root *os.Root, path string) (*os.File, error) {
	f, err := root.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, &os.PathError{Op: "open session metadata", Path: path, Err: os.ErrInvalid}
	}
	return f, nil
}
