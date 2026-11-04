package box

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

// ControllerRuntimeEnv is host-parent setup authority. It is scrubbed from the environment
// entering the sandbox, just like ControllerJobEnv; repository files cannot create this grant.
const ControllerRuntimeEnv = "COOP_CONTROLLER_RUNTIME"

const controllerRuntimeEnvelopeBytes = 128 << 10

// ControllerRuntime binds a narrow project setup to its authenticated job and private source.
// Repository is the worker-staged immutable source, never the writable job workspace.
type ControllerRuntime struct {
	Version    int                     `json:"version"`
	JobDigest  string                  `json:"job_digest"`
	Repository string                  `json:"repository"`
	Runtime    *workerproto.JobRuntime `json:"runtime"`
}

func (g *ControllerRuntime) validate() error {
	if g == nil || g.Version != 1 || len(g.JobDigest) != 64 || g.Runtime == nil || g.Runtime.Validate() != nil || len(g.Repository) > 4096 || !filepath.IsAbs(g.Repository) || filepath.Clean(g.Repository) != g.Repository {
		return errors.New("controller project runtime authority is invalid")
	}
	digest, err := hex.DecodeString(g.JobDigest)
	if err != nil || hex.EncodeToString(digest) != g.JobDigest {
		return errors.New("controller project runtime job identity is invalid")
	}
	return nil
}

// EncodeControllerRuntime transports a verified job's setup to its host child, never its box.
func EncodeControllerRuntime(jobDigest, repository string, setup *workerproto.JobRuntime) (string, error) {
	if setup == nil {
		return "", nil
	}
	grant := ControllerRuntime{Version: 1, JobDigest: jobDigest, Repository: repository, Runtime: setup}
	if err := grant.validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(grant)
	if len(data) >= controllerRuntimeEnvelopeBytes {
		return "", errors.New("controller project runtime authority is oversized")
	}
	return string(data), err
}

func ControllerRuntimeFromEnvironment() (*ControllerRuntime, error) {
	value := os.Getenv(ControllerRuntimeEnv)
	if value == "" {
		return nil, nil
	}
	if len(value) > controllerRuntimeEnvelopeBytes {
		return nil, errors.New("controller project runtime authority is oversized")
	}
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.DisallowUnknownFields()
	var grant ControllerRuntime
	if err := decoder.Decode(&grant); err != nil {
		return nil, fmt.Errorf("decode controller project runtime: %w", err)
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, errors.New("controller project runtime contains trailing data")
	}
	if err := grant.validate(); err != nil {
		return nil, err
	}
	if grant.JobDigest != os.Getenv(ControllerJobEnv) {
		return nil, errors.New("controller project runtime belongs to another job")
	}
	return &grant, nil
}

// prepareControllerImage uses only the controller's frozen selector and source. The normal
// project builder's sanitized context keeps Git metadata, ignored output and secrets out.
func prepareControllerImage(cfg *config.Config, rt runtime.Runtime, grant *ControllerRuntime, output io.Writer) (string, error) {
	if err := grant.validate(); err != nil {
		return "", err
	}
	if cfg.Egress == "filtered" && grant.Runtime.Dockerfile != "" {
		return "", errors.New("filtered controller jobs cannot use a project toolchain image")
	}
	if grant.Runtime.Dockerfile == "" {
		return SharedImage(cfg.BaseImage, cfg.ImageOverride), nil
	}
	content, err := readRepositoryRegularFileNoFollow(grant.Repository, grant.Runtime.Dockerfile, imageInputFileLimit)
	if err != nil {
		return "", fmt.Errorf("read granted project Dockerfile: %w", err)
	}
	if err := refuseCopiedProjectFile(content, grant.Runtime.Dockerfile); err != nil {
		return "", err
	}
	base := SharedImage(cfg.BaseImage, cfg.ImageOverride)
	inputs := sha256.Sum256([]byte(grant.Repository + "\x00" + grant.Runtime.Dockerfile + "\x00" + base + "\x00" + rt.ImageID(base)))
	image := "coop-job:" + hex.EncodeToString(inputs[:16])
	if ImageExists(rt, image) {
		return image, nil
	}
	if cfg.Egress != "open" {
		return "", errors.New("project toolchain image must be prepared before a restricted job")
	}
	buildCfg := cfg.Clone()
	buildCfg.BaseImage = base
	usesBase := strings.Contains(string(content), "COOP_BASE_IMAGE")
	plan := BuildPlan{Project: true, Dockerfile: grant.Runtime.Dockerfile, Image: image, usesBase: usesBase, BaseFirst: usesBase && !ImageExists(rt, base)}
	if output == nil {
		output = os.Stderr
	}
	if err := BuildPlanned(rt, buildCfg, grant.Repository, plan, false, "", strings.NewReader(""), output); err != nil {
		return "", fmt.Errorf("build project toolchain: %w", err)
	}
	return image, nil
}

// StopControllerServices uses the private workspace's project label, which cannot change when
// an agent edits or deletes its Compose/project file. Only final discard removes named data.
func StopControllerServices(ctx context.Context, rt runtime.Runtime, workspace string, volumes bool) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	project := ComposeProject(workspace)
	if _, err := rt.RemoveByLabelsWithAnonymousVolumes(ctx, map[string]string{composeProjectLabel: project}); err != nil {
		return fmt.Errorf("stop controller project services: %w", err)
	}
	_, networkErr := RemoveProjectNetworks(ctx, rt, project)
	var volumeErr error
	if volumes {
		_, volumeErr = rt.RemoveVolumesByLabel(ctx, composeProjectLabel, project)
	}
	_, filesystemErr := RecoverFilesystemAdmissions(ctx, rt, workspace)
	return errors.Join(networkErr, volumeErr, filesystemErr)
}

type controllerServicesKey struct{}

func validateControllerServices(data []byte) error {
	var document composeDoc
	if err := yaml.Unmarshal(data, &document); err != nil {
		return err
	}
	for name, volume := range document.Volumes {
		if volume.External || volume.Name != "" {
			return fmt.Errorf("controller service volume %q must be project-local", name)
		}
	}
	for _, service := range document.Services {
		switch networks := service.Networks.(type) {
		case nil:
			return nil
		case []any:
			if len(networks) == 0 {
				return nil
			}
			for _, network := range networks {
				if network == "default" {
					return nil
				}
			}
		case map[string]any:
			if _, ok := networks["default"]; ok || len(networks) == 0 {
				return nil
			}
		}
	}
	return errors.New("controller project services require a service on the default network")
}

func controllerServiceOverride(cfg *config.Config, services []string, workspace string, exposedRoots ...string) (string, func(), error) {
	if cfg.CPUs == "" || cfg.Memory == "" || cfg.Pids == "" || cfg.Pids == "0" || cfg.Pids == "unlimited" {
		return "", nil, errors.New("controller project services require finite resource limits")
	}
	var content strings.Builder
	content.WriteString("services:\n")
	for _, service := range services {
		// Host-published ports are unnecessary: the box uses project service DNS.
		// Reset rather than merge so a development port cannot collide with another job.
		fmt.Fprintf(&content, "  %q:\n    cpus: %q\n    mem_limit: %q\n    pids_limit: %s\n    ports: !reset []\n", service, cfg.CPUs, cfg.Memory, cfg.Pids)
	}
	dir, err := privateWorkspaceTempDir(workspace, "coop-controller-services-", exposedRoots...)
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	path := filepath.Join(dir, "override.yml")
	if err := os.WriteFile(path, []byte(content.String()), 0600); err != nil {
		cleanup()
		return "", nil, err
	}
	return path, cleanup, nil
}
