package runtime

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
)

// RunProjectBuild executes explicit host build arguments against the frozen local daemon.
// The ordinary host registry configuration remains available, but changing Docker contexts
// or selecting a remote Buildx builder cannot redirect reviewed repository instructions.
func (r Runtime) RunProjectBuild(ctx context.Context, lease *os.File, stdout, stderr io.Writer, args ...string) (int, error) {
	if len(args) == 0 || args[0] != "build" {
		return -1, errors.New("invalid project build command")
	}
	if r.composeEndpoint == "" || r.composeDaemon == "" {
		return -1, errors.New("project build requires a frozen Docker daemon")
	}
	docker, err := InspectDocker(ctx, r)
	if err != nil {
		return -1, err
	}
	defer docker.Close()
	argv := append([]string{"--config", docker.pluginConfig, "--host", docker.endpoint, "build", "--builder", "default"}, args[1:]...)
	cmd := exec.Command(docker.binary, argv...)
	cmd.Env, cmd.Stdout, cmd.Stderr = docker.env, stdout, stderr
	// A killed host must not let housekeeping remove a context the surviving client is reading.
	if lease != nil {
		cmd.ExtraFiles = []*os.File{lease}
	}
	code, err := runInterruptibleCommand(ctx, cmd)
	if err != nil || code != 0 {
		return code, err
	}
	return code, docker.Verify(ctx)
}
