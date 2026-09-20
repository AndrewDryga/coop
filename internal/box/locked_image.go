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

// lockedClientRepository holds the qualified client sets, one tag per definition.
const lockedClientRepository = "coop-clients"

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
	return networkstate.RuntimeBinding{HostFamily: hostruntime.GOOS, Endpoint: endpoint, DaemonID: info.ID,
		OS: info.OSType, Architecture: runtime.Architecture(info.Architecture), ServerVersion: info.ServerVersion, KernelVersion: info.KernelVersion,
		SecurityOptions: append([]string{}, info.SecurityOptions...)}
}

// lockedPath is the filtered image's PATH: the launchers first, and no asdf shims at all.
const lockedPath = agents.LauncherDir + ":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

func lockedImageDefinition(platform agents.ClientPlatform) (runtime.DockerBuild, []byte, agents.ClientClosure, error) {
	closure, err := agents.LockedClientClosure(platform)
	if err != nil {
		return runtime.DockerBuild{}, nil, agents.ClientClosure{}, err
	}
	parts := lockedClientParts(closure)
	parts.loginPath = `printf 'export PATH="` + lockedPath + `"\n' > /etc/profile.d/coop-path.sh`
	parts.pathEnv = lockedPath
	closure.Files["Dockerfile"] = []byte(renderBaseDockerfile(parts))
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
	spec.Tag = lockedClientRepository + ":" + definition[:32]
	spec.Labels = map[string]string{"coop.clients.definition": definition, "coop.clients.closure": closure.Digest, "coop.clients.libc": platform.Libc}
	return spec, contextBytes.Bytes(), closure, nil
}

// lockedClientParts renders the client installation both Coop images share from one closure: npm
// ci of the embedded lock (lifecycle scripts off), each native artifact checked against its digest
// before it is unpacked or run, the launchers in agents.LauncherDir, and every adapter's update
// controls. The images differ only in PATH and the base's startup provisioning.
func lockedClientParts(closure agents.ClientClosure) baseImageParts {
	install := "/usr/local/bin/npm ci --prefix /opt/coop/clients --ignore-scripts --include=optional --omit=dev --no-audit --no-fund --registry=https://registry.npmjs.org --userconfig=/dev/null --globalconfig=/opt/coop/clients/global.npmrc --cache=/tmp/coop-client-npm-cache \\\n && rm -rf /tmp/coop-client-npm-cache"
	native := map[string]bool{}
	for _, client := range closure.Clients {
		artifact := client.NativeArtifact
		if artifact == nil || native[artifact.Destination] {
			continue
		}
		native[artifact.Destination] = true
		install += fmt.Sprintf(" \\\n && mkdir -p /opt/coop/clients/native \\\n && curl --fail --silent --show-error --proto '=https' --output /tmp/coop-client.gz '%s' \\\n && printf '%%s  %%s\\n' '%s' /tmp/coop-client.gz | sha256sum -c - \\\n && gzip -dc /tmp/coop-client.gz > '%s.tmp' \\\n && install -m 0755 '%s.tmp' '%s' \\\n && rm -f /tmp/coop-client.gz '%s.tmp'", artifact.URL, artifact.SHA256, artifact.Destination, artifact.Destination, artifact.Destination, artifact.Destination)
	}
	// npm ci still creates normal executable links while ignoring every lifecycle hook; none may be
	// on PATH before the absolute launchers are the only way in.
	var checks strings.Builder
	checks.WriteString("RUN")
	for i, client := range closure.Clients {
		if i > 0 {
			checks.WriteString(" &&")
		}
		fmt.Fprintf(&checks, " ! command -v %s", client.Binary)
	}
	checks.WriteString("\nCOPY launchers/ " + agents.LauncherDir + "/\nRUN chmod 0755 " + agents.LauncherDir)
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
	for _, name := range closure.FileNames() {
		if strings.HasPrefix(name, "system/") {
			checks.WriteString("COPY system/ /\n")
			break
		}
	}
	if len(closure.Env) > 0 {
		checks.WriteString("ENV " + strings.Join(closure.Env, " ") + "\n")
	}
	closure.Files["global.npmrc"] = []byte{}
	return baseImageParts{
		files:       "COPY package.json package-lock.json global.npmrc /opt/coop/clients/",
		install:     install,
		browserDeps: "/usr/local/bin/node /opt/coop/clients/node_modules/playwright/cli.js install-deps chromium",
		scripts:     checks.String(),
	}
}
