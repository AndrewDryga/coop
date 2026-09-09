package box

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"reflect"
	hostruntime "runtime"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/runtime"
)

func TestNetworkConstructionRequiresHostCapabilitiesBeforeBuilding(t *testing.T) {
	for _, input := range []struct {
		ctx    context.Context
		docker *runtime.Docker
	}{
		{},
		{ctx: context.Background()},
		{docker: &runtime.Docker{}},
	} {
		if got, err := BuildNetworkCandidate(input.ctx, input.docker, nil, nil); err == nil || got.ClientImage != "" {
			t.Fatal("missing host capability reached construction", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := BuildNetworkCandidate(ctx, &runtime.Docker{}, nil, nil); !errors.Is(err, context.Canceled) || got.ClientImage != "" {
		t.Fatal("canceled operation reached construction", err)
	}
}

func TestNetworkRuntimeBindingPreservesAllObservedFields(t *testing.T) {
	for arch, want := range map[string]string{"aarch64": "arm64", "arm64": "arm64", "x86_64": "amd64", "amd64": "amd64", "other": "other"} {
		info := runtime.DockerInfo{ID: "daemon", OSType: "linux", Architecture: arch, ServerVersion: "29.4.0", KernelVersion: "7.0.14", SecurityOptions: []string{"name=cgroupns"}}
		got := networkRuntimeBinding(info, "unix:///fixture.sock")
		if got.HostFamily != hostruntime.GOOS || got.Endpoint != "unix:///fixture.sock" || got.DaemonID != info.ID || got.OS != info.OSType || got.Architecture != want || got.ServerVersion != info.ServerVersion || got.KernelVersion != info.KernelVersion || !reflect.DeepEqual(got.SecurityOptions, info.SecurityOptions) {
			t.Fatal("runtime binding lost observation", arch, got)
		}
		info.SecurityOptions[0] = "changed"
		if got.SecurityOptions[0] != "name=cgroupns" {
			t.Fatal("runtime binding shares mutable observation")
		}
	}
}

func TestLockedImageBuildContextIsPinnedDeterministicAndEmbeddedOnly(t *testing.T) {
	p := agents.ClientPlatform{OS: "linux", Architecture: "arm64", Libc: "glibc"}
	spec, data, closure, err := lockedImageDefinition(p)
	if err != nil {
		t.Fatal(err)
	}
	again, againData, _, err := lockedImageDefinition(p)
	if err != nil || !reflect.DeepEqual(spec, again) || !bytes.Equal(data, againData) {
		t.Fatal("nondeterministic definition", err)
	}
	if spec.Args["NODE_IMAGE"] != pinnedNodeImage || spec.Args["GO_IMAGE"] != pinnedGoImage || len(spec.Args) != 2 || spec.Labels["coop.clients.closure"] != closure.Digest {
		t.Fatal("mutable base or lost identity", spec)
	}
	files := make(map[string][]byte)
	r := tar.NewReader(bytes.NewReader(data))
	for {
		h, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag != tar.TypeReg || h.Mode != 0644 || h.Uid != 0 || h.Gid != 0 {
			t.Fatal("unexpected context entry", h)
		}
		files[h.Name], err = io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(files) != 8 || len(files["global.npmrc"]) != 0 || !bytes.Equal(files["package-lock.json"], closure.Files["package-lock.json"]) {
		t.Fatal("context omitted or added inputs")
	}
	df := string(files["Dockerfile"])
	for _, want := range []string{"npm ci --prefix /opt/coop/clients --ignore-scripts --include=optional --omit=dev", "--userconfig=/dev/null --globalconfig=/opt/coop/clients/global.npmrc", "/node_modules/playwright/cli.js install-deps chromium", "chmod -R a-w /opt/coop/clients", "USER node", "COOP_SUPERVISE_DESCENDANTS", "terminate_jobs"} {
		if !strings.Contains(df, want) {
			t.Fatal("missing locked-image requirement", want)
		}
	}
	for _, bad := range []string{"AGENT_PACKAGES", "npm install -g", "npx -y", "asdf plugin add", "asdf install >", "if ! node --version", "COOP_NO_ASDF", "/home/node/.asdf/shims", "%!"} {
		if strings.Contains(df, bad) {
			t.Fatal("mutable launcher/provisioning in locked image", bad)
		}
	}
	// The supervisor after optional provisioning must be byte-for-byte shared.
	start := "# Sidecar forwarders:"
	end := "\nENTRY\n"
	supervisor := func(s string) string {
		_, body, ok := strings.Cut(s, start)
		if !ok {
			t.Fatal("missing supervisor")
		}
		body, _, ok = strings.Cut(body, end)
		if !ok {
			t.Fatal("missing entrypoint end")
		}
		return body
	}
	if supervisor(df) != supervisor(BaseDockerfile()) {
		t.Fatal("locked image forked process supervision")
	}
	for _, client := range closure.Clients {
		if !strings.Contains(df, "test -x "+client.Exec[0]) || !bytes.Contains(files["launchers/"+client.Binary], []byte("exec '"+client.Exec[0]+"'")) {
			t.Fatal("wrong installed entrypoint", client)
		}
		for _, executable := range client.RequiredExecutables {
			if !strings.Contains(df, "test -x "+executable.Path) {
				t.Fatal("optional native executable not asserted", executable)
			}
		}
	}
}

func TestBaseDockerfilePreservesShellLongestSuffixRemoval(t *testing.T) {
	df := BaseDockerfile()
	for _, expression := range []string{"${forward%%:*}", "${rest%%:*}", "${record%%:*}"} {
		if !strings.Contains(df, expression) {
			t.Errorf("Go formatting damaged shell expansion %s", expression)
		}
	}
	for _, line := range strings.Split(df, "\n") {
		if !strings.Contains(line, "hp=${forward") {
			continue
		}
		cmd := exec.Command("sh", "-c", "oldifs=$IFS; forward=43123:postgres:5432\n"+line+"\nprintf '%s|%s|%s' \"$hp\" \"$svc\" \"$sp\"")
		out, err := cmd.CombinedOutput()
		if err != nil || string(out) != "43123|postgres|5432" {
			t.Fatalf("rendered forwarding parse: %q, %v", out, err)
		}
		return
	}
	t.Fatal("forwarder parser missing")
}
