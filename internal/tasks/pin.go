package tasks

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// The in-box task tools run on the host but act on a workspace the box can change while a call
// runs, and a path the host resolves by name follows a folder the box swapped for a link (.agent,
// the queue root, a state folder) into another project. So a tool call pins the workspace and each
// queue in it once (PinQueues). Until the call ends, every task read and write in this process
// that names a path inside them goes through those handles (openPinned) instead of the name. The
// helpers take paths so the host commands share them; only the pin changes how a path is opened.

type pinnedFolder struct {
	path, canonical string
	root            *os.Root
}

var pins struct {
	sync.Mutex
	folders []*pinnedFolder
}

// PinQueues pins top, the workspace a box mounts, and each queue root in it, opened from top
// through real folders only, until release. A queue missing now stays reachable only through the
// workspace pin, so it reads as empty and a task created in it is still found.
func PinQueues(top string, roots []string) (release func(), err error) {
	top = filepath.Clean(top)
	canonicalTop, err := filepath.EvalSymlinks(top)
	if err != nil {
		return nil, err
	}
	workspace, err := os.OpenRoot(top)
	if err != nil {
		return nil, err
	}
	held := []*pinnedFolder{{path: top, canonical: canonicalTop, root: workspace}}
	release = func() {
		pins.Lock()
		pins.folders = slices.DeleteFunc(pins.folders, func(f *pinnedFolder) bool { return slices.Contains(held, f) })
		pins.Unlock()
		for _, f := range held {
			f.root.Close()
		}
	}
	for _, queue := range roots {
		queue = filepath.Clean(queue)
		rel, ok := pathBelow(top, queue)
		if !ok {
			release()
			return nil, fmt.Errorf("task queue %s is outside the workspace %s", queue, top)
		}
		root, err := openRealSubroot(workspace, rel)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			release()
			return nil, fmt.Errorf("open task queue %s: %w", queue, err)
		}
		held = append(held, &pinnedFolder{path: queue, canonical: filepath.Join(canonicalTop, rel), root: root})
	}
	pins.Lock()
	pins.folders = append(pins.folders, held...)
	pins.Unlock()
	return release, nil
}

// pathBelow returns path relative to dir when dir holds it ("." for dir itself).
func pathBelow(dir, path string) (string, bool) {
	rel, err := filepath.Rel(dir, path)
	if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// deepestPin returns the pinned folder nearest above path, a copy of its handle the caller closes,
// and path relative to it. A nil copy means no pin holds path. A path is matched as given and as
// its real path, since some callers resolve a queue's links before they open it.
func deepestPin(path string) (*os.Root, *pinnedFolder, string, error) {
	pins.Lock()
	defer pins.Unlock()
	var best *pinnedFolder
	bestRel := ""
	for _, folder := range pins.folders {
		rel, ok := pathBelow(folder.path, path)
		if !ok {
			rel, ok = pathBelow(folder.canonical, path)
		}
		if ok && (best == nil || len(folder.path) > len(best.path)) {
			best, bestRel = folder, rel
		}
	}
	if best == nil {
		return nil, nil, "", nil
	}
	root, err := best.root.OpenRoot(".")
	return root, best, bestRel, err
}

// openPinned opens path through the pinned folder nearest above it, every folder below that pin
// a real folder (openRealSubroot). ok is false when no pin holds path.
func openPinned(path string) (root *os.Root, ok bool, err error) {
	base, _, rel, err := deepestPin(filepath.Clean(path))
	if base == nil && err == nil {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	defer base.Close()
	root, err = openRealSubroot(base, rel)
	return root, true, err
}

// pinnedCanonical is filepath.EvalSymlinks for a path a pin holds: the pinned workspace's real path
// joined with the rest, which the pin's walk proved holds no link.
func pinnedCanonical(path string) (string, bool) {
	base, folder, rel, err := deepestPin(filepath.Clean(path))
	if base != nil {
		base.Close()
	}
	if folder == nil || err != nil {
		return "", false
	}
	return filepath.Join(folder.canonical, rel), true
}

// lstatTaskPath is os.Lstat, except that a path a pin holds is looked up in its pinned parent.
func lstatTaskPath(path string) (os.FileInfo, error) {
	path = filepath.Clean(path)
	parent, ok, err := openPinned(filepath.Dir(path))
	if !ok {
		return os.Lstat(path)
	}
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	return parent.Lstat(filepath.Base(path))
}

// openQueueRoot opens a task queue root: through its pin, or else by name as a real folder.
func openQueueRoot(root string) (*os.Root, error) {
	if queue, ok, err := openPinned(root); ok {
		return queue, err
	}
	return openRealRoot(root)
}
