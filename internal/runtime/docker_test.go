package runtime

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"text/template"
	"time"

	"github.com/AndrewDryga/coop/internal/processidentity"
)

func TestDockerImageTemplateAcceptsOmittedLabels(t *testing.T) {
	format, err := template.New("docker-inspect").Option("missingkey=error").Funcs(template.FuncMap{
		"json": func(value any) string { data, _ := json.Marshal(value); return string(data) },
	}).Parse(dockerImageFormat)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := format.Execute(&output, map[string]any{"Id": "sha256:" + strings.Repeat("a", 64), "Config": map[string]any{}}); err != nil {
		t.Fatal("unlabelled image inspection failed", err)
	}
	if !strings.Contains(output.String(), `"Labels":null`) {
		t.Fatal("absent labels changed meaning", output.String())
	}
}

type dockerFixture struct {
	Mode, ID, Kernel, Context string
	Container                 *DockerContainer
	Volume                    *DockerVolume
	Mutations                 int
	ClientPID                 int
	ClientToken               string
	SharedVolume              *volumeDefinition
	AfterVolumeID             string
	Layers                    []string
	Copy                      string
	CopyBody                  string
}

func fixtureDocker(t *testing.T, value dockerFixture) (Runtime, string) {
	t.Helper()
	root := t.TempDir()
	file := filepath.Join(root, "fixture.json")
	writeDockerFixture(t, file, value)
	t.Setenv("COOP_DOCKER_FIXTURE", file)
	t.Setenv("COOP_DOCKER_TEST_BINARY", os.Args[0])
	// Each fixture CLI is this race-instrumented test executable. Remove only
	// the race runtime's one-second exit sleep; otherwise two completed fake
	// commands consume the entire real startup observation deadline.
	t.Setenv("GORACE", os.Getenv("GORACE")+" atexit_sleep_ms=0")
	for _, key := range []string{"DOCKER_CONTEXT", "DOCKER_HOST", "DOCKER_API_VERSION", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH"} {
		t.Setenv(key, "")
	}
	binary := filepath.Join(root, "docker")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexec \"$COOP_DOCKER_TEST_BINARY\" -test.run=^TestDockerFixtureProcess$ -- \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return Runtime{Name: binary}, file
}

func writeDockerFixture(t *testing.T, file string, value dockerFixture) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func dockerFixtureContainer() *DockerContainer {
	return &DockerContainer{ID: strings.Repeat("a", 64), Name: "/coop-test", Image: "sha256:" + strings.Repeat("b", 64),
		User: "1000:1000", Labels: map[string]string{"coop.network.run": "fixture"}, CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges"},
		RestartPolicy: "no", State: DockerContainerState{Status: "created"}}
}

func dockerFixtureRef() DockerRef {
	return DockerRef{Name: "coop-test", ID: strings.Repeat("a", 64), Labels: map[string]string{"coop.network.run": "fixture"}}
}

// Separate processes exercise the actual exec/copy and attached-start paths.
// The fixture owns only its test-local JSON file; it never contacts a daemon.
func TestDockerFixtureProcess(t *testing.T) {
	file := os.Getenv("COOP_DOCKER_FIXTURE")
	if file == "" {
		return
	}
	index := slices.Index(os.Args, "--")
	if index < 0 {
		os.Exit(91)
	}
	args := os.Args[index+1:]
	data, err := os.ReadFile(file)
	var fixture dockerFixture
	if err != nil || json.Unmarshal(data, &fixture) != nil {
		os.Exit(92)
	}
	if len(args) >= 2 && args[0] == "--config" {
		entries, err := os.ReadDir(args[1])
		if err != nil || len(entries) != 0 {
			os.Exit(102)
		}
		args = args[2:]
	}
	bound := len(args) >= 2 && args[0] == "--host"
	if bound {
		if args[1] != "unix:///fixture.sock" {
			os.Exit(93)
		}
		for _, key := range []string{"DOCKER_CONTEXT", "DOCKER_HOST", "DOCKER_API_VERSION", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH"} {
			if _, exists := os.LookupEnv(key); exists {
				os.Exit(94)
			}
		}
		args = args[2:]
	}
	emit := func(value any) { _ = json.NewEncoder(os.Stdout).Encode(value) }
	store := func() {
		data, _ := json.Marshal(fixture)
		tmp := fmt.Sprintf("%s.%d", file, os.Getpid())
		if os.WriteFile(tmp, data, 0o600) != nil || os.Rename(tmp, file) != nil {
			os.Exit(95)
		}
	}
	if len(args) == 0 {
		os.Exit(96)
	}
	switch args[0] {
	case "run":
		if !bound || len(args) != 2 || args[1] != "--ordinary-binding-test" {
			os.Exit(106)
		}
		fmt.Println("ordinary workload")
	case "ps":
		if !bound {
			os.Exit(106)
		}
	case "context":
		if args[1] == "show" {
			fmt.Println("chosen")
		} else {
			if fixture.Context != "" && args[len(args)-1] != fixture.Context {
				os.Exit(97)
			}
			emit(map[string]any{"Host": "unix:///fixture.sock", "SkipTLSVerify": false})
		}
	case "info":
		if (fixture.Mode == "transient-start" || fixture.Mode == "transient-fast-exit") && fixture.ClientPID != 0 {
			marker, err := os.OpenFile(file+".probe-failed", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err == nil {
				_ = marker.Close()
				os.Exit(1)
			}
		}
		id, kernel := fixture.ID, fixture.Kernel
		if id == "" {
			id = "fixture-daemon"
		}
		if kernel == "" {
			kernel = "fixture-kernel"
		}
		info := DockerInfo{ID: id, OSType: "linux", Architecture: "amd64", ServerVersion: "29.4.0", KernelVersion: kernel}
		if fixture.Mode == "rootless" {
			info.SecurityOptions = []string{"name=rootless"}
		}
		emit(info)
	case "container":
		switch args[1] {
		case "create":
			if fixture.Mode == "create-fail" {
				os.Exit(1)
			}
			fixture.Container = dockerFixtureContainer()
			fixture.Mutations++
			if fixture.Mode == "create-post-fail" {
				fixture.Mode = "inspect-fail"
			}
			store()
			fmt.Println(fixture.Container.ID)
		case "inspect":
			if fixture.Mode == "overflow" {
				fmt.Print(strings.Repeat("x", 2<<20))
				break
			}
			if fixture.Container == nil || fixture.Mode == "inspect-fail" || fixture.Mode == "list-fail" {
				os.Exit(1)
			}
			emit(fixture.Container)
		case "ls":
			if fixture.Mode == "list-fail" {
				os.Exit(1)
			}
			if fixture.Container != nil {
				emit(map[string]string{"name": strings.TrimPrefix(fixture.Container.Name, "/"), "id": fixture.Container.ID})
			}
		case "start":
			if fixture.Container == nil {
				os.Exit(1)
			}
			if fixture.Mode == "no-start" {
				os.Exit(1)
			}
			if fixture.Mode == "pgid" {
				// The client reports the process group it was started in: coop's own when it
				// drives the terminal, a group of its own otherwise.
				fmt.Fprintf(os.Stdout, "PGID=%d\n", syscall.Getpgrp())
			}
			if fixture.Mode == "late-start" {
				// The daemon accepted the attach but has not started the workload
				// yet: every inspection in this window reads the created state the
				// test wrote, with no StartedAt.
				time.Sleep(300 * time.Millisecond)
			}
			fixture.Container.State = DockerContainerState{Status: "exited", StartedAt: time.Now().UTC(), FinishedAt: time.Now().UTC(), ExitCode: 7}
			if fixture.Mode == "detach" || fixture.Mode == "hold" {
				fixture.Container.State.Status, fixture.Container.State.Running = "running", true
			}
			fixture.Mutations++
			fixture.ClientPID = os.Getpid()
			fixture.ClientToken = processidentity.StartToken(os.Getpid())
			store()
			fmt.Fprint(os.Stdout, "first provider output\n")
			if fixture.Mode == "hold" {
				time.Sleep(time.Minute) // cancellation must reap this exact client group
			}
			if fixture.Mode == "queued-cancel" || fixture.Mode == "transient-start" {
				deadline := time.Now().Add(30 * time.Second)
				for {
					if _, err := os.Stat(file + ".release"); err == nil {
						break
					}
					if time.Now().After(deadline) {
						os.Exit(103)
					}
					time.Sleep(5 * time.Millisecond)
				}
			}
			os.Exit(0) // deliberately differs from the workload's daemon exit code
		case "cp":
			if len(args) != 4 || args[3] != "-" || !strings.HasPrefix(args[2], strings.Repeat("a", 64)+":/") {
				os.Exit(107)
			}
			writeCopyArchive(fixture)
		case "rm":
			if args[len(args)-1] != strings.Repeat("a", 64) {
				os.Exit(98)
			}
			fixture.Container = nil
			fixture.Mutations++
			store()
		default:
			os.Exit(99)
		}
	case "image":
		if !bound || args[1] != "inspect" {
			os.Exit(108)
		}
		emit(map[string]any{"ID": "sha256:" + strings.Repeat("d", 64), "Labels": map[string]string{}, "Layers": fixture.Layers})
	case "volume":
		if fixture.Mode == "shared-volume" {
			if !bound {
				os.Exit(104)
			}
			switch args[1] {
			case "ls":
				if fixture.SharedVolume != nil {
					fmt.Println(fixture.SharedVolume.Name)
				}
			case "inspect":
				if fixture.SharedVolume == nil {
					os.Exit(1)
				}
				emit(fixture.SharedVolume)
				if fixture.AfterVolumeID != "" {
					fixture.ID = fixture.AfterVolumeID
					store()
				}
			default:
				os.Exit(105)
			}
			os.Exit(0)
		}
		switch args[1] {
		case "create":
			if fixture.Mode == "create-fail" {
				os.Exit(1)
			}
			fixture.Volume = &DockerVolume{Name: "coop-test", Driver: "local", Scope: "local", CreatedAt: "fixture-time", Labels: dockerFixtureRef().Labels}
			fixture.Mutations++
			store()
			fmt.Println(fixture.Volume.Name)
		case "inspect":
			if fixture.Volume == nil {
				os.Exit(1)
			}
			emit(fixture.Volume)
		case "ls":
			if fixture.Volume != nil {
				emit(fixture.Volume.Name)
			}
		case "rm":
			fixture.Volume = nil
			fixture.Mutations++
			store()
		default:
			os.Exit(100)
		}
	default:
		os.Exit(101)
	}
	os.Exit(0)
}

func TestDockerBindingFreezesEndpointEnvironmentAndFencesDaemon(t *testing.T) {
	rt, file := fixtureDocker(t, dockerFixture{Context: "explicit"})
	t.Setenv("DOCKER_CONTEXT", "explicit")
	t.Setenv("DOCKER_HOST", "tcp://unselected.invalid:2375")
	t.Setenv("DOCKER_TLS_VERIFY", "1")
	t.Setenv("DOCKER_API_VERSION", "invalid")
	d, err := BindDocker(context.Background(), rt, "", "")
	if err != nil || d.Endpoint() != "unix:///fixture.sock" {
		t.Fatal("explicit context was not captured", err)
	}
	defer d.Close()
	t.Setenv("DOCKER_HOST", "tcp://later.invalid:2375")
	if err := d.Verify(context.Background()); err != nil {
		t.Fatal("ambient mutation changed bound endpoint", err)
	}
	writeDockerFixture(t, file, dockerFixture{ID: "another-daemon"})
	if err := d.Verify(context.Background()); err == nil {
		t.Fatal("replacement daemon retained custody")
	}
}

func TestDockerSharedVolumeInventoryUsesBoundEndpointAndNeverCreates(t *testing.T) {
	definition := &volumeDefinition{Name: "coop-cache", Driver: "local", Scope: "local", Mountpoint: t.TempDir()}
	fixture := dockerFixture{Mode: "shared-volume", SharedVolume: definition}
	rt, file := fixtureDocker(t, fixture)
	docker, err := BindDocker(context.Background(), rt, "unix:///fixture.sock", "")
	if err != nil {
		t.Fatal(err)
	}
	defer docker.Close()
	t.Setenv("DOCKER_HOST", "unix:///other.sock")
	t.Setenv("DOCKER_CONTEXT", "other")
	if _, err := rt.ExistingNamedVolumeExposure(context.Background(), []string{"coop-cache"}); err == nil {
		t.Fatal("fixture did not distinguish ambient from bound inventory")
	}
	exposure, err := docker.ExistingNamedVolumeExposure(context.Background(), []string{"coop-cache", "coop-asdf"})
	if err != nil || !slices.Equal(exposure.Sources, []string{definition.Mountpoint}) {
		t.Fatal("bound inventory followed ambient context or lost its volume", exposure, err)
	}
	fixture.SharedVolume = nil
	writeDockerFixture(t, file, fixture)
	exposure, err = docker.ExistingNamedVolumeExposure(context.Background(), []string{"coop-cache"})
	if err != nil || len(exposure.Sources) != 0 {
		t.Fatal("confirmed absence attempted volume creation", exposure, err)
	}
	fixture.SharedVolume, fixture.AfterVolumeID = definition, "replacement-during-inventory"
	writeDockerFixture(t, file, fixture)
	if _, err := docker.ExistingNamedVolumeExposure(context.Background(), []string{"coop-cache"}); err == nil {
		t.Fatal("inventory returned paths after its daemon changed")
	}
	fixture.AfterVolumeID = ""
	fixture.ID = "replacement-daemon"
	writeDockerFixture(t, file, fixture)
	if _, err := docker.ExistingNamedVolumeExposure(context.Background(), []string{"coop-cache"}); err == nil {
		t.Fatal("inventory accepted replacement daemon")
	}
}

func TestDockerEndpointAndCreationGrammarRefuseEscapes(t *testing.T) {
	for _, endpoint := range []string{"", "tcp://localhost:2375", "ssh://host", "unix://host/path", "unix:relative", "unix:///", "unix:///a/../socket", "unix:///socket?", "unix:///socket#x"} {
		if validDockerEndpoint(endpoint) {
			t.Fatalf("unsafe endpoint accepted: %q", endpoint)
		}
	}
	for _, options := range [][]string{{"--privileged"}, {"--name", "other"}, {"--rm"}, {"--restart", "always"}, {"--pid", "host"}, {"--"}, {"--mount"}, {"alpine"}} {
		if validDockerCreateOptions(options) {
			t.Fatalf("unexpected creation grammar: %q", options)
		}
	}
	if !validDockerCreateOptions([]string{"--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--network", "container:" + strings.Repeat("a", 64), "-i"}) {
		t.Fatal("valid composed options rejected")
	}
}

func TestDockerRebindUsesCapturedBinaryAndEndpoint(t *testing.T) {
	rt, _ := fixtureDocker(t, dockerFixture{})
	d, err := BindDocker(context.Background(), rt, "unix:///fixture.sock", "")
	if err != nil {
		t.Fatal(err)
	}
	binary, endpoint := d.Binary(), d.Endpoint()
	if !filepath.IsAbs(binary) || binary != rt.Name {
		t.Fatal("binding did not retain exact executable", binary)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	t.Setenv("DOCKER_CONTEXT", "must-not-be-discovered")
	t.Setenv("DOCKER_HOST", "tcp://must-not-be-contacted.invalid:2375")
	replayed, err := BindDocker(context.Background(), Runtime{Name: binary}, endpoint, "")
	if err != nil {
		t.Fatal("replay depended on ambient executable or context", err)
	}
	defer replayed.Close()
	if !replayed.launchAllowed || replayed.Endpoint() != endpoint {
		t.Fatal("exact admitted replay lost launch capability")
	}
}

func TestDockerCopyBoundCannotUsePromotedReadFrom(t *testing.T) {
	output := dockerBoundedOutput{limit: 1024}
	reader := struct{ io.Reader }{strings.NewReader(strings.Repeat("x", 2<<20))}
	if n, err := io.Copy(&output, reader); err != nil || n != 2<<20 || output.Len() != 1024 || !output.overflow {
		t.Fatalf("bounded copy bypassed: n=%d retained=%d overflow=%v err=%v", n, output.Len(), output.overflow, err)
	}
}

func TestDockerInspectionNeverConfusesFailureWithAbsence(t *testing.T) {
	for _, mode := range []string{"absent", "inspect-fail", "list-fail", "overflow", "foreign"} {
		t.Run(mode, func(t *testing.T) {
			fixture := dockerFixture{Mode: mode, Container: dockerFixtureContainer()}
			if mode == "absent" {
				fixture.Container = nil
			}
			if mode == "foreign" {
				fixture.Container.Labels["coop.network.run"] = "foreign"
			}
			rt, _ := fixtureDocker(t, fixture)
			d, err := BindDocker(context.Background(), rt, "unix:///fixture.sock", "")
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			_, present, err := d.InspectContainer(context.Background(), dockerFixtureRef())
			if present || (err == nil) != (mode == "absent") {
				t.Fatalf("inspection mode=%s present=%v err=%v", mode, present, err)
			}
		})
	}
}

func TestDockerAttachedStartCapturesFirstOutputAndDaemonOutcome(t *testing.T) {
	for _, mode := range []string{"normal", "detach"} {
		t.Run(mode, func(t *testing.T) {
			rt, _ := fixtureDocker(t, dockerFixture{Mode: mode, Container: dockerFixtureContainer()})
			d, err := BindDocker(context.Background(), rt, "unix:///fixture.sock", "")
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			var output bytes.Buffer
			started := 0
			code, err := d.StartAttached(context.Background(), dockerFixtureRef(), nil, &output, io.Discard, func() error { started++; return nil })
			if output.String() != "first provider output\n" || started != 1 {
				t.Fatalf("lost startup/output: started=%d output=%q err=%v", started, output.String(), err)
			}
			if mode == "normal" && (err != nil || code != 7) || mode == "detach" && (err == nil || code != -1) {
				t.Fatalf("client exit substituted for workload: mode=%s code=%d err=%v", mode, code, err)
			}
			if err := d.StartContainer(context.Background(), dockerFixtureRef()); err == nil {
				t.Fatal("same container restarted")
			}
			if err := d.RemoveContainer(context.Background(), dockerFixtureRef()); err != nil {
				t.Fatal("exact cleanup failed", err)
			}
		})
	}
}

func TestDockerRecoveryCanCleanAfterQualificationDriftButCannotLaunch(t *testing.T) {
	rt, file := fixtureDocker(t, dockerFixture{Container: dockerFixtureContainer()})
	d, err := BindDocker(context.Background(), rt, "unix:///fixture.sock", "fixture-daemon")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.StartContainer(context.Background(), dockerFixtureRef()); err == nil {
		t.Fatal("recovery binding gained start authority")
	}
	writeDockerFixture(t, file, dockerFixture{Kernel: "upgraded-kernel", Container: dockerFixtureContainer()})
	if err := d.RemoveContainer(context.Background(), dockerFixtureRef()); err != nil {
		t.Fatal("qualification change prevented exact cleanup", err)
	}
}

func TestDockerCancelledAttachmentDoesNotAssertWorkloadDeath(t *testing.T) {
	rt, _ := fixtureDocker(t, dockerFixture{Mode: "hold", Container: dockerFixtureContainer()})
	d, err := BindDocker(context.Background(), rt, "unix:///fixture.sock", "")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	code, err := d.StartAttached(ctx, dockerFixtureRef(), nil, io.Discard, io.Discard, func() error { cancel(); return nil })
	if code != -1 || !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation became a completed workload", code, err)
	}
	value, present, err := d.InspectContainer(context.Background(), dockerFixtureRef())
	if err != nil || !present || !value.State.Running {
		t.Fatal("fixture did not preserve daemon-owned workload", err)
	}
	if err := d.RemoveContainer(context.Background(), dockerFixtureRef()); err != nil {
		t.Fatal("caller could not complete exact cleanup", err)
	}
}

func TestDockerCancellationAfterQueuedClientSuccessHasUnknownExit(t *testing.T) {
	rt, file := fixtureDocker(t, dockerFixture{Mode: "queued-cancel", Container: dockerFixtureContainer()})
	d, err := BindDocker(context.Background(), rt, "unix:///fixture.sock", "")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	code, err := d.StartAttached(ctx, dockerFixtureRef(), nil, io.Discard, io.Discard, func() error {
		data, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		var fixture dockerFixture
		if err := json.Unmarshal(data, &fixture); err != nil {
			return err
		}
		if err := os.WriteFile(file+".release", nil, 0600); err != nil {
			return err
		}
		deadline := time.Now().Add(30 * time.Second)
		for processidentity.Inspect(fixture.ClientPID, fixture.ClientToken) != processidentity.Gone {
			if time.Now().After(deadline) {
				return errors.New("fixture client did not exit")
			}
			time.Sleep(5 * time.Millisecond)
		}
		// The CLI completion is now queued while this callback holds the
		// supervisor. Cancellation must not turn its exit0 into provider success.
		cancel()
		return nil
	})
	if code != -1 || !errors.Is(err, context.Canceled) {
		t.Fatal("queued Docker client success escaped cancellation", code, err)
	}
}

func TestInspectDockerHasNoLaunchAuthority(t *testing.T) {
	for _, mode := range []string{"ordinary", "rootless"} {
		t.Run(mode, func(t *testing.T) {
			rt, file := fixtureDocker(t, dockerFixture{Mode: mode})
			d, err := InspectDocker(t.Context(), rt)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			if d.Endpoint() != "unix:///fixture.sock" || d.Info().ID != "fixture-daemon" || d.launchAllowed {
				t.Fatal("inspection binding changed authority")
			}
			if err := d.Verify(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := d.VerifyLaunch(t.Context()); err == nil {
				t.Fatal("inspection authorized a launch")
			}
			data, err := os.ReadFile(file)
			var fixture dockerFixture
			if err != nil || json.Unmarshal(data, &fixture) != nil || fixture.Mutations != 0 {
				t.Fatal("inspection mutated runtime", err)
			}
			launch, err := BindDocker(t.Context(), rt, d.Endpoint(), "")
			if launch != nil {
				defer launch.Close()
			}
			if (err == nil) != (mode == "ordinary") {
				t.Fatal("inspection widened filtered runtime qualification", err)
			}
		})
	}
}

func TestDockerClientFailureWithoutStartIsNotProviderExit(t *testing.T) {
	rt, _ := fixtureDocker(t, dockerFixture{Mode: "no-start", Container: dockerFixtureContainer()})
	d, err := BindDocker(context.Background(), rt, "unix:///fixture.sock", "")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	started := false
	code, err := d.StartAttached(context.Background(), dockerFixtureRef(), nil, io.Discard, io.Discard, func() error { started = true; return nil })
	if code != -1 || err == nil || started {
		t.Fatal("failed client invented startup/provider exit", code, err)
	}
}

func TestDockerVolumeOwnershipAndPrivateConfigLifetime(t *testing.T) {
	for _, kind := range []string{"owned", "foreign", "options"} {
		t.Run(kind, func(t *testing.T) {
			volume := &DockerVolume{Name: "coop-test", Driver: "local", Scope: "local", CreatedAt: "fixture-time", Labels: map[string]string{"coop.network.run": "fixture"}}
			if kind == "foreign" {
				volume.Labels["coop.network.run"] = "another-run"
			}
			if kind == "options" {
				volume.Options = map[string]string{"type": "nfs"}
			}
			rt, file := fixtureDocker(t, dockerFixture{Volume: volume})
			t.Setenv("DOCKER_CONFIG", "/a/private/operator/config")
			d, err := BindDocker(context.Background(), rt, "unix:///fixture.sock", "")
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			ref := dockerFixtureRef()
			ref.ID = ref.Name
			err = d.RemoveVolume(context.Background(), ref)
			if (err == nil) != (kind == "owned") {
				t.Fatal("volume ownership not enforced", kind, err)
			}
			data, err := os.ReadFile(file)
			var fixture dockerFixture
			if err != nil || json.Unmarshal(data, &fixture) != nil || kind != "owned" && fixture.Mutations != 0 {
				t.Fatal("foreign volume was mutated")
			}
			config := d.clientConfig
			if err := d.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(config); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("private client config survived close")
			}
			if err := d.Verify(context.Background()); err == nil {
				t.Fatal("closed Docker binding remained usable")
			}
		})
	}
}

// A daemon that takes its time starting the workload is slow, not broken. An
// ordinary `docker run` waits on exactly that with no bound, so a filtered launch
// must not turn it into a failed run — it may only stop being silent about it.
func TestDockerLateWorkloadStartIsReportedNotFailed(t *testing.T) {
	previous := slowStartupAfter
	slowStartupAfter = 20 * time.Millisecond
	t.Cleanup(func() { slowStartupAfter = previous })
	rt, _ := fixtureDocker(t, dockerFixture{Mode: "late-start", Container: dockerFixtureContainer()})
	d, err := BindDocker(context.Background(), rt, "unix:///fixture.sock", "")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	notices := 0
	d.OnSlowStart = func(time.Duration) { notices++ }
	started := 0
	code, err := d.StartAttached(context.Background(), dockerFixtureRef(), nil, io.Discard, io.Discard, func() error { started++; return nil })
	if err != nil || code != 7 || started != 1 {
		t.Fatal("a late start was not carried to its real workload outcome", code, started, err)
	}
	if notices != 1 {
		t.Fatal("the operator was told about the wait either never or more than once", notices)
	}
}

// writeCopyArchive is the tar stream `docker cp <container>:<path> -` produces.
// Each mode is one shape a replaced pinned client would arrive in.
func writeCopyArchive(fixture dockerFixture) {
	w := tar.NewWriter(os.Stdout)
	body := []byte(fixture.CopyBody)
	switch fixture.Copy {
	case "missing":
		os.Exit(1) // the daemon refuses a path that is not there
	case "symlink":
		_ = w.WriteHeader(&tar.Header{Name: "claude", Typeflag: tar.TypeSymlink, Linkname: "/tmp/theirs", Mode: 0o777})
	case "directory":
		_ = w.WriteHeader(&tar.Header{Name: "clients/", Typeflag: tar.TypeDir, Mode: 0o755})
	case "two-entries":
		_ = w.WriteHeader(&tar.Header{Name: "claude", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(body))})
		_, _ = w.Write(body)
		_ = w.WriteHeader(&tar.Header{Name: "claude.bak", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(body))})
		_, _ = w.Write(body)
	case "short":
		_ = w.WriteHeader(&tar.Header{Name: "claude", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(body)) + 1})
		_, _ = w.Write(body)
	default:
		_ = w.WriteHeader(&tar.Header{Name: "claude", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(body))})
		_, _ = w.Write(body)
	}
	_ = w.Close()
}

// The layer chain is what proves one image was built on another, so an
// observation that cannot carry that meaning is refused rather than trusted.
func TestDockerImageLayersRefusesAnUnprovableChain(t *testing.T) {
	good := []string{"sha256:" + strings.Repeat("1", 64), "sha256:" + strings.Repeat("2", 64)}
	var beyond []string
	for i := range maxDockerImageLayers + 1 {
		beyond = append(beyond, fmt.Sprintf("sha256:%064x", i))
	}
	for name, layers := range map[string][]string{
		"none":       nil,
		"not hex":    {"sha256:" + strings.Repeat("z", 64)},
		"unprefixed": {strings.Repeat("1", 64)},
		"truncated":  {"sha256:" + strings.Repeat("1", 32)},
		"beyond max": beyond,
		"chain":      good,
	} {
		t.Run(name, func(t *testing.T) {
			rt, _ := fixtureDocker(t, dockerFixture{Layers: layers})
			d, err := BindDocker(context.Background(), rt, "unix:///fixture.sock", "")
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			id, got, err := d.ImageLayers(context.Background(), "coop-clients:pinned")
			if name != "chain" {
				if err == nil {
					t.Fatal("an unprovable chain was accepted", got)
				}
				return
			}
			if err != nil || id != "sha256:"+strings.Repeat("d", 64) || !slices.Equal(got, good) {
				t.Fatal("the chain this image reported was lost", id, got, err)
			}
		})
	}
	rt, _ := fixtureDocker(t, dockerFixture{Layers: good})
	d, err := BindDocker(context.Background(), rt, "unix:///fixture.sock", "")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, name := range []string{"", "-rm", strings.Repeat("x", 513)} {
		if _, _, err := d.ImageLayers(context.Background(), name); err == nil {
			t.Fatalf("an invalid reference was inspected: %q", name)
		}
	}
}

// One regular file, read out of a container that is never started. Anything else
// is how a replaced pinned client hides, so it is refused, not summarized.
func TestDockerFileDigestIdentifiesOneRegularFile(t *testing.T) {
	body := "#!/bin/sh\nexec claude\n"
	sum := sha256.Sum256([]byte(body))
	for name, test := range map[string]struct {
		mode, wantErr string
		running       bool
		limit         int64
	}{
		"regular":     {mode: "", limit: 1 << 20},
		"symlink":     {mode: "symlink", limit: 1 << 20, wantErr: "ordinary file"},
		"directory":   {mode: "directory", limit: 1 << 20, wantErr: "ordinary file"},
		"two entries": {mode: "two-entries", limit: 1 << 20, wantErr: "more than one entry"},
		"truncated":   {mode: "short", limit: 1 << 20, wantErr: "whole"},
		"absent":      {mode: "missing", limit: 1 << 20, wantErr: "no file was copied out"},
		"over limit":  {mode: "", limit: 4, wantErr: "readable size"},
		"no limit":    {mode: "", limit: 0, wantErr: "unavailable"},
		"beyond max":  {mode: "", limit: maxDockerFileEvidence + 1, wantErr: "unavailable"},
		"running":     {mode: "", limit: 1 << 20, running: true, wantErr: "unavailable"},
	} {
		t.Run(name, func(t *testing.T) {
			container := dockerFixtureContainer()
			if test.running {
				container.State = DockerContainerState{Status: "running", Running: true, StartedAt: time.Now()}
			}
			rt, _ := fixtureDocker(t, dockerFixture{Container: container, Copy: test.mode, CopyBody: body})
			d, err := BindDocker(context.Background(), rt, "unix:///fixture.sock", "")
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			file, err := d.FileDigest(context.Background(), dockerFixtureRef(), "/usr/local/bin/claude", test.limit)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("read %v, want a refusal naming %q: %v", file, test.wantErr, err)
				}
				return
			}
			if err != nil || file.SHA256 != hex.EncodeToString(sum[:]) || file.Size != int64(len(body)) || file.Mode != 0o755 {
				t.Fatal("one regular file was not identified", file, err)
			}
		})
	}
}

func TestDockerTreeDigestChangesWithTheArchivedTree(t *testing.T) {
	read := func(mode string, running bool, limit int64) (DockerTree, error) {
		t.Helper()
		container := dockerFixtureContainer()
		if running {
			container.State = DockerContainerState{Status: "running", Running: true, StartedAt: time.Now()}
		}
		rt, _ := fixtureDocker(t, dockerFixture{Container: container, Copy: mode, CopyBody: "module.exports = 1\n"})
		d, err := BindDocker(context.Background(), rt, "unix:///fixture.sock", "")
		if err != nil {
			return DockerTree{}, err
		}
		defer d.Close()
		return d.TreeDigest(context.Background(), dockerFixtureRef(), "/opt/coop/clients", limit)
	}
	base, err := read("", false, 1<<20)
	if err != nil || base.Size == 0 || len(base.SHA256) != 64 {
		t.Fatal("client tree was not digested", base, err)
	}
	for _, mode := range []string{"two-entries", "directory", "symlink"} {
		changed, err := read(mode, false, 1<<20)
		if err != nil || changed == base {
			t.Fatalf("%s tree was not distinguished: %v %v", mode, changed, err)
		}
	}
	for name, test := range map[string]struct {
		mode    string
		running bool
		limit   int64
	}{
		"missing":   {mode: "missing", limit: 1 << 20},
		"running":   {running: true, limit: 1 << 20},
		"too large": {limit: 32},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := read(test.mode, test.running, test.limit); err == nil {
				t.Fatal("unreadable tree was accepted")
			}
		})
	}
}

// A client attached to coop's terminal must run in coop's OWN process group: a background group is
// suspended by the kernel the moment it touches the terminal, which hangs an interactive box before
// the daemon is ever told to start it. Every other client keeps its own group, so cancelling one
// tears down the whole client tree.
func TestAttachedClientJoinsCoopsGroupOnlyWhenItDrivesTheTerminal(t *testing.T) {
	for _, terminal := range []bool{true, false} {
		name := "own-group"
		if terminal {
			name = "coop-group"
		}
		t.Run(name, func(t *testing.T) {
			previous := attachedToTerminal
			attachedToTerminal = func(...any) bool { return terminal }
			t.Cleanup(func() { attachedToTerminal = previous })
			rt, _ := fixtureDocker(t, dockerFixture{Mode: "pgid", Container: dockerFixtureContainer()})
			d, err := BindDocker(context.Background(), rt, "unix:///fixture.sock", "")
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			var out bytes.Buffer
			if _, err := d.StartAttached(context.Background(), dockerFixtureRef(), nil, &out, io.Discard, nil); err != nil {
				t.Fatal(err)
			}
			_, value, found := strings.Cut(strings.TrimSpace(out.String()), "PGID=")
			if !found {
				t.Fatal("the client did not report its process group", out.String())
			}
			group, err := strconv.Atoi(strings.Fields(value)[0])
			if err != nil {
				t.Fatal(err)
			}
			if shared := group == syscall.Getpgrp(); shared != terminal {
				t.Fatalf("client group %d against coop's %d: shares=%v, want %v", group, syscall.Getpgrp(), shared, terminal)
			}
		})
	}
}
