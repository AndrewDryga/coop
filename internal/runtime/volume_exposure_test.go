package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

type volumeExposureFixture struct {
	Kind, Mode string
	Definition volumeDefinition
	Present    bool
	Creates    int
}

func volumeExposureRuntime(t *testing.T, fixture volumeExposureFixture) (Runtime, string) {
	t.Helper()
	root := t.TempDir()
	file := filepath.Join(root, "fixture.json")
	data, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, fixture.Kind)
	script := "#!/bin/sh\nexport GORACE='atexit_sleep_ms=0'\nexec " + shellQuote(os.Args[0]) + " -test.run=^TestVolumeExposureFixtureProcess$ -- " + shellQuote(file) + " \"$@\"\n"
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return Runtime{Name: binary}, file
}

func TestVolumeExposureFixtureProcess(t *testing.T) {
	index := slices.Index(os.Args, "--")
	if index < 0 {
		return
	}
	args := os.Args[index+1:]
	if len(args) < 3 {
		os.Exit(91)
	}
	file := args[0]
	args = args[1:]
	data, err := os.ReadFile(file)
	var fixture volumeExposureFixture
	if err != nil || json.Unmarshal(data, &fixture) != nil || args[0] != "volume" {
		os.Exit(92)
	}
	switch args[1] {
	case "inspect":
		if !fixture.Present || fixture.Mode == "unavailable" {
			os.Exit(1)
		}
		if fixture.Mode == "oversized" {
			fmt.Print(strings.Repeat("x", 65537))
			os.Exit(0)
		}
		if fixture.Kind == "container" {
			d := fixture.Definition
			data, _ = json.Marshal([]any{map[string]any{"id": d.Name, "configuration": map[string]any{"name": d.Name, "driver": d.Driver, "format": "ext4", "source": d.Mountpoint, "options": d.Options}}})
		} else {
			data, _ = json.Marshal(fixture.Definition)
		}
		_, _ = os.Stdout.Write(data)
	case "ls":
		if fixture.Mode == "unavailable" {
			os.Exit(125)
		}
		if fixture.Present {
			fmt.Println(fixture.Definition.Name)
		}
	case "create":
		if fixture.Present {
			os.Exit(93)
		}
		fixture.Present = true
		fixture.Creates++
		data, _ = json.Marshal(fixture)
		if os.WriteFile(file, data, 0600) != nil {
			os.Exit(94)
		}
		fmt.Println(fixture.Definition.Name)
	default:
		os.Exit(95)
	}
	os.Exit(0)
}

func TestNamedVolumeExposurePreservesColdAndWarmLocalVolumes(t *testing.T) {
	for _, kind := range []string{"docker", "podman", "container"} {
		for _, present := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", kind, present), func(t *testing.T) {
				definition := volumeDefinition{Name: "coop-cache", Driver: "local", Scope: "local", Mountpoint: "/runtime/volumes/coop-cache/data"}
				if kind == "container" {
					definition.Options = map[string]string{"size": "10G", "journal": "ordered"}
				}
				rt, file := volumeExposureRuntime(t, volumeExposureFixture{Kind: kind, Present: present, Definition: definition})
				paths, err := rt.NamedVolumeExposure(context.Background(), []string{"coop-cache", "coop-cache"}, true)
				if err != nil || !slices.Equal(paths.Sources, []string{definition.Mountpoint}) || len(paths.BindSources) != 0 {
					t.Fatal(paths, err)
				}
				if _, err := rt.NamedVolumeExposure(context.Background(), []string{"coop-cache"}, false); err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(file)
				var got volumeExposureFixture
				if err != nil || json.Unmarshal(data, &got) != nil {
					t.Fatal(err)
				}
				want := 0
				if !present {
					want = 1
				}
				if got.Creates != want {
					t.Fatal("existing or duplicate volume recreated", got.Creates)
				}
			})
		}
	}
}

func TestExistingNamedVolumeExposureNeverCreates(t *testing.T) {
	for _, test := range []struct {
		name, mode    string
		present, fail bool
	}{{name: "absent"}, {name: "present", present: true}, {name: "unavailable", mode: "unavailable", fail: true}} {
		t.Run(test.name, func(t *testing.T) {
			definition := volumeDefinition{Name: "coop-cache", Driver: "local", Scope: "local", Mountpoint: "/runtime/cache"}
			rt, file := volumeExposureRuntime(t, volumeExposureFixture{Kind: "docker", Present: test.present, Mode: test.mode, Definition: definition})
			paths, err := rt.ExistingNamedVolumeExposure(context.Background(), []string{"coop-cache"})
			if (err != nil) != test.fail {
				t.Fatal(paths, err)
			}
			if !test.fail && ((test.present && !slices.Equal(paths.Sources, []string{definition.Mountpoint})) || (!test.present && len(paths.Sources) != 0)) {
				t.Fatal("wrong exposure", paths)
			}
			data, err := os.ReadFile(file)
			var got volumeExposureFixture
			if err != nil || json.Unmarshal(data, &got) != nil || got.Creates != 0 || got.Present != test.present {
				t.Fatal("read-only inventory mutated a volume", string(data), err)
			}
		})
	}
}

func TestNamedVolumeInspectionFailureNeverMeansAbsentOrSafe(t *testing.T) {
	for _, mode := range []string{"unavailable", "oversized", "wrong-name", "plugin"} {
		t.Run(mode, func(t *testing.T) {
			definition := volumeDefinition{Name: "coop-cache", Driver: "local", Scope: "local", Mountpoint: "/runtime/data"}
			if mode == "wrong-name" {
				definition.Name = "coop-cache-suffix"
			}
			if mode == "plugin" {
				definition.Driver = "remote"
			}
			rt, file := volumeExposureRuntime(t, volumeExposureFixture{Kind: "docker", Mode: mode, Present: true, Definition: definition})
			if _, err := rt.NamedVolumeExposure(context.Background(), []string{"coop-cache"}, true); err == nil {
				t.Fatal("failed/partial inspection accepted")
			}
			data, err := os.ReadFile(file)
			var got volumeExposureFixture
			if err != nil || json.Unmarshal(data, &got) != nil || got.Creates != 0 {
				t.Fatal("failed inspection triggered creation", err)
			}
		})
	}
	rt, _ := volumeExposureRuntime(t, volumeExposureFixture{Kind: "docker"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := rt.NamedVolumeExposure(ctx, []string{"coop-cache"}, true); err == nil {
		t.Fatal("cancelled inspection accepted")
	}
}

func TestNamedVolumeBackingPathsExposeHiddenHostBind(t *testing.T) {
	value := volumeDefinition{Name: "cache", Driver: "local", Scope: "local", Mountpoint: "/runtime/cache", Options: map[string]string{"type": "none", "device": "/private/authority", "o": "ro,rbind"}}
	paths, err := volumeBackingPaths(value, false)
	if err != nil || !slices.Equal(paths, []string{"/runtime/cache", "/private/authority"}) {
		t.Fatal("host backing path hidden by volume name", paths, err)
	}
	value.Options = map[string]string{"type": "nfs", "device": ":/private/authority", "o": "addr=host"}
	if _, err := volumeBackingPaths(value, false); err == nil {
		t.Fatal("remote driver source treated as inspectable host path")
	}
}

func TestVolumeMountpointDoesNotWaiveOwnerControlledInaccessibility(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root bypasses directory search permissions")
	}
	for _, mode := range []os.FileMode{0600, 0620, 0602} {
		dir := filepath.Join(t.TempDir(), "blocked")
		if err := os.Mkdir(dir, mode); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
		path := filepath.Join(dir, "volumes", "data")
		if _, err := filepath.EvalSymlinks(path); !errors.Is(err, os.ErrPermission) {
			t.Fatal("fixture is not inaccessible", err)
		}
		if _, err := volumeMountpointExposure(path); err == nil {
			t.Fatal("uninspectable owner path got managed waiver")
		}
	}
}

func TestNamedVolumeProtectsWholeRootPrivateParent(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("requires unprivileged owner")
	}
	// Read-only fixture: the platform's root home is normally a root-owned
	// unsearchable directory. No files are created or changed beneath it.
	for _, dir := range []string{"/var/root", "/root"} {
		parent, err := filepath.EvalSymlinks(dir)
		if err != nil {
			continue
		}
		info, err := os.Lstat(parent)
		if err != nil {
			continue
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 || !info.IsDir() || info.Mode().Perm()&0022 != 0 {
			continue
		}
		path := filepath.Join(parent, "coop-volume-readonly-fixture", "data")
		if _, err := filepath.EvalSymlinks(path); !errors.Is(err, os.ErrPermission) {
			continue
		}
		rt, _ := volumeExposureRuntime(t, volumeExposureFixture{Kind: "docker", Present: true, Definition: volumeDefinition{Name: "cache", Driver: "local", Scope: "local", Mountpoint: path}})
		exposure, err := rt.NamedVolumeExposure(context.Background(), []string{"cache"}, false)
		if err != nil || !slices.Equal(exposure.Sources, []string{parent}) || !slices.Equal(exposure.ManagedParents, []string{parent}) || len(exposure.BindSources) != 0 {
			t.Fatal("managed subtree not conservatively exposed", exposure, err)
		}
		return
	}
	t.Skip("no root-private directory available for read-only permission fixture")
}
