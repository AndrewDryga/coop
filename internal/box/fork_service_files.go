package box

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
	"golang.org/x/sys/unix"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/safefile"
)

// CopyForkServiceFiles populates a newly cloned, unpublished fork. It copies files, not grants;
// service approval and the box's secret shadowing still decide who may read them.
func CopyForkServiceFiles(parent, fork string) error {
	p, err := project.Load(parent)
	if err != nil || len(p.Services.RequireRealFiles) == 0 {
		return err
	}
	parent, err = filepath.EvalSymlinks(parent)
	if err != nil {
		return err
	}
	source, err := safefile.OpenRoot(parent)
	if err != nil {
		return err
	}
	defer source.Close()
	destination, err := safefile.OpenRoot(fork)
	if err != nil {
		return err
	}
	defer destination.Close()
	var created []forkServiceDirectory
	defer func() {
		for _, dir := range created {
			_ = dir.file.Close()
		}
	}()
	files, dirs := make(map[string]bool), make(map[string]bool)
	for _, path := range p.Services.RequireRealFiles {
		rel := filepath.FromSlash(path)
		files[rel], dirs[filepath.Dir(rel)] = true, true
	}
	compose := ComposeFileAt(parent, p.ComposeRel())
	if compose != "" {
		// A fork may be opened to repair this file. Only validated definitions may add
		// siblings to the copy; explicit requests still work when Compose is invalid.
		data, err := readValidatedCompose(compose, parent, false)
		var doc composeDoc
		if err == nil {
			if err := yaml.Unmarshal(data, &doc); err != nil {
				return err
			}
		}
		for _, service := range doc.Services {
			for _, volume := range service.Volumes {
				bind, _ := bindSourceTarget(volume)
				if !serviceVolumeIsBind(volume, bind) {
					continue
				}
				rel, err := RepoRelative(parent, serviceBindPath(compose, bind))
				if err != nil {
					return err
				}
				if rel != "." && dirs[filepath.Dir(rel)] && !files[rel] {
					files[rel] = false
				}
			}
		}
	}
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	for _, path := range paths {
		copied, err := copyForkServiceFile(source, destination, path, files[path], &created)
		if err != nil {
			return fmt.Errorf("copy service file %q into fork: %w", path, err)
		}
		if copied {
			// A requested file may be untracked without being ignored in the parent. Keep the
			// copy out of commits and fork dirty-state checks, using an exact literal pattern.
			pattern := strings.NewReplacer("\\", "\\\\", "*", "\\*", "?", "\\?", "[", "\\[", "]", "\\]", " ", "\\ ").Replace(filepath.ToSlash(path))
			if err := forkspace.Exclude(fork, "/"+pattern); err != nil {
				return err
			}
		}
	}
	// Keep new directories writable until all siblings are copied, including when the
	// parent's directory is read-only. Owner write keeps the fork removable; group/other
	// access and existing clone directories keep their own modes.
	for i := len(created) - 1; i >= 0; i-- {
		if err := created[i].file.Chmod(created[i].mode); err != nil {
			return err
		}
	}
	return nil
}

type forkServiceDirectory struct {
	file *os.File
	mode os.FileMode
}

func copyForkServiceFile(source, destination *os.File, path string, required bool, created *[]forkServiceDirectory) (bool, error) {
	if !filepath.IsLocal(path) || path == "." || strings.ContainsAny(path, "\r\n") {
		return false, errors.New("service file must be a local file path on one line")
	}
	for _, part := range strings.Split(path, string(filepath.Separator)) {
		if strings.EqualFold(part, ".git") || strings.EqualFold(part, ".coop") {
			return false, errors.New("service file cannot be repository bookkeeping")
		}
	}
	dir, err := forkServiceFileDir(source, filepath.Dir(path), nil, nil)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer dir.Close()
	name := filepath.Base(path)
	mode, err := safefile.LstatMode(dir, name)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !mode.IsRegular() {
		if !required {
			return false, nil
		}
		return false, errors.New("required service file must be a regular file, not a link or directory")
	}
	in, err := safefile.OpenRegular(dir, name)
	if err != nil {
		return false, err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return false, err
	}
	outDir, err := forkServiceFileDir(destination, filepath.Dir(path), source, created)
	if err != nil {
		return false, err
	}
	defer outDir.Close()
	fd, err := unix.Openat(int(outDir.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, uint32(info.Mode().Perm()))
	if errors.Is(err, os.ErrExist) {
		mode, err := safefile.LstatMode(outDir, name)
		if err == nil && !mode.IsRegular() {
			err = errors.New("fork destination is not a regular file")
		}
		return false, err // the clone's tracked contents win
	}
	if err != nil {
		return false, err
	}
	out := os.NewFile(uintptr(fd), name)
	_, copyErr := io.Copy(out, in)
	// Preserve service-readable permissions even when Coop runs with a private umask.
	modeErr := out.Chmod(info.Mode().Perm())
	return true, errors.Join(copyErr, modeErr, out.Close())
}

func forkServiceFileDir(root *os.File, path string, source *os.File, created *[]forkServiceDirectory) (*os.File, error) {
	dir, err := safefile.CloneDir(root)
	if err != nil {
		return nil, err
	}
	if path == "." {
		return dir, nil
	}
	rel := ""
	for _, part := range strings.Split(path, string(filepath.Separator)) {
		rel = filepath.Join(rel, part)
		made := false
		if source != nil {
			err := unix.Mkdirat(int(dir.Fd()), part, 0o700)
			if err != nil && !errors.Is(err, os.ErrExist) {
				dir.Close()
				return nil, err
			}
			made = err == nil
		}
		next, err := safefile.OpenDir(dir, part)
		dir.Close()
		if err != nil {
			return nil, fmt.Errorf("open directory %q without following symlinks: %w", rel, err)
		}
		dir = next
		if made {
			original, err := safefile.OpenDir(source, rel)
			if err != nil {
				dir.Close()
				return nil, err
			}
			info, err := original.Stat()
			original.Close()
			if err != nil {
				dir.Close()
				return nil, err
			}
			pin, err := safefile.CloneDir(dir)
			if err != nil {
				dir.Close()
				return nil, err
			}
			*created = append(*created, forkServiceDirectory{file: pin, mode: info.Mode().Perm() | 0o200})
		}
	}
	return dir, nil
}
