package box

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/fsidentity"
	"github.com/AndrewDryga/coop/internal/networkstate"
)

func admitIsolatedOptions(cfg *config.Config, spec RunSpec) error {
	parents, err := isolatedMountParents(spec)
	if err != nil || len(parents) == 0 {
		return err
	}
	options, err := limitedExtraArgs(cfg.ExtraRunArgs, spec.ExtraArgs, true)
	if err != nil {
		return fmt.Errorf("isolated launch runtime arguments: %w", err)
	}
	for i := 0; i+1 < len(options); i += 2 {
		if options[i] != "-v" {
			continue
		}
		parts := strings.Split(options[i+1], ":")
		if len(parts) < 2 || len(parts) > 3 {
			return errors.New("isolated runtime mount has an ambiguous descriptor")
		}
		if len(parts) == 3 {
			if _, err := checkedVolumeModes("isolated fork", parts[0], parts[2], looksLikePath(parts[0])); err != nil {
				return err
			}
		}
	}
	return nil
}

// A socket mount is host IPC even when read-only. Check whole directory sources too;
// a friendly directory name must not conceal another daemon's control socket.
func checkIsolatedControlSource(source string, markers map[string]os.FileInfo) error {
	canonical, err := resolveAuthorityPath(source)
	if err != nil {
		return err
	}
	for _, control := range runtimeControlPaths {
		real, err := resolveExisting(control)
		if err != nil {
			return err
		}
		if canonical == real || pathContains(canonical, real) || control != "/" && pathContains(real, canonical) {
			return fmt.Errorf("isolated fork mount %q exposes runtime or kernel control at %q", source, control)
		}
	}
	count := 0
	return filepath.WalkDir(canonical, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			// Docker may interpret a VM-only bind or volume path that does not
			// exist on the host. Missing/unreadable is unknown content, not empty.
			return fmt.Errorf("inspect isolated mount %q for host IPC; use an inspectable independent source: %w", source, err)
		}
		count++
		if count > 1<<20 {
			return errors.New("isolated mount exceeds the host IPC inspection limit")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&(os.ModeSocket|os.ModeNamedPipe|os.ModeDevice) != 0 {
			return fmt.Errorf("isolated fork mount %q contains host IPC or a device at %q", source, path)
		}
		if info.Mode().IsRegular() {
			stat, ok := info.Sys().(*syscall.Stat_t)
			marker := markers[path]
			if !ok || stat.Nlink != 1 && (stat.Nlink != 2 || marker == nil || !os.SameFile(marker, info)) {
				return fmt.Errorf("isolated fork mount %q contains a shared file inode at %q; use an independent copy", source, path)
			}
		}
		return nil
	})
}

// Only exact public markers proved against their inaccessible private half may
// share an inode. An identically named external file never gets this exception.
func isolatedSharedMarkers(spec RunSpec, networkPath string) (map[string]os.FileInfo, error) {
	workspace, err := filepath.EvalSymlinks(spec.Repo)
	if err != nil {
		return nil, err
	}
	markers := map[string]os.FileInfo{}
	for _, name := range []string{forkspace.GenerationMarkerName, networkstate.ProjectApprovalMarker, serviceApprovalMarker} {
		path := filepath.Join(workspace, name)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		switch name {
		case forkspace.GenerationMarkerName:
			repo, identity, err := forkspace.ResolveProjectBinding(workspace)
			if err != nil || identity == nil {
				return nil, errors.Join(errors.New("isolated workspace marker has no generation binding"), err)
			}
			info, err = forkspace.GenerationMarkerInfo(repo, *identity)
			if err != nil {
				return nil, err
			}
		case networkstate.ProjectApprovalMarker:
			if networkPath == "" {
				networkPath, err = NetworkStatePath()
				if err != nil {
					return nil, err
				}
			}
			store, err := networkstate.OpenExisting(networkPath, []string{workspace})
			if err != nil {
				return nil, err
			}
			info, err = store.ProjectMarkerInfo(workspace)
			err = errors.Join(err, store.Close())
			if err != nil {
				return nil, err
			}
		case serviceApprovalMarker:
			anchor, err := serviceApprovalAnchor(workspace, false)
			if err != nil {
				return nil, err
			}
			path, err := serviceApprovalRoot()
			if err != nil {
				return nil, err
			}
			private, err := os.OpenRoot(path)
			if err != nil {
				return nil, err
			}
			info, err = fsidentity.MarkerInfo(serviceAnchorBinding(workspace, anchor, private))
			err = errors.Join(err, private.Close())
			if err != nil {
				return nil, err
			}
		}
		markers[path] = info
	}
	return markers, nil
}
