package liveprovider

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/AndrewDryga/coop/internal/testutil/procharness"
)

// CopyRegularTree publishes a bounded private copy of one source subtree. It rejects links and
// special files, reads every file through the same replacement-resistant boundary as credentials,
// and leaves no partial destination on failure.
func CopyRegularTree(sourceRoot, source, destination string, maxBytes int64) error {
	if maxBytes <= 0 {
		return errors.New("regular tree size limit must be positive")
	}
	canonicalRoot, canonicalSource, err := canonicalSourcePath(sourceRoot, source)
	if err != nil {
		return errors.New("regular tree source escapes its root")
	}
	rootInfo, err := os.Lstat(canonicalSource)
	if err != nil || rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return errors.New("regular tree source is not a non-symlink directory")
	}

	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return errors.New("create regular tree parent")
	}
	if _, err := os.Lstat(destination); err == nil {
		return errors.New("regular tree destination already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("inspect regular tree destination")
	}
	stage, err := os.MkdirTemp(parent, ".coop-live-tree-")
	if err != nil {
		return errors.New("create regular tree staging directory")
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(stage)
		}
	}()
	if err := os.Chmod(stage, 0o700); err != nil {
		return errors.New("secure regular tree staging directory")
	}

	var total int64
	ordinal := 0
	err = filepath.WalkDir(canonicalSource, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return errors.New("inspect regular tree source")
		}
		relative, err := filepath.Rel(canonicalSource, path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return errors.New("regular tree entry escapes its source")
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("regular tree contains a symlink")
		}
		if entry.IsDir() {
			if _, err := procharness.CanonicalUnderRoot(canonicalRoot, path); err != nil {
				return errors.New("regular tree directory changed while copying")
			}
			if relative == "." {
				return nil
			}
			if err := os.Mkdir(filepath.Join(stage, relative), 0o700); err != nil {
				return errors.New("create regular tree directory")
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return errors.New("regular tree contains a special file")
		}
		state, err := readSource(canonicalRoot, path, "preset", ordinal, nil)
		ordinal++
		if err != nil || !state.exists {
			return errors.New("regular tree file is not stable and private")
		}
		if state.size > maxBytes-total {
			return errors.New("regular tree exceeds the size limit")
		}
		total += state.size
		if err := writeCopy(filepath.Join(stage, relative), state.data, state.info); err != nil {
			return errors.New("copy regular tree file")
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := os.Rename(stage, destination); err != nil {
		return errors.New("publish regular tree destination")
	}
	published = true
	return nil
}
