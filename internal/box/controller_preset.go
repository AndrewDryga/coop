package box

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"

	"github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/preset"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

// The session daemon writes the recipe beside its private provider configuration.
// Only the commitment crosses exec: a large approved role prompt may exceed the
// operating system's per-argument limit.
const ControllerPresetFile = ".coop-controller-preset.json"
const ControllerPresetDigestEnv = "COOP_CONTROLLER_PRESET_DIGEST"

type controllerPreset struct {
	JobDigest string                `json:"job_digest"`
	Targets   []string              `json:"targets"`
	ReadOnly  bool                  `json:"repository_read_only"`
	Preset    workerproto.JobPreset `json:"preset"`
}

func EncodeControllerPreset(jobDigest string, job workerproto.JobSpec) ([]byte, string, error) {
	if job.Preset == nil {
		return nil, "", nil
	}
	data, err := json.Marshal(controllerPreset{JobDigest: jobDigest, Targets: job.Targets, ReadOnly: job.RepositoryReadOnly, Preset: *job.Preset})
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:]), nil
}

// ControllerPresetForChild reconstructs only the preset granted to this host
// child, before any fork-local preset or provider model is selected.
func ControllerPresetForChild(configDir string, target agent.Target, readOnly bool) (*preset.Preset, error) {
	expected := os.Getenv(ControllerPresetDigestEnv)
	if expected == "" {
		return nil, nil
	}
	isJob, err := ControllerJobFromEnvironment()
	if err != nil || !isJob || !filepath.IsAbs(configDir) {
		return nil, errors.New("controller preset is missing its private job authority")
	}
	data, err := agent.ReadCredentialArtifact(filepath.Join(configDir, ControllerPresetFile), 512<<10)
	if err != nil {
		return nil, errors.New("controller preset is unavailable")
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != expected {
		return nil, errors.New("controller preset digest does not match its grant")
	}
	var grant controllerPreset
	if err := json.Unmarshal(data, &grant); err != nil || grant.JobDigest != os.Getenv(ControllerJobEnv) || grant.ReadOnly != readOnly || !slices.Contains(grant.Targets, target.String()) {
		return nil, errors.New("controller preset does not grant this job, target and repository access")
	}
	return grant.Preset.Recipe(grant.Targets, readOnly)
}
