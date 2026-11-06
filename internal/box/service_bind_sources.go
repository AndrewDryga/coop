package box

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/AndrewDryga/coop/internal/project"
)

func serviceBindPath(file, source string) string {
	if filepath.IsAbs(source) {
		return filepath.Clean(source)
	}
	return filepath.Join(filepath.Dir(file), source)
}

// Inspecting and stopping services must still work after a bind source disappears. Starts alone
// require sources, including dependencies of the selected services, before contacting Compose.
func checkServiceBindSources(workspace, file string, data []byte, selected []string) error {
	var doc composeDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return err
	}
	if len(selected) == 0 {
		for name, service := range doc.Services {
			if profiles, ok := service.Profiles.([]any); ok && len(profiles) > 0 {
				continue
			}
			selected = append(selected, name)
		}
		slices.Sort(selected)
	}
	names, err := composeServiceClosure(doc.Services, selected)
	if err != nil {
		return err
	}
	var p *project.Project
	for _, name := range names {
		for _, volume := range doc.Services[name].Volumes {
			source, _ := bindSourceTarget(volume)
			if !serviceVolumeIsBind(volume, source) {
				continue
			}
			path := serviceBindPath(file, source)
			info, err := os.Stat(path)
			if err != nil {
				return fmt.Errorf("service %q: bind source %q must already exist — create it before starting services: %w", name, source, err)
			}
			if info.Mode().IsRegular() {
				continue
			}
			// A directory, or a Unix socket a service shares (which Compose validation accepts), may be
			// bound unless the project declares this path as one that must be a real file. Anything
			// else, such as a device or a named pipe, must be a regular file.
			if !info.IsDir() && info.Mode()&os.ModeSocket == 0 {
				return fmt.Errorf("service %q: bind source %q must be a regular file", name, source)
			}
			// Frozen controller stacks cannot bind the checkout and must not depend on its mutable
			// project config, so only these binds read the request list.
			if p == nil {
				p, err = project.Load(workspace)
				if err != nil {
					return err
				}
			}
			for _, request := range p.Services.RequireRealFiles {
				if SameFolder(filepath.Join(workspace, filepath.FromSlash(request)), path) {
					return fmt.Errorf("service %q: bind source %q must be a regular file", name, source)
				}
			}
		}
	}
	return nil
}

// Disable Compose's short-form directory creation in the private snapshot too: a source can
// disappear after the preflight. Approval and secret shadowing continue to use the original bytes.
func composeWithoutBindCreation(data []byte) ([]byte, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	services, _ := doc["services"].(map[string]any)
	changed := false
	for _, value := range services {
		service, _ := value.(map[string]any)
		volumes, _ := service["volumes"].([]any)
		for i, entry := range volumes {
			source, target := bindSourceTarget(entry)
			if !serviceVolumeIsBind(entry, source) {
				continue
			}
			volume, ok := entry.(map[string]any)
			if !ok {
				_, writable := serviceVolumeTargetAccess(entry)
				volume = map[string]any{"type": "bind", "source": source, "target": target, "read_only": !writable}
				parts := strings.SplitN(entry.(string), ":", 3)
				if len(parts) == 3 {
					for _, mode := range strings.Split(parts[2], ",") {
						if slices.Contains([]string{"consistent", "cached", "delegated"}, mode) {
							volume["consistency"] = mode
						}
					}
				}
			}
			volume["type"] = "bind"
			volume["bind"] = map[string]any{"create_host_path": false}
			volumes[i] = volume
			changed = true
		}
	}
	if !changed {
		return data, nil
	}
	return yaml.Marshal(doc)
}
