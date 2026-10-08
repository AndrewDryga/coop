package box

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/AndrewDryga/coop/internal/runtime"
	"gopkg.in/yaml.v3"
)

// Approval never widens an isolated boundary. Inspect every referenced volume on the
// same frozen daemon that will start Compose, including project-prefixed volumes.
type isolatedServiceParentKey struct{}

func validateIsolatedServiceVolumes(ctx context.Context, rt runtime.Runtime, workspace, file, owner string, data []byte) error {
	spec := RunSpec{Repo: workspace}
	spec.IsolatedParent, _ = ctx.Value(isolatedServiceParentKey{}).(string)
	parents, err := isolatedMountParents(spec)
	if err != nil || len(parents) == 0 {
		return err
	}
	var doc composeDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return err
	}
	var names []string
	var options []string
	for serviceName, service := range doc.Services {
		for _, entry := range service.Volumes {
			var source string
			switch volume := entry.(type) {
			case string:
				parts := strings.SplitN(volume, ":", 3)
				if len(parts) > 1 {
					source = parts[0]
				}
			case map[string]any:
				kind, _ := volume["type"].(string)
				if !slices.Contains([]string{"", "bind", "volume", "tmpfs"}, kind) {
					return fmt.Errorf("isolated service %q has unsupported volume type %q", serviceName, kind)
				}
				source, _ = volume["source"].(string)
				if kind == "tmpfs" && source != "" || kind == "bind" && source == "" || kind == "volume" && looksLikePath(source) {
					return fmt.Errorf("isolated service %q has an ambiguous volume source", serviceName)
				}
			}
			if serviceVolumeIsBind(entry, source) {
				if !filepath.IsAbs(source) {
					source = filepath.Join(filepath.Dir(file), source)
				}
				_, writable := serviceVolumeTargetAccess(entry)
				access := "ro"
				if writable {
					access = "rw"
				}
				options = append(options, "-v", source+":/isolated-service:"+access)
				continue
			}
			source = namedVolumeSource(entry)
			declaration, exists := doc.Volumes[source]
			if source == "" || !exists {
				continue
			}
			name := declaration.Name
			if name == "" {
				name = source
				if !declaration.External {
					name = ComposeProjectFor(workspace, owner) + "_" + source
				}
			}
			if !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	}
	if len(names) == 0 {
		return validateAuthorityMounts(ctx, spec, options, "", nil, authorityMountAllowlist{})
	}
	docker, err := runtime.InspectDocker(ctx, rt)
	if err != nil {
		return fmt.Errorf("inspect isolated service volumes: %w", err)
	}
	defer docker.Close()
	for _, name := range names {
		options = append(options, "-v", name+":/isolated-service:ro")
	}
	return validateAuthorityMounts(ctx, spec, options, "", docker.ExistingNamedVolumeExposure, authorityMountAllowlist{})
}
