package box

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// BuildBaseCandidate builds the ordinary release-owned image for inspection,
// without publishing host metadata or reclaiming any operator's images.
func BuildBaseCandidate(ctx context.Context, docker *runtime.Docker, stdout, stderr *os.File) (string, error) {
	if ctx == nil || docker == nil || !boxAgentIdentity().valid() {
		return "", errors.New("base construction requires a bound runtime and supported user")
	}
	info := docker.Info()
	platform := agents.ClientPlatform{OS: info.OSType, Architecture: runtime.Architecture(info.Architecture), Libc: "glibc"}
	files, err := baseImageDefinition(platform)
	if err != nil {
		return "", err
	}
	data, err := imageContext(files)
	if err != nil {
		return "", err
	}
	id := boxAgentIdentity()
	spec := runtime.DockerBuild{Platform: platform.OS + "/" + platform.Architecture, Args: map[string]string{
		"NODE_IMAGE": pinnedNodeImage, "COOP_BOX_UID": fmt.Sprint(id.uid), "COOP_BOX_GID": fmt.Sprint(id.gid),
	}}
	definition, err := json.Marshal(struct {
		Spec    runtime.DockerBuild
		Context []byte
	}{spec, data})
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(definition)
	spec.Tag = "coop-base-candidate:" + hex.EncodeToString(hash[:16])
	spec.Labels = map[string]string{"coop.base.build": hex.EncodeToString(hash[:])}
	image, err := docker.BuildImage(ctx, spec, data, stdout, stderr)
	return image.ID, err
}
