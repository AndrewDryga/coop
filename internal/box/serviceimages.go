package box

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/AndrewDryga/coop/internal/runtime"
	"gopkg.in/yaml.v3"
)

func validServiceImagePins(images map[string]string) bool {
	if len(images) == 0 {
		return false
	}
	for service, id := range images {
		if service == "" || len(id) != len("sha256:")+64 || !strings.HasPrefix(id, "sha256:") {
			return false
		}
		if _, err := hex.DecodeString(strings.TrimPrefix(id, "sha256:")); err != nil {
			return false
		}
	}
	return true
}

func approvedServiceImagesMatch(approved, observed map[string]string) bool {
	for service, id := range approved {
		if observed[service] != id {
			return false
		}
	}
	return true
}

func pinsForServiceGrant(workspace, file string, data []byte, approval ServiceApproval, reviewed map[string]string) (map[string]string, error) {
	names, err := activeElevatedServiceNames(workspace, file, data, approval)
	if err != nil {
		return nil, err
	}
	pins := make(map[string]string, len(names))
	for _, name := range names {
		if reviewed[name] == "" {
			return nil, fmt.Errorf("service %q has no reviewed image ID", name)
		}
		pins[name] = reviewed[name]
	}
	if !validServiceImagePins(pins) {
		return nil, errors.New("no service image was reviewed for the approved host data")
	}
	return pins, nil
}

// elevatedServiceNames is the exact set that can receive approved host data.
// A blocked secret bind remains a decoy and does not elevate its service.
func elevatedServiceNames(workspace, file string, data []byte, volumes []ServiceVolumeAccess) ([]string, error) {
	decoys, _, err := serviceShadowPlan(workspace, file, data)
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for service, entries := range decoys {
		for _, entry := range entries {
			if entry.source != "" && !entry.dir && !entry.writable && entry.bindSource == "" {
				names[service] = true
			}
		}
	}
	for _, volume := range volumes {
		for _, consumer := range volume.Consumers {
			service, _, ok := strings.Cut(consumer, " → ")
			if !ok || service == "" {
				return nil, errors.New("invalid Docker volume consumer")
			}
			names[service] = true
		}
	}
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// A terminal review may fetch an uncached tag through the existing private Docker client.
// Saved IDs are used offline; an already-local tag that moved requires new consent.
func reviewServiceImages(ctx context.Context, rt runtime.Runtime, workspace, file string, data []byte, volumes []ServiceVolumeAccess) (map[string]string, error) {
	names, err := elevatedServiceNames(workspace, file, data, volumes)
	if err != nil || len(names) == 0 {
		return nil, err
	}
	var doc composeDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	prior, _ := ApprovedServiceSecrets(workspace, file, data)
	images := make(map[string]string, len(names))
	for _, name := range names {
		ref := doc.Services[name].Image
		id, inspectErr := rt.ComposeImageID(ctx, ref)
		if inspectErr != nil && prior.Images[name] != "" {
			if saved, err := rt.ComposeImageID(ctx, prior.Images[name]); err == nil && saved == prior.Images[name] {
				id, inspectErr = saved, nil
			}
		}
		if inspectErr != nil {
			if err := rt.PullComposeImage(ref); err != nil {
				return nil, fmt.Errorf("image %q for service %q is unavailable; pull it with Docker, then run 'coop up' again: %w", ref, name, err)
			}
			id, inspectErr = rt.ComposeImageID(ctx, ref)
			if inspectErr != nil {
				return nil, fmt.Errorf("inspect pulled service image %q: %w", ref, inspectErr)
			}
		}
		images[name] = id
	}
	return images, nil
}

func activeElevatedServiceNames(workspace, file string, data []byte, approval ServiceApproval) ([]string, error) {
	decoys, _, err := serviceShadowPlan(workspace, file, data)
	if err != nil {
		return nil, err
	}
	approvedPaths := map[string]bool{}
	for _, path := range approval.Paths {
		approvedPaths[path] = true
	}
	names := map[string]bool{}
	for service, entries := range decoys {
		for _, entry := range entries {
			if approvedPaths[entry.source] && !entry.dir && !entry.writable && entry.bindSource == "" {
				names[service] = true
			}
		}
	}
	for _, volume := range approval.Volumes {
		for _, consumer := range volume.Consumers {
			service, _, ok := strings.Cut(consumer, " → ")
			if !ok {
				return nil, errors.New("invalid approved Docker volume consumer")
			}
			names[service] = true
		}
	}
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// pinServiceStart adds a private Compose override for approved host-data consumers only.
// Missing image IDs fail before any Compose command can create a container.
func pinServiceStart(ctx context.Context, rt runtime.Runtime, workspace, file string, data []byte, args []string, exposedRoots ...string) ([]string, func(), error) {
	approval, ok := ApprovedServiceSecrets(workspace, file, data)
	if !ok {
		return args, func() {}, nil
	}
	names, err := activeElevatedServiceNames(workspace, file, data, approval)
	if err != nil {
		return nil, nil, err
	}
	if len(names) == 0 {
		return args, func() {}, nil
	}
	type imageOverride struct {
		Image      string `yaml:"image"`
		PullPolicy string `yaml:"pull_policy"`
	}
	override := struct {
		Services map[string]imageOverride `yaml:"services"`
	}{Services: make(map[string]imageOverride, len(names))}
	for _, name := range names {
		id := approval.Images[name]
		if id == "" {
			return nil, nil, fmt.Errorf("service %q has no approved image ID — run 'coop up' again", name)
		}
		observed, err := rt.ComposeImageID(ctx, id)
		if err != nil || observed != id {
			return nil, nil, fmt.Errorf("approved image %s for service %q is unavailable — run 'coop up' to review it again", id, name)
		}
		override.Services[name] = imageOverride{Image: id, PullPolicy: "never"}
	}
	encoded, err := yaml.Marshal(override)
	if err != nil {
		return nil, nil, err
	}
	dir, err := privateWorkspaceTempDir(workspace, "coop-service-images-", exposedRoots...)
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	path := filepath.Join(dir, "images.yml")
	if err := os.WriteFile(path, encoded, 0o400); err != nil {
		cleanup()
		return nil, nil, err
	}
	return append(args, "-f", path), cleanup, nil
}
