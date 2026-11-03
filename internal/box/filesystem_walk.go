package box

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
)

// Inventory every object, including hidden descendants and .git aliases. The
// coordinator owns the result; workers only read directory metadata. Policy is
// evaluated afterwards in lexical order, so scheduling cannot change origins.
type filesystemInventory struct {
	objects map[string]filesystemObject
	paths   []string
}

func filesystemObjects(root string) (*filesystemInventory, error) {
	return inventoryFilesystem(root, true)
}

// The APFS final check needs every name and every directory identity, but pinned
// objects already protect new public aliases. Other backends use full metadata.
func inventoryFilesystem(root string, fullMetadata bool) (*filesystemInventory, error) {
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
			if r.err != nil {
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
