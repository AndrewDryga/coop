package box

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/AndrewDryga/coop/internal/config"
)

// networkExposureRoots is every host directory this run mounts wholesale. The
// owner-private network authority must be outside all of them: an owner-only
// directory is not private when the same owner can reach it from inside the box.
//
// This is the DECLARED set, checked before any host state is created. A filtered
// launch rechecks the exact emitted mount plan against the same rule, so a bind
// added later cannot escape it.
func networkExposureRoots(cfg *config.Config, spec RunSpec) ([]string, error) {
	roots := []string{spec.Repo, projectPolicyRepo(spec)}
	roots = append(roots, ConfigExposureRoots(cfg)...)
	for _, companion := range spec.CompanionRepositories {
		roots = append(roots, companion.HostPath)
	}
	// A configuration with no config dir contributes no host config roots — that
	// is exactly what ConfigExposureRoots reports for it — so it has no
	// credential homes either. Deriving them anyway yields relative paths, which
	// are not mount sources and cannot be checked for isolation at all.
	if cfg != nil && cfg.ConfigDir != "" {
		for _, name := range credentialScope(cfg, spec) {
			roots = append(roots, cfg.AgentDir(name))
		}
	}
	if repo := projectPolicyRepo(spec); repo != "" {
		live, err := LiveBoxes(repo, "")
		if err != nil {
			return nil, err
		}
		for _, observation := range live {
			roots = append(roots, observation.Record.Workspace)
		}
	}
	return roots, nil
}

// checkNamedVolumeExposure inspects the named volumes a filtered workload would
// mount. A local-driver volume is backed by a host path: it can carry network
// authority into the box, or hang off a parent an agent can rename. Inspect the
// existing definition on the bound daemon; never create one to find out.
func (f *filteredExecution) checkNamedVolumeExposure(ctx context.Context, names []string) error {
	if len(names) == 0 {
		return nil
	}
	exposure, err := f.docker.ExistingNamedVolumeExposure(ctx, names)
	if err != nil {
		return err
	}
	roots := append([]string{}, f.unsafeRoots...)
	roots = append(roots, f.record.Project)
	for _, source := range append(append([]string{}, exposure.BindSources...), exposure.ManagedParents...) {
		canonical, err := filepath.EvalSymlinks(source)
		if err != nil || canonical != source {
			return errors.New("named volume source must be canonical; use an explicit bind or recreate the volume with its resolved source path")
		}
		for _, root := range roots {
			if err := safeBindParent(source, root); err != nil {
				return err
			}
		}
	}
	return f.store.CheckExposure(exposure.Sources)
}
