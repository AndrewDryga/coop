package box

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
)

// Inventory every object, including hidden descendants and .git aliases. The
// coordinator owns the result; workers only read directory metadata. Policy is
// evaluated afterwards in lexical order, so scheduling cannot change origins.
type filesystemInventory struct {
	objects map[string]filesystemObject
	paths   []string
}

func filesystemObjects(root, exclude string) (*filesystemInventory, error) {
	return inventoryFilesystem(root, exclude, true)
}

// The APFS final check needs every name and every directory identity, but pinned
// objects already protect new public aliases. Other backends use full metadata.
func inventoryFilesystem(root, exclude string, fullMetadata bool) (*filesystemInventory, error) {
	info, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("filesystem admission root %s is not a real directory", root)
	}
	id, ok := identityOf(info)
	if !ok {
		return nil, errors.New("filesystem object identity unavailable")
	}
	objects := map[string]filesystemObject{".": {id: id, mode: info.Mode().Type()}}
	type entry struct {
		rel string
		filesystemObject
	}
	type result struct {
		dir     string
		entries []entry
		err     error
		last    bool
		// unsettled is what could not be read under a probe-named root entry (probeLeaf), which
		// may be another admission's probe going away; leaveOutProbes judges it after the walk.
		unsettled []unsettledProbe
	}
	jobs := make(chan entry)
	results := make(chan result)
	done := make(chan struct{})
	var workers sync.WaitGroup
	defer func() { close(done); close(jobs); workers.Wait() }()
	emit := func(r result) bool {
		select {
		case results <- r:
			return true
		case <-done:
			return false
		}
	}
	readDirectory := func(directory entry) {
		dir := directory.rel
		f, err := os.Open(filepath.Join(root, dir))
		if err != nil {
			emit(result{dir: dir, err: err, last: true})
			return
		}
		defer f.Close()
		opened, err := f.Stat()
		if err != nil {
			emit(result{dir: dir, err: err, last: true})
			return
		}
		openedID, ok := identityOf(opened)
		if !ok || !opened.IsDir() || openedID != directory.id {
			emit(result{dir: dir, err: errors.New("filesystem directory changed during inventory"), last: true})
			return
		}
		for {
			// Bound each worker's retained metadata as well as the final map.
			entries, err := f.ReadDir(256)
			r := result{dir: dir, err: err, last: err != nil}
			if errors.Is(err, io.EOF) {
				r.err = nil
			} else if err != nil {
				emit(r)
				return
			}
			for _, d := range entries {
				rel := d.Name()
				if dir != "." {
					rel = dir + "/" + rel
				}
				if rel == exclude {
					continue
				}
				if !fullMetadata && !d.IsDir() {
					r.entries = append(r.entries, entry{rel, filesystemObject{mode: d.Type()}})
					continue
				}
				info, err := d.Info()
				var identity fileIdentity
				if err == nil {
					var ok bool
					if identity, ok = identityOf(info); !ok || info.Mode().Type() != d.Type() {
						err = errors.New("filesystem object identity changed during inventory")
					}
				}
				if err != nil {
					if leaf := probeLeaf(rel); leaf != "" {
						r.unsettled = append(r.unsettled, unsettledProbe{leaf, err})
						continue
					}
					r.err, r.last = err, true
					break
				}
				r.entries = append(r.entries, entry{rel, filesystemObject{id: identity, mode: info.Mode().Type()}})
			}
			if !emit(r) || r.last || r.err != nil {
				return
			}
		}
	}
	for range min(8, runtime.GOMAXPROCS(0)) {
		workers.Go(func() {
			for dir := range jobs {
				readDirectory(dir)
			}
		})
	}
	children := map[string][]string{}
	unsettled := map[string]error{}
	pending := []entry{{".", objects["."]}}
	active := 0
	for len(pending) > 0 || active > 0 {
		var send chan entry
		var next entry
		if len(pending) > 0 {
			send, next = jobs, pending[len(pending)-1]
		}
		select {
		case send <- next:
			pending = pending[:len(pending)-1]
			active++
		case r := <-results:
			if r.last {
				active--
			}
			for _, u := range r.unsettled {
				unsettled[u.leaf] = errors.Join(unsettled[u.leaf], u.err)
			}
			if leaf := probeLeaf(r.dir); r.err != nil && leaf != "" {
				unsettled[leaf] = errors.Join(unsettled[leaf], r.err)
			} else if r.err != nil {
				return nil, r.err
			}
			for _, e := range r.entries {
				if (fullMetadata || e.mode.IsDir()) && e.id.dev != id.dev {
					return nil, errors.New("filesystem admission cannot cross a mounted filesystem")
				}
				if len(objects) >= 1_000_000 {
					return nil, errors.New("filesystem inventory exceeds admission bounds")
				}
				objects[e.rel] = e.filesystemObject
				children[r.dir] = append(children[r.dir], e.rel)
				if e.mode.IsDir() {
					pending = append(pending, e)
				}
			}
		}
	}
	if err := leaveOutProbes(root, objects, children, unsettled); err != nil {
		return nil, err
	}
	// Sort siblings, then visit subtrees, matching WalkDir without comparing
	// long shared prefixes across the entire repository.
	for _, entries := range children {
		slices.Sort(entries)
	}
	paths := make([]string, 0, len(objects))
	var visit func(string)
	visit = func(rel string) {
		paths = append(paths, rel)
		for _, child := range children[rel] {
			visit(child)
		}
	}
	visit(".")
	return &filesystemInventory{objects: objects, paths: paths}, nil
}

// unsettledProbe is what a walk could not read under one probe-named root entry.
type unsettledProbe struct {
	leaf string
	err  error
}

// probeLeaf is the probe-named root entry (newFilesystemProbe) that rel is or lies under, or "".
func probeLeaf(rel string) string {
	leaf, _, _ := strings.Cut(rel, "/")
	if strings.HasPrefix(leaf, filesystemProbePrefix) {
		return leaf
	}
	return ""
}

// leaveOutProbes removes from an inventory the probe directories other live admissions made at
// root, and everything under them. Parallel launches in one project (an editor's warm boxes) each
// make one, and each removes its own once its shares qualify. Another launch's walk took that
// directory and the .env inside it for project content, then failed its final check with
// "lstat <repo>/.coop-fs-probe-...: no such file or directory" once the directory went away.
//
// A probe counts only when an admission that is not provably gone recorded it with this identity,
// in custody state no box can write; a recorded probe the walk found gone counts too, since nothing
// else can take that name before it goes. Any other probe-named entry is project content like the
// rest, and what the walk could not read under it fails the walk as it always did. A dead
// admission's probe is walked too, so an entry planted in one its owner could not retire stays
// protected.
func leaveOutProbes(root string, objects map[string]filesystemObject, children map[string][]string, unsettled map[string]error) error {
	probes := liveProbes(root)
	genuine := func(leaf string) bool {
		recorded, ok := probes[leaf]
		object, seen := objects[leaf]
		return ok && (!seen || object.mode.IsDir() && object.id == recorded)
	}
	for leaf, err := range unsettled {
		if !genuine(leaf) {
			return err
		}
	}
	var drop func(string)
	drop = func(rel string) {
		delete(objects, rel)
		for _, child := range children[rel] {
			drop(child)
		}
		delete(children, rel)
	}
	kept := children["."][:0]
	for _, rel := range children["."] {
		if probeLeaf(rel) != "" && genuine(rel) {
			drop(rel)
			continue
		}
		kept = append(kept, rel)
	}
	children["."] = kept
	return nil
}

// liveProbes maps the probe directories at root that admissions not provably gone recorded to the
// identity each was made with. An unreadable record contributes nothing: its probe, if any, is
// walked as project content.
func liveProbes(root string) map[string]fileIdentity {
	probes := map[string]fileIdentity{}
	state, err := serviceStateRoot("filesystem")
	if err != nil {
		return probes
	}
	entries, err := os.ReadDir(state)
	if err != nil {
		return probes
	}
	host, _ := thisHost()
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "coop-fs-") {
			continue
		}
		record, err := readFilesystemRecord(filepath.Join(state, entry.Name()))
		if err != nil {
			continue
		}
		if supervisor, _ := parseSupervisorLabel(record.Supervisor); supervisorProvablyDead(supervisor, host) {
			continue
		}
		for _, probe := range record.Probes {
			if filepath.Clean(probe.Root) == filepath.Clean(root) {
				probes[probe.Leaf] = fileIdentity{dev: probe.Dev, ino: probe.Ino}
			}
		}
	}
	return probes
}
