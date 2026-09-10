package box

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	hostruntime "runtime"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/gatewayimage"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// BuildNetworkCandidate is explicit host setup, never a fallback during launch.
// It returns the actual image pair construction produced; only the qualification
// that proves it is ever persisted, so a failed build grants no launch authority
// and never prunes shared Docker state to hide the failed operation. Building
// the client image records construction, never provider qualification:
// authentication, MCP and constrained-network behavior are proven separately
// against this exact ID and closure before a public filtered launch uses it.
// It deliberately accepts no Config, repository path, fresh/floating flag,
// package override or custom Dockerfile: the inputs are embedded.
func BuildNetworkCandidate(ctx context.Context, docker *runtime.Docker, stdout, stderr *os.File) (networkstate.CandidateSpec, error) {
	if ctx == nil || docker == nil {
		return networkstate.CandidateSpec{}, errors.New("network construction requires a bound Docker runtime")
	}
	if err := ctx.Err(); err != nil {
		return networkstate.CandidateSpec{}, err
	}
	binding := networkRuntimeBinding(docker.Info(), docker.Endpoint())
	platform := agents.ClientPlatform{OS: binding.OS, Architecture: binding.Architecture, Libc: "glibc"}
	spec, contextTar, closure, err := lockedImageDefinition(platform)
	if err != nil {
		return networkstate.CandidateSpec{}, err
	}
	client, err := docker.BuildImage(ctx, spec, contextTar, stdout, stderr)
	if err != nil {
		return networkstate.CandidateSpec{}, err
	}
	helper, err := gatewayimage.Build(ctx, docker, stdout, stderr)
	if err != nil {
		return networkstate.CandidateSpec{}, err
	}
	if err := docker.VerifyLaunch(ctx); err != nil {
		return networkstate.CandidateSpec{}, err
	}
	if err := ctx.Err(); err != nil {
		return networkstate.CandidateSpec{}, err
	}
	return networkstate.CandidateSpec{Runtime: binding, ClientImage: client.ID, GatewayImage: helper,
		ClientDefinition: spec.Labels["coop.clients.definition"], ClientClosure: closure.Digest, GatewaySource: gatewayimage.Fingerprint(),
		Libc: platform.Libc, NodeBase: pinnedNodeImage, GoBase: pinnedGoImage}, nil
}

func networkRuntimeBinding(info runtime.DockerInfo, endpoint string) networkstate.RuntimeBinding {
	arch := info.Architecture
	if arch == "aarch64" {
		arch = "arm64"
	}
	if arch == "x86_64" {
		arch = "amd64"
	}
	return networkstate.RuntimeBinding{HostFamily: hostruntime.GOOS, Endpoint: endpoint, DaemonID: info.ID,
		OS: info.OSType, Architecture: arch, ServerVersion: info.ServerVersion, KernelVersion: info.KernelVersion,
		SecurityOptions: append([]string{}, info.SecurityOptions...)}
}

func lockedImageDefinition(platform agents.ClientPlatform) (runtime.DockerBuild, []byte, agents.ClientClosure, error) {
	closure, err := agents.LockedClientClosure(platform)
	if err != nil {
		return runtime.DockerBuild{}, nil, agents.ClientClosure{}, err
	}
	// npm ci still creates normal executable links while ignoring every
	// lifecycle hook. Our absolute launchers replace only the four selected bins.
	var checks strings.Builder
	checks.WriteString("RUN")
	for i, client := range closure.Clients {
		if i > 0 {
			checks.WriteString(" &&")
		}
		fmt.Fprintf(&checks, " ! command -v %s", client.Binary)
	}
	checks.WriteString("\nCOPY launchers/ /usr/local/bin/\nRUN chmod 0755")
	for _, client := range closure.Clients {
		fmt.Fprintf(&checks, " %s", client.Launcher())
	}
	for _, client := range closure.Clients {
		fmt.Fprintf(&checks, " \\\n && test -f %s && test -x %s", client.Exec[0], client.Exec[0])
		for _, arg := range client.Exec[1:] {
			fmt.Fprintf(&checks, " \\\n && test -f %s && test -r %s", arg, arg)
		}
		for _, executable := range client.RequiredExecutables {
			fmt.Fprintf(&checks, " \\\n && test -f %s && test -x %s", executable.Path, executable.Path)
		}
	}
	checks.WriteString("\nRUN chmod -R a-w /opt/coop/clients\n")
	dockerfile := renderBaseDockerfile(baseImageParts{
		files:       "COPY package.json package-lock.json global.npmrc /opt/coop/clients/",
		install:     "/usr/local/bin/npm ci --prefix /opt/coop/clients --ignore-scripts --include=optional --omit=dev --no-audit --no-fund --registry=https://registry.npmjs.org --userconfig=/dev/null --globalconfig=/opt/coop/clients/global.npmrc --cache=/tmp/coop-client-npm-cache \\\n && rm -rf /tmp/coop-client-npm-cache",
		browserDeps: "/usr/local/bin/node /opt/coop/clients/node_modules/playwright/cli.js install-deps chromium",
		loginPath:   `printf 'export PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"\n' > /etc/profile.d/coop-path.sh`,
		pathEnv:     "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		scripts:     checks.String(),
	})
	closure.Files["Dockerfile"] = []byte(dockerfile)
	closure.Files["global.npmrc"] = []byte{}
	var contextBytes bytes.Buffer
	w := tar.NewWriter(&contextBytes)
	for _, name := range closure.FileNames() {
		data := closure.Files[name]
		if err := w.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(data))}); err != nil {
			return runtime.DockerBuild{}, nil, agents.ClientClosure{}, err
		}
		if _, err := w.Write(data); err != nil {
			return runtime.DockerBuild{}, nil, agents.ClientClosure{}, err
		}
	}
	if err := w.Close(); err != nil {
		return runtime.DockerBuild{}, nil, agents.ClientClosure{}, err
	}
	spec := runtime.DockerBuild{Platform: platform.OS + "/" + platform.Architecture, Args: map[string]string{"NODE_IMAGE": pinnedNodeImage, "GO_IMAGE": pinnedGoImage}}
	identity, err := json.Marshal(struct {
		Platform string
		Args     map[string]string
		Context  []byte
	}{spec.Platform, spec.Args, contextBytes.Bytes()})
	if err != nil {
		return runtime.DockerBuild{}, nil, agents.ClientClosure{}, err
	}
	digest := sha256.Sum256(identity)
	definition := hex.EncodeToString(digest[:])
	spec.Tag = "coop-clients:" + definition[:32]
	spec.Labels = map[string]string{"coop.clients.definition": definition, "coop.clients.closure": closure.Digest, "coop.clients.libc": platform.Libc}
	return spec, contextBytes.Bytes(), closure, nil
}
