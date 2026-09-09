package box

import (
	"context"
	"encoding/csv"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/AndrewDryga/coop/internal/runtime"
)

// Check the complete emitted mount plan, not only its config ancestors. The
// sole authority-tree exception is an exact, locally generated mount source
// recorded by composition; the runfiles directory itself is never exposed.
func (f *filteredExecution) validateMounts(options, files, directories []string) error {
	root, err := f.store.RunFilesPath(f.record.ID)
	if err != nil || root != f.runfiles {
		return errors.New("network workload files changed during composition")
	}
	generated := make(map[string]os.FileInfo, len(files)+len(directories))
	for _, list := range [][]string{files, directories} {
		for _, name := range list {
			canonical, err := filepath.EvalSymlinks(name)
			info, statErr := os.Lstat(name)
			rel, relErr := filepath.Rel(root, name)
			if err != nil || statErr != nil || relErr != nil || canonical != name || rel == "." || !filepath.IsLocal(rel) ||
				!info.IsDir() && !info.Mode().IsRegular() {
				return errors.New("generated network workload mount is not an owned descendant")
			}
			generated[name] = info
		}
	}
	var exposed, volumes []string
	bindings := map[string]os.FileInfo{}
	directoryRoots := append([]string{}, f.unsafeRoots...)
	directoryRoots = append(directoryRoots, f.record.Project)
	observations, err := LiveBoxes(f.record.Project, "")
	if err != nil {
		return err
	}
	for _, observation := range observations {
		directoryRoots = append(directoryRoots, observation.Record.Workspace)
	}
	for i := 0; i < len(options); i++ {
		switch options[i] {
		case "--init", "-i", "-t", "-it":
			continue
		case "--label", "-e", "-w", "--memory", "--pids-limit", "--cpus":
			i++
			if i >= len(options) {
				return errors.New("incomplete restricted workload option")
			}
			continue
		case "--cap-drop", "--security-opt":
			key := options[i]
			i++
			if i >= len(options) || key == "--cap-drop" && options[i] != "ALL" || key == "--security-opt" && options[i] != "no-new-privileges" {
				return errors.New("restricted workload security options cannot be overridden")
			}
			continue
		case "-v", "--env-file":
		default:
			return errors.New("unqualified restricted workload option")
		}
		kind := options[i]
		i++
		if i >= len(options) {
			return errors.New("incomplete network workload mount")
		}
		source := options[i]
		if kind == "-v" {
			parts := strings.Split(source, ":")
			if len(parts) != 2 && len(parts) != 3 || len(parts) == 3 && parts[2] != "ro" || len(parts) > 1 && !filepath.IsAbs(parts[1]) {
				return errors.New("ambiguous network workload mount")
			}
			source = parts[0]
			// A named volume has no host path here. Its backing source is a
			// daemon fact, inspected below, never guessed from this string.
			if !looksLikePath(source) {
				volumes = append(volumes, source)
				continue
			}
		}
		if !filepath.IsAbs(source) || strings.ContainsAny(source, "\x00\r\n") {
			return errors.New("network workload source must be an absolute host path")
		}
		if prior, ok := generated[source]; ok {
			info, err := os.Lstat(source)
			if err != nil || !os.SameFile(prior, info) || kind == "--env-file" && !info.Mode().IsRegular() {
				return errors.New("generated network workload source was replaced")
			}
			if kind == "-v" {
				bindings[source] = info
			}
			continue
		}
		if kind == "--env-file" {
			return errors.New("restricted workload environment must be an owned immutable snapshot")
		}
		canonical, err := filepath.EvalSymlinks(source)
		if err != nil || strings.ContainsAny(canonical, ":\x00\r\n") {
			return errors.New("network workload source cannot be frozen unambiguously")
		}
		info, err := os.Lstat(canonical)
		if err != nil || !info.IsDir() && !info.Mode().IsRegular() {
			return errors.New("network workload mount source is not a regular file or directory")
		}
		parts := strings.Split(options[i], ":")
		parts[0] = canonical
		options[i] = strings.Join(parts, ":")
		bindings[canonical] = info
		exposed = append(exposed, canonical)
		if info.IsDir() {
			directoryRoots = append(directoryRoots, canonical)
		}
	}
	if err := f.store.CheckExposure(exposed); err != nil {
		return err
	}
	if err := f.store.CheckExposure(directoryRoots); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), filteredControlTimeout)
	defer cancel()
	if err := f.checkNamedVolumeExposure(ctx, volumes); err != nil {
		return err
	}
	for _, source := range exposed {
		for _, root := range directoryRoots {
			if err := safeBindParent(source, root); err != nil {
				return err
			}
		}
	}
	f.bindSources = bindings
	return f.checkBindings()
}

// Docker accepts bind paths, not directory capabilities. A nested source whose
// parent an agent can rename cannot be qualified by restatting it before start.
// Keep live root binds; refuse nested overlays rather than silently copy them
// into a stale snapshot or remove their read-only protection.
func safeBindParent(source, root string) error {
	if root == "" {
		return nil
	}
	rootInfo, err := os.Stat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("network mount parent authority is unavailable")
	}
	if !rootInfo.IsDir() {
		return nil
	}
	for parent := filepath.Dir(source); ; parent = filepath.Dir(parent) {
		info, err := os.Stat(parent)
		if err != nil {
			return errors.New("network mount parent identity is unavailable")
		}
		if os.SameFile(rootInfo, info) {
			return errors.New("restricted networking cannot bind a nested source with an agent-writable parent; use an independent repository or credential root")
		}
		if parent == filepath.Dir(parent) {
			return nil
		}
	}
}

func (f *filteredExecution) checkBindings() error {
	observations, err := LiveBoxes(f.record.Project, "")
	if err != nil {
		return err
	}
	var roots []string
	for _, observation := range observations {
		roots = append(roots, observation.Record.Workspace)
	}
	if err := f.store.CheckExposure(roots); err != nil {
		return err
	}
	for source, prior := range f.bindSources {
		info, err := os.Lstat(source)
		if err != nil || !os.SameFile(info, prior) || info.Mode().Type() != prior.Mode().Type() {
			return errors.New("restricted bind source changed after capture")
		}
		for _, root := range roots {
			if err := safeBindParent(source, root); err != nil {
				return err
			}
		}
	}
	return nil
}

// Verify the daemon's configured topology before start; Docker installs binds
// during start. Frozen, independent source roots above close the agent's path
// replacement opportunity. Inspection strings alone are not inode custody.
func verifyNetworkMounts(actual []runtime.DockerMount, tmpfs map[string]string, options []string) error {
	expected, err := networkMountPlan(options)
	if err != nil {
		return err
	}
	// Docker reports --tmpfs in HostConfig.Tmpfs, not in Mounts. Verify its
	// options too: a writable private directory without UID/mode/noexec is
	// not interchangeable with the requested guard IPC boundary.
	remainingTmpfs := len(tmpfs)
	for i := 0; i < len(options); i++ {
		if options[i] == "--tmpfs" {
			i++
			destination, constraints, ok := strings.Cut(options[i], ":")
			if !ok || tmpfs[destination] != constraints {
				return errors.New("restricted container tmpfs differs from the plan")
			}
			delete(expected, destination)
			remainingTmpfs--
		}
	}
	if remainingTmpfs != 0 || len(actual) != len(expected) {
		return errors.New("restricted container has unexpected mount topology")
	}
	seen := map[string]bool{}
	for _, mount := range actual {
		want, ok := expected[mount.Destination]
		if !ok || seen[mount.Destination] || want.Type != mount.Type || want.RW != mount.RW {
			return errors.New("restricted container mount identity differs from the plan")
		}
		seen[mount.Destination] = true
		switch want.Type {
		case "bind":
			if want.Source != mount.Source {
				return errors.New("restricted container bind source differs from the plan")
			}
		case "volume":
			if mount.Name != want.Name {
				return errors.New("restricted container volume differs from the plan")
			}
		case "tmpfs":
		default:
			return errors.New("restricted container mount type is unqualified")
		}
	}
	return nil
}

func networkMountPlan(options []string) (map[string]runtime.DockerMount, error) {
	result := map[string]runtime.DockerMount{}
	for i := 0; i < len(options); i++ {
		kind := options[i]
		if kind != "-v" && kind != "--mount" && kind != "--tmpfs" {
			continue
		}
		i++
		if i == len(options) {
			return nil, errors.New("incomplete restricted mount plan")
		}
		mount := runtime.DockerMount{RW: true}
		switch kind {
		case "-v":
			parts := strings.Split(options[i], ":")
			if len(parts) != 2 && len(parts) != 3 || len(parts) == 3 && parts[2] != "ro" {
				return nil, errors.New("ambiguous restricted mount plan")
			}
			mount.Source, mount.Destination, mount.Type = parts[0], parts[1], "bind"
			mount.RW = len(parts) == 2
			if !filepath.IsAbs(mount.Source) {
				mount.Type, mount.Name = "volume", mount.Source
				mount.Source = ""
			}
		case "--mount":
			fields, err := csv.NewReader(strings.NewReader(options[i])).Read()
			if err != nil {
				return nil, errors.New("invalid restricted mount descriptor")
			}
			for _, field := range fields {
				key, value, _ := strings.Cut(field, "=")
				switch key {
				case "type":
					mount.Type = value
				case "source":
					mount.Source = value
				case "target":
					mount.Destination = value
				case "readonly":
					mount.RW = false
				default:
					return nil, errors.New("unknown restricted mount field")
				}
			}
			if mount.Type == "volume" {
				mount.Name, mount.Source = mount.Source, ""
			}
		case "--tmpfs":
			mount.Type = "tmpfs"
			mount.Destination, _, _ = strings.Cut(options[i], ":")
		}
		if !filepath.IsAbs(mount.Destination) {
			return nil, errors.New("restricted mount destination is not absolute")
		}
		if _, duplicate := result[mount.Destination]; duplicate {
			return nil, errors.New("restricted mount destination occurs twice")
		}
		result[mount.Destination] = mount
	}
	return result, nil
}
