package box

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

// Scratch gates have no generation marker: the trusted launch carries the parent fence.
func isolatedScratchServiceFixture(t *testing.T) (string, string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "global"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "system"))
	repo, git := gitrepo.New(t)
	if err := os.WriteFile(filepath.Join(repo, "source.txt"), []byte("parent"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "source.txt")
	git("commit", "-qm", "base")
	head, err := forkspace.ObserveGit(t.Context(), repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := os.MkdirTemp("", "coop-svc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(ws) })
	commit := strings.TrimSpace(string(head))
	if err := forkspace.GitClonePinnedContext(t.Context(), repo, ws, commit); err != nil {
		t.Fatal(err)
	}
	if err := forkspace.CheckoutIndependent(t.Context(), ws, commit, ""); err != nil {
		t.Fatal(err)
	}
	return repo, ws
}

func TestIsolatedServiceBindsRejectControlSourcesBeforeCompose(t *testing.T) {
	for _, kind := range []string{"socket", "shared inode", "npipe", "unknown type"} {
		t.Run(kind, func(t *testing.T) {
			repo, ws := isolatedScratchServiceFixture(t)
			recorder := filepath.Join(t.TempDir(), "runtime.log")
			rt := serviceReviewRuntime(t, recorder)
			source := filepath.Join(ws, "service-data")
			volume := "\"./service-data:/data:ro\""
			switch kind {
			case "socket":
				listener, err := net.Listen("unix", source)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = listener.Close() })
			case "shared inode":
				if err := os.Link(filepath.Join(repo, "source.txt"), source); err != nil {
					t.Fatal(err)
				}
			case "npipe":
				volume = "{type: npipe, source: control, target: /control}"
			case "unknown type":
				volume = "{type: future-ipc, source: control, target: /control}"
			}
			data := []byte("services:\n  db:\n    image: postgres:18\n    volumes: [" + volume + "]\n")
			file := filepath.Join(ws, "compose.yml")
			if err := os.WriteFile(file, data, 0o600); err != nil {
				t.Fatal(err)
			}
			// Generic containment alone used to accept these sources.
			if err := validateComposeData(data, file, ws, false); err != nil {
				t.Fatal(err)
			}
			ctx := context.WithValue(t.Context(), isolatedServiceParentKey{}, repo)
			_, cleanup, _, err := snapshotComposeArgsForStartPinned(ctx, rt, ws, file, "", data, true)
			if cleanup != nil {
				cleanup()
			}
			if err == nil {
				t.Fatal("isolated bind reached service pinning")
			}
			if calls, _ := os.ReadFile(recorder); len(calls) != 0 {
				t.Fatalf("runtime touched before source refusal: %s", calls)
			}
			if err := validateIsolatedServiceVolumes(t.Context(), rt, ws, file, "", data); err != nil {
				t.Fatalf("ordinary scratch behavior changed: %v", err)
			}
		})
	}
}

func TestFilteredScratchServiceBoundarySurvivesPreparationAndStart(t *testing.T) {
	repo, ws := isolatedScratchServiceFixture(t)
	file := filepath.Join(ws, "compose.yml")
	data := []byte("services:\n  db:\n    image: postgres:18\n    volumes: [\"data:/data:ro\"]\nvolumes:\n  data: {}\n")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	digests, err := composeServiceDigests(file, ws, false, []string{"db"})
	if err != nil {
		t.Fatal(err)
	}
	approval := &networkstate.Approval{Services: digests}
	recorder := filepath.Join(t.TempDir(), "runtime.log")
	rt := serviceReviewRuntime(t, recorder)
	docker := &filteredDaemonFixture{extraNetworks: []runtime.DockerNetwork{{Name: ComposeProject(ws) + "_filtered", Internal: true,
		Subnets: []netip.Prefix{netip.MustParsePrefix("172.31.0.0/16")}, Gateways: []netip.Addr{netip.MustParseAddr("172.31.0.1")}}}}
	spec := RunSpec{Repo: ws, IsolatedParent: repo}
	t.Setenv("COOP_TEST_VOLUME_MOUNTPOINT", repo)
	_, _, _, prepared, err := resolveServiceBindings(t.Context(), docker, rt, spec, file, approval, serviceGrants(servicePolicy(t, "db")), nil, nil)
	if prepared != nil {
		prepared.cleanup()
	}
	if err == nil || !strings.Contains(err.Error(), "parent checkout") {
		t.Fatalf("filtered preparation lost the scratch fence: %v", err)
	}
	if calls, _ := os.ReadFile(recorder); strings.Contains(string(calls), " up ") {
		t.Fatalf("Compose created a parent-backed service: %s", calls)
	}
	t.Setenv("COOP_TEST_VOLUME_MOUNTPOINT", t.TempDir())
	_, _, _, prepared, err = resolveServiceBindings(t.Context(), docker, rt, spec, file, approval, serviceGrants(servicePolicy(t, "db")), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(prepared.cleanup)
	before, err := os.ReadFile(recorder)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("COOP_TEST_VOLUME_MOUNTPOINT", repo)
	if err := prepared.start(t.Context()); err == nil || !strings.Contains(err.Error(), "parent checkout") {
		t.Fatalf("filtered final start lost the scratch fence: %v", err)
	}
	after, err := os.ReadFile(recorder)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(after[len(before):]), " up ") {
		t.Fatalf("Compose started after the volume changed: %s", after[len(before):])
	}
}

func isolatedMountFixture(t *testing.T, repo string) (string, forkspace.Identity) {
	t.Helper()
	command, err := forkspace.GitCommand(context.Background(), repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	head, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	ws, err := forkspace.SetupPinnedContext(context.Background(), repo, "safe", strings.TrimSpace(string(head)))
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := forkspace.LockState(repo, "safe")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := forkspace.EnsureIsolatedGenerationLocked(repo, "safe")
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	return ws, identity
}

func TestIsolatedLaunchRefusesHostAuthorityExtrasBeforeComposition(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "global"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "system"))
	repo, git := gitrepo.New(t)
	git("commit", "--allow-empty", "-qm", "base")
	ws, _ := isolatedMountFixture(t, repo)
	for _, args := range [][]string{
		{"--privileged"}, {"--device", "/dev/sda"}, {"--pid=host"}, {"--ipc", "host"},
		{"--network", "host"}, {"--cap-add", "SYS_ADMIN"}, {"--security-opt", "seccomp=unconfined"},
		{"--user", "0:0"}, {"-v", ws + ":/workspace:rshared"},
	} {
		for _, fromConfig := range []bool{false, true} {
			cfg := &config.Config{}
			spec := RunSpec{Repo: ws, ExpectedImageID: "invalid-image"}
			if fromConfig {
				cfg.ExtraRunArgs = args
			} else {
				spec.ExtraArgs = args
			}
			_, err := runWithCompositionArtifacts(cfg, runtime.Runtime{}, spec, defaultCompositionArtifactOps())
			if err == nil || !strings.Contains(err.Error(), "isolated") {
				t.Fatalf("isolated extra %v fromConfig=%v = %v", args, fromConfig, err)
			}
			// Ordinary admission stays unchanged; this test deliberately does not launch it.
			spec.Repo = repo
			if err := admitIsolatedOptions(cfg, spec); err != nil {
				t.Fatalf("ordinary runtime extras restricted: %v", err)
			}
		}
	}
}

func TestIsolatedMountRefusesSocketsAndKernelSources(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "global"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "system"))
	repo, git := gitrepo.New(t)
	git("commit", "--allow-empty", "-qm", "base")
	ws, _ := isolatedMountFixture(t, repo)
	// t.TempDir includes the long test name; Darwin's Unix socket path limit is 104 bytes.
	directory, err := os.MkdirTemp("", "coop-ipc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket := filepath.Join(directory, "control.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	alias := filepath.Join(t.TempDir(), "control-alias")
	if err := os.Symlink(directory, alias); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{socket, directory, alias, "/dev", "/proc", "/sys", "/run"} {
		if err := validateAuthorityMounts(t.Context(), RunSpec{Repo: ws}, []string{"-v", source + ":/control:ro"}, "", nil, authorityMountAllowlist{}); err == nil {
			t.Fatalf("host control source admitted: %s", source)
		}
	}
	reader := func(context.Context, []string) (runtime.VolumeExposure, error) {
		return runtime.VolumeExposure{Sources: []string{directory}}, nil
	}
	if err := validateAuthorityMounts(t.Context(), RunSpec{Repo: ws}, []string{"-v", "control:/control:ro"}, "", reader, authorityMountAllowlist{}); err == nil {
		t.Fatal("volume concealed a host control socket")
	}
}

func TestIsolatedMountRefusesUnobservableExistingVolume(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "global"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "system"))
	repo, git := gitrepo.New(t)
	git("commit", "--allow-empty", "-qm", "base")
	ws, _ := isolatedMountFixture(t, repo)
	missing := filepath.Join(t.TempDir(), "daemon-only-volume")
	for _, access := range []string{"ro", "rw"} {
		if err := validateAuthorityMounts(t.Context(), RunSpec{Repo: ws}, []string{"-v", missing + ":/external:" + access}, "", nil, authorityMountAllowlist{}); err == nil {
			t.Fatal("unobservable daemon-interpreted bind admitted")
		}
		reader := func(context.Context, []string) (runtime.VolumeExposure, error) {
			return runtime.VolumeExposure{Sources: []string{missing}}, nil
		}
		options := []string{"-v", "control:/control:" + access}
		if err := validateAuthorityMounts(t.Context(), RunSpec{Repo: ws}, options, "", reader, authorityMountAllowlist{}); err == nil {
			t.Fatal("existing daemon volume with unobservable contents admitted")
		}
		absent := func(context.Context, []string) (runtime.VolumeExposure, error) {
			return runtime.VolumeExposure{}, nil
		}
		if err := validateAuthorityMounts(t.Context(), RunSpec{Repo: ws}, options, "", absent, authorityMountAllowlist{}); err != nil {
			t.Fatalf("positively absent volume refused: %v", err)
		}
		if err := validateAuthorityMounts(t.Context(), RunSpec{Repo: ws}, options, "", reader, authorityMountAllowlist{volumes: map[string]bool{"control": true}}); err != nil {
			t.Fatalf("creator-owned private volume refused: %v", err)
		}
	}
}

func TestIsolatedSelectedSharedCacheTrustIsNotPrivateOwnership(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "global"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "system"))
	repo, git := gitrepo.New(t)
	git("commit", "--allow-empty", "-qm", "base")
	ws, _ := isolatedMountFixture(t, repo)
	cfg := &config.Config{BaseImage: "base", ConfigDir: t.TempDir(), HomeInBox: "/home/node"}
	spec := RunSpec{Repo: ws, Image: "base", Cache: true, Homes: true}
	missing := filepath.Join(t.TempDir(), "daemon-cache")
	reader := func(_ context.Context, names []string) (runtime.VolumeExposure, error) {
		proofs := map[string]runtime.PlainLocalVolume{}
		for _, name := range names {
			proofs[name] = runtime.PlainLocalVolume{Mountpoint: missing, ExposureRoot: missing, CreatedAt: "2026-10-08T00:00:00Z"}
		}
		return runtime.VolumeExposure{Sources: []string{missing}, PlainLocalVolumes: proofs}, nil
	}
	allow, err := protectRunPrivateState(cfg, spec, authorityMountAllowlist{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{boxAgentIdentity().volume("coop-cache"), boxAgentIdentity().volume("coop-asdf")} {
		if err := validateAuthorityMounts(t.Context(), spec, []string{"-v", name + ":/cache"}, "", reader, allow); err != nil {
			t.Fatalf("ordinary selected shared cache refused: %s: %v", name, err)
		}
	}
	name := boxAgentIdentity().volume("coop-cache")
	options := []string{"-v", name + ":/cache"}
	for _, disabled := range []RunSpec{{Repo: ws}, {Repo: ws, Homes: true, Image: "custom"}} {
		allow, err := protectRunPrivateState(cfg, disabled, authorityMountAllowlist{})
		if err != nil {
			t.Fatal(err)
		}
		if err := validateAuthorityMounts(t.Context(), disabled, options, "", reader, allow); err == nil {
			t.Fatal("unselected shared cache acquired trust")
		}
	}
	if err := validateAuthorityMounts(t.Context(), spec, []string{"-v", "external:/data"}, "", reader, allow); err == nil {
		t.Fatal("arbitrary plain local volume acquired cache trust")
	}
	for _, exposure := range []runtime.VolumeExposure{
		{Sources: []string{missing}},
		{Sources: []string{missing}, BindSources: []string{missing}, PlainLocalVolumes: map[string]runtime.PlainLocalVolume{name: {Mountpoint: missing, ExposureRoot: missing}}},
		{Sources: []string{missing}, PlainLocalVolumes: map[string]runtime.PlainLocalVolume{name: {Mountpoint: missing, ExposureRoot: repo}}},
		{Sources: []string{repo}, PlainLocalVolumes: map[string]runtime.PlainLocalVolume{name: {Mountpoint: missing, ExposureRoot: repo}}},
	} {
		bad := func(context.Context, []string) (runtime.VolumeExposure, error) { return exposure, nil }
		if err := validateAuthorityMounts(t.Context(), spec, options, "", bad, allow); err == nil {
			t.Fatalf("cache trust waived missing provenance or parent exposure: %+v", exposure)
		}
	}
	if err := validateAuthorityMounts(t.Context(), spec, options, "", func(context.Context, []string) (runtime.VolumeExposure, error) {
		return runtime.VolumeExposure{}, errors.New("inventory unavailable")
	}, allow); err == nil {
		t.Fatal("failed inventory acquired cache trust")
	}
	regular := filepath.Join(t.TempDir(), "not-a-volume-directory")
	if err := os.WriteFile(regular, []byte("readable non-directory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	knownFile := func(context.Context, []string) (runtime.VolumeExposure, error) {
		return runtime.VolumeExposure{Sources: []string{regular}, PlainLocalVolumes: map[string]runtime.PlainLocalVolume{
			name: {Mountpoint: regular, ExposureRoot: regular},
		}}, nil
	}
	if err := validateAuthorityMounts(t.Context(), spec, options, "", knownFile, allow); err == nil {
		t.Fatal("known readable non-directory acquired opaque cache trust")
	}
}

func TestIsolatedMountRefusesOutOfTreeHardlinks(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "global"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "system"))
	repo, git := gitrepo.New(t)
	tracked := filepath.Join(repo, "source.txt")
	if err := os.WriteFile(tracked, []byte("parent"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "source.txt")
	git("commit", "-qm", "base")
	ws, _ := isolatedMountFixture(t, repo)
	for _, target := range []string{tracked, filepath.Join(repo, ".git", "config")} {
		directory := t.TempDir()
		// A friendly marker name must not grant an external hardlink an exception.
		link := filepath.Join(directory, forkspace.GenerationMarkerName)
		if err := os.Link(target, link); err != nil {
			t.Fatal(err)
		}
		for _, access := range []string{"ro", "rw"} {
			for _, source := range []string{link, directory} {
				err := validateAuthorityMounts(t.Context(), RunSpec{Repo: ws}, []string{"-v", source + ":/external:" + access}, "", nil, authorityMountAllowlist{})
				if err == nil || !strings.Contains(err.Error(), "shared file inode") {
					t.Fatalf("shared source %s (%s) = %v", source, access, err)
				}
				if err := validateAuthorityMounts(t.Context(), RunSpec{Repo: repo}, []string{"-v", source + ":/external:" + access}, "", nil, authorityMountAllowlist{}); err != nil {
					t.Fatalf("ordinary mount changed: %v", err)
				}
			}
			reader := func(context.Context, []string) (runtime.VolumeExposure, error) {
				return runtime.VolumeExposure{Sources: []string{directory}}, nil
			}
			if err := validateAuthorityMounts(t.Context(), RunSpec{Repo: ws}, []string{"-v", "external:/external:" + access}, "", reader, authorityMountAllowlist{}); err == nil {
				t.Fatal("named volume concealed a parent hardlink")
			}
		}
	}
}

func TestIsolatedMountAcceptsOnlyValidatedPublicMarkers(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "global"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "system"))
	t.Setenv(ServiceStateRootEnv, t.TempDir())
	repo, git := gitrepo.New(t)
	git("commit", "--allow-empty", "-qm", "base")
	ws, _ := isolatedMountFixture(t, repo)
	networkPath := filepath.Join(t.TempDir(), "network")
	store, err := networkstate.Open(networkPath, []string{ws})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.ReviewApproval(ws, egress.None, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := serviceApprovalAnchor(ws, true); err != nil {
		t.Fatal(err)
	}
	options := []string{"-v", ws + ":/workspace"}
	if err := validateAuthorityMounts(t.Context(), RunSpec{Repo: ws}, options, networkPath, nil, authorityMountAllowlist{}); err != nil {
		t.Fatalf("real identity markers refused: %v", err)
	}
	path := filepath.Join(ws, serviceApprovalMarker)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateAuthorityMounts(t.Context(), RunSpec{Repo: ws}, options, networkPath, nil, authorityMountAllowlist{}); err == nil {
		t.Fatal("copied marker was treated as a private-anchor exception")
	}
}

func TestIsolatedServiceVolumesCannotExposeParentOrPrivateState(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "global"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "system"))
	repo, git := gitrepo.New(t)
	git("commit", "--allow-empty", "-qm", "base")
	ws, _ := isolatedMountFixture(t, repo)
	rt := serviceReviewRuntime(t, "")
	bound, err := rt.FreezeCompose(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, declaration := range []string{"    name: foreign\n", "    external: true\n", ""} {
		data := []byte("services:\n  db:\n    image: postgres:18\n    volumes: [\"data:/data:ro\"]\nvolumes:\n  data:\n" + declaration)
		file := filepath.Join(ws, "compose.yml")
		if err := os.WriteFile(file, data, 0o600); err != nil {
			t.Fatal(err)
		}
		for _, source := range []string{repo, filepath.Join(repo, ".git"), forkspace.StateDir(repo)} {
			t.Setenv("COOP_TEST_VOLUME_MOUNTPOINT", source)
			if err := validateIsolatedServiceVolumes(t.Context(), bound, ws, file, "", data); err == nil {
				t.Fatalf("service volume declaration %q exposed %s", declaration, source)
			}
			// A previously approved path still cannot reach start, including explicit coop up.
			_, cleanup, _, err := snapshotComposeArgsForStartPinned(t.Context(), bound, ws, file, "", data, true)
			if cleanup != nil {
				cleanup()
			}
			if err == nil {
				t.Fatalf("approved service volume exposed %s", source)
			}
			if _, err := ReviewServiceStart(ws, file, rt, true); err == nil {
				t.Fatal("offered approval for an isolation bypass")
			}
		}
		t.Setenv("COOP_TEST_VOLUME_MOUNTPOINT", t.TempDir())
		if err := validateIsolatedServiceVolumes(t.Context(), bound, ws, file, "", data); err != nil {
			t.Fatalf("ordinary isolated service data refused: %v", err)
		}
	}
}

func TestIsolatedMountDeniesParentAliasesAndVolumes(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "global"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "system"))
	repo, git := gitrepo.New(t)
	git("commit", "--allow-empty", "-qm", "base")
	ws, identity := isolatedMountFixture(t, repo)
	alias := filepath.Join(t.TempDir(), "parent-alias")
	if err := os.Symlink(repo, alias); err != nil {
		t.Fatal(err)
	}
	workspaceAlias := filepath.Join(t.TempDir(), "workspace-alias")
	if err := os.Symlink(ws, workspaceAlias); err != nil {
		t.Fatal(err)
	}
	// A command invoked directly from the fork must enforce the persisted boundary too.
	for _, spec := range []RunSpec{{Repo: ws}, {Repo: workspaceAlias}, {Repo: ws, ActivityRepo: repo, ForkName: identity.Name, ForkGeneration: string(identity.Generation)}} {
		for _, source := range []string{repo, alias, filepath.Dir(repo), filepath.Join(repo, ".git")} {
			err := validateAuthorityMounts(context.Background(), spec, []string{"-v", source + ":/parent"}, "", nil,
				authorityMountAllowlist{sources: map[string]bool{source: true}})
			if err == nil || !strings.Contains(err.Error(), "parent checkout") {
				t.Fatalf("parent source %s = %v", source, err)
			}
		}
		reader := func(context.Context, []string) (runtime.VolumeExposure, error) {
			return runtime.VolumeExposure{Sources: []string{repo}}, nil
		}
		if err := validateAuthorityMounts(context.Background(), spec, []string{"-v", "foreign:/parent:ro"}, "", reader, authorityMountAllowlist{}); err == nil {
			t.Fatal("inspected volume restored parent access")
		}
		if err := validateAuthorityMounts(context.Background(), spec, []string{"-v", ws + ":/workspace"}, "", nil, authorityMountAllowlist{}); err != nil {
			t.Fatalf("independent workspace refused: %v", err)
		}
	}
}

func TestIsolatedMountDeniesExternalParentGitMetadata(t *testing.T) {
	for _, topology := range []string{"linked-worktree", "separate-gitdir"} {
		t.Run(topology, func(t *testing.T) {
			t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "global"))
			t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "system"))
			repo, git := gitrepo.New(t)
			git("commit", "--allow-empty", "-qm", "base")
			if topology == "linked-worktree" {
				linked := filepath.Join(t.TempDir(), "linked")
				git("worktree", "add", "--detach", linked, "HEAD")
				repo = linked
			} else {
				git("init", "--separate-git-dir="+filepath.Join(t.TempDir(), "metadata"))
			}
			ws, _ := isolatedMountFixture(t, repo)
			roots, err := forkspace.GitMetadataDirectories(repo)
			if err != nil {
				t.Fatal(err)
			}
			for _, root := range roots {
				alias := filepath.Join(t.TempDir(), "metadata-alias")
				if err := os.Symlink(root, alias); err != nil {
					t.Fatal(err)
				}
				for _, source := range []string{root, alias, filepath.Dir(root)} {
					if err := validateAuthorityMounts(context.Background(), RunSpec{Repo: ws}, []string{"-v", source + ":/metadata"}, "", nil, authorityMountAllowlist{}); err == nil {
						t.Fatalf("external metadata admitted: %s", source)
					}
				}
				reader := func(context.Context, []string) (runtime.VolumeExposure, error) {
					return runtime.VolumeExposure{Sources: []string{root}}, nil
				}
				if err := validateAuthorityMounts(context.Background(), RunSpec{Repo: ws}, []string{"-v", "foreign:/metadata"}, "", reader, authorityMountAllowlist{}); err == nil {
					t.Fatal("volume exposed external metadata")
				}
			}
		})
	}
}
