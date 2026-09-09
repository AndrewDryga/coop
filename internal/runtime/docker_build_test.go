package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

const buildFixtureID = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func buildFixture(t *testing.T, mode string) (*Docker, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "docker")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	script := "#!/bin/sh\nexport GORACE='atexit_sleep_ms=0'\nexec " + quote(os.Args[0]) + " -test.run=^TestDockerBuildFixtureProcess$ -- " + quote(root) + " " + quote(mode) + " \"$@\"\n"
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	d, err := BindDocker(context.Background(), Runtime{Name: binary}, "unix:///fixture.sock", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Error(err)
		}
	})
	return d, root
}

func TestDockerBuildFixtureProcess(t *testing.T) {
	i := slices.Index(os.Args, "--")
	if i < 0 {
		return
	}
	root, mode := os.Args[i+1], os.Args[i+2]
	args := os.Args[i+3:]
	if len(args) < 5 || args[0] != "--config" || args[2] != "--host" || args[3] != "unix:///fixture.sock" {
		os.Exit(91)
	}
	clientConfig := args[1]
	args = args[4:]
	emit := func(v any) {
		if json.NewEncoder(os.Stdout).Encode(v) != nil {
			os.Exit(92)
		}
	}
	switch args[0] {
	case "info":
		if args[len(args)-1] == "{{json .ClientInfo.Plugins}}" {
			switch mode {
			case "plugin-missing":
				emit([]map[string]string{})
				os.Exit(0)
			case "plugin-error":
				emit([]map[string]string{{"Name": "buildx", "Path": filepath.Join(root, "docker"), "Err": "failed"}})
				os.Exit(0)
			case "plugin-duplicate":
				emit([]map[string]string{{"Name": "buildx", "Path": filepath.Join(root, "docker")}, {"Name": "buildx", "Path": filepath.Join(root, "docker")}})
				os.Exit(0)
			case "plugin-not-executable":
				emit([]map[string]string{{"Name": "buildx", "Path": filepath.Join(root, "not-executable")}})
				os.Exit(0)
			}
			emit([]map[string]string{{"Name": "buildx", "Path": filepath.Join(root, "docker")}})
			break
		}
		id := "fixture-daemon"
		if _, err := os.Stat(filepath.Join(root, "built")); err == nil && mode == "daemon-change" {
			id = "changed-daemon"
		}
		emit(DockerInfo{ID: id, OSType: "linux", Architecture: "amd64", ServerVersion: "29.4.0", KernelVersion: "fixture-kernel"})
	case "build":
		if !slices.Equal(args[:7], []string{"build", "--builder", "default", "--platform", "linux/amd64", "--progress", "plain"}) || args[len(args)-1] != "-" {
			os.Exit(93)
		}
		for _, key := range []string{"DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "BUILDX_BUILDER", "BUILDKIT_HOST", "EXPERIMENTAL_BUILDKIT_SOURCE_POLICY", "SOURCE_DATE_EPOCH", "HTTP_PROXY", "https_proxy", "ALL_PROXY", "AWS_ACCESS_KEY_ID"} {
			if _, ok := os.LookupEnv(key); ok {
				os.Exit(94)
			}
		}
		if os.Getenv("DOCKER_BUILDKIT") != "1" || os.Getenv("HOME") != clientConfig || os.Getenv("BUILDX_CONFIG") != filepath.Join(clientConfig, "buildx") {
			os.Exit(95)
		}
		if _, err := os.Stat(filepath.Join(clientConfig, "config.json")); !errors.Is(err, os.ErrNotExist) {
			os.Exit(96)
		}
		if target, err := filepath.EvalSymlinks(filepath.Join(clientConfig, "cli-plugins/docker-buildx")); err != nil || target != filepath.Join(root, "docker") {
			os.Exit(97)
		}
		data, err := io.ReadAll(os.Stdin)
		if err != nil || string(data) != "fixture tar" {
			os.Exit(98)
		}
		if os.MkdirAll(filepath.Join(clientConfig, "buildx/activity"), 0700) != nil || os.WriteFile(filepath.Join(clientConfig, "buildx/activity/record"), []byte("state"), 0600) != nil {
			os.Exit(99)
		}
		iid := args[slices.Index(args, "--iidfile")+1]
		if os.WriteFile(filepath.Join(root, "built"), []byte(iid), 0600) != nil {
			os.Exit(100)
		}
		switch mode {
		case "missing-id":
		case "symlink-id":
			if os.Symlink(filepath.Join(root, "built"), iid) != nil {
				os.Exit(101)
			}
		case "fifo-id":
			if syscall.Mkfifo(iid, 0600) != nil {
				os.Exit(102)
			}
		default:
			id := buildFixtureID
			if mode == "malformed-id" {
				id = "sha256:abc"
			}
			if mode == "oversized-id" {
				id = strings.Repeat("a", 4096)
			}
			if os.WriteFile(iid, []byte(id+"\n"), 0644) != nil {
				os.Exit(103)
			}
		}
		if mode == "cancel" {
			time.Sleep(time.Minute)
		}
		if mode == "failed" {
			os.Exit(1)
		}
	case "image":
		if args[len(args)-1] != buildFixtureID {
			os.Exit(104)
		} // mutable tags must NEVER be inspected
		image := DockerBuiltImage{ID: buildFixtureID, OS: "linux", Architecture: "amd64", Labels: map[string]string{"coop.candidate": "fixture"}}
		switch mode {
		case "platform-mismatch":
			image.Architecture = "arm64"
		case "label-mismatch":
			image.Labels = nil
		case "id-mismatch":
			image.ID = "sha256:" + strings.Repeat("c", 64)
		}
		emit(image)
	default:
		os.Exit(105)
	}
	os.Exit(0)
}

func fixtureBuildSpec() DockerBuild {
	return DockerBuild{Tag: "coop-candidate:fixture", Platform: "linux/amd64", Args: map[string]string{"NODE_IMAGE": "node@sha256:fixture"}, Labels: map[string]string{"coop.candidate": "fixture"}}
}

func TestDockerBuildBindsImageAndCleansOnlyItsPrivateState(t *testing.T) {
	for _, key := range []string{"BUILDX_BUILDER", "BUILDKIT_HOST", "EXPERIMENTAL_BUILDKIT_SOURCE_POLICY", "SOURCE_DATE_EPOCH", "HTTP_PROXY", "https_proxy", "ALL_PROXY", "AWS_ACCESS_KEY_ID"} {
		t.Setenv(key, "host-value-must-not-cross")
	}
	d, root := buildFixture(t, "success")
	image, err := d.BuildImage(context.Background(), fixtureBuildSpec(), []byte("fixture tar"), nil, nil)
	if err != nil || image.ID != buildFixtureID {
		t.Fatal(image, err)
	}
	data, err := os.ReadFile(filepath.Join(root, "built"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(string(data))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("build scratch retained", err)
	}
	if entries, err := os.ReadDir(d.clientConfig); err != nil || len(entries) != 0 {
		t.Fatal("lifecycle config polluted", entries, err)
	}
}

func TestDockerBuildRefusesAmbiguousOrDifferentResults(t *testing.T) {
	wants := map[string]string{"missing-id": "image ID unavailable", "symlink-id": "image ID unavailable", "fifo-id": "image ID file", "malformed-id": "invalid Docker build image ID", "oversized-id": "image ID file", "failed": "client exit 1", "daemon-change": "qualification changed", "platform-mismatch": "identity or platform mismatch", "label-mismatch": "labels mismatch", "id-mismatch": "identity or platform mismatch"}
	for mode, want := range wants {
		t.Run(mode, func(t *testing.T) {
			d, _ := buildFixture(t, mode)
			image, err := d.BuildImage(context.Background(), fixtureBuildSpec(), []byte("fixture tar"), nil, nil)
			if err == nil || image.ID != "" {
				t.Fatal("accepted unproven build", image, err)
			}
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("did not reach intended refusal: %v, want %s", err, want)
			}
		})
	}
}

func TestDockerBuildCancellationNeverAdoptsPublishedID(t *testing.T) {
	d, root := buildFixture(t, "cancel")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		image, err := d.BuildImage(ctx, fixtureBuildSpec(), []byte("fixture tar"), nil, nil)
		if image.ID != "" {
			err = fmt.Errorf("canceled image accepted: %s", image.ID)
		}
		done <- err
	}()
	deadline := time.After(30 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
waiting:
	for {
		select {
		case err := <-done:
			t.Fatal("build did not wait for cancellation", err)
		case <-deadline:
			t.Fatal("fixture build never submitted")
		case <-ticker.C:
			if _, err := os.Stat(filepath.Join(root, "built")); err == nil {
				break waiting
			}
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("build cancellation did not reap")
	}
}

func TestDockerBuildFiniteGrammarAndRecoveryFence(t *testing.T) {
	for _, mutate := range []func(*DockerBuild){
		func(s *DockerBuild) { s.Tag = "--push" }, func(s *DockerBuild) { s.Tag = "registry.invalid/image:tag" }, func(s *DockerBuild) { s.Platform = "linux/amd64,linux/arm64" },
		func(s *DockerBuild) { s.Args = map[string]string{"IMPORT_HOST_ENV": "\nsecret"} }, func(s *DockerBuild) { s.Args = map[string]string{"KEY=value": "other"} }, func(s *DockerBuild) { s.Labels = nil },
	} {
		s := fixtureBuildSpec()
		mutate(&s)
		if s.valid() {
			t.Fatal("unsafe grammar", s)
		}
	}
	d, root := buildFixture(t, "success")
	d.launchAllowed = false
	if _, err := d.BuildImage(context.Background(), fixtureBuildSpec(), []byte("fixture tar"), nil, nil); err == nil {
		t.Fatal("recovery binding built image")
	}
	if _, err := os.Stat(filepath.Join(root, "built")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("build submitted", err)
	}
}

func TestDockerBuildPluginDiscoveryRefusesUnavailableOrAmbiguousTooling(t *testing.T) {
	for _, mode := range []string{"plugin-missing", "plugin-error", "plugin-duplicate", "plugin-not-executable"} {
		t.Run(mode, func(t *testing.T) {
			d, root := buildFixture(t, mode)
			if err := os.WriteFile(filepath.Join(root, "not-executable"), []byte("not an executable"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := d.BuildImage(context.Background(), fixtureBuildSpec(), []byte("fixture tar"), nil, nil); err == nil {
				t.Fatal("unproven plugin accepted")
			}
			if _, err := os.Stat(filepath.Join(root, "built")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("build submitted", err)
			}
		})
	}
}

func TestDockerBindingDoesNotRequireBuildConfiguration(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("DOCKER_CONFIG", "")
	d, _ := buildFixture(t, "success")
	if err := d.Verify(context.Background()); err != nil {
		t.Fatal("lifecycle depends on build config", err)
	}
	if _, err := d.BuildImage(context.Background(), fixtureBuildSpec(), []byte("fixture tar"), nil, nil); err == nil || !strings.Contains(err.Error(), "plugin configuration path unavailable") {
		t.Fatal("build did not refuse missing configuration", err)
	}
}
