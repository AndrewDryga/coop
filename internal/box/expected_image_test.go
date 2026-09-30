package box

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
)

func TestRunBindsExpectedImmutableImage(t *testing.T) {
	id := "sha256:" + strings.Repeat("a", 64)
	for _, tc := range []struct {
		name, image, expected, refusal string
		mode                           agents.ExecutionMode
	}{
		{name: "unchanged default", image: "mutable:tag"},
		{name: "exact immutable image", image: id, expected: id},
		{name: "mutable alias", image: "mutable:tag", expected: id, refusal: "image selected"},
		{name: "different image", image: "sha256:" + strings.Repeat("b", 64), expected: id, refusal: "image selected"},
		{name: "missing image", expected: id, refusal: "image selected"},
		{name: "tag expectation", image: "mutable:tag", expected: "mutable:tag", refusal: "immutable image ID"},
		{name: "short digest", image: "sha256:aaa", expected: "sha256:aaa", refusal: "immutable image ID"},
		{name: "nonhex digest", image: "sha256:" + strings.Repeat("z", 64), expected: "sha256:" + strings.Repeat("z", 64), refusal: "immutable image ID"},
		{name: "restricted refuses", image: id, expected: id, mode: agents.ModeReadOnly, refusal: "expected image"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := filepath.Join(t.TempDir(), "runtime-args")
			cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "none", BaseImage: tc.image}
			spec := RunSpec{Image: tc.image, ExpectedImageID: tc.expected, Repo: t.TempDir(), Workdir: "/workspace",
				Cmd: []string{"true"}, Mode: tc.mode, Batch: true, Quiet: true}
			code, err := Run(cfg, recorderRuntime(t, recorder), spec)
			if tc.refusal != "" {
				if err == nil || !strings.Contains(err.Error(), tc.refusal) {
					t.Fatalf("Run = %d, %v; want refusal naming %q", code, err, tc.refusal)
				}
				if _, err := os.Stat(recorder); !os.IsNotExist(err) {
					t.Fatalf("refused image reached runtime: %v", err)
				}
				return
			}
			if err != nil || code != 0 {
				t.Fatalf("Run = %d, %v", code, err)
			}
			args, err := os.ReadFile(recorder)
			if err != nil || !slices.Contains(strings.Fields(string(args)), tc.image) {
				t.Fatalf("runtime did not receive exact image %q: %v, %q", tc.image, err, args)
			}
		})
	}
}

func TestFilteredImageBindingUsesProvedEffectiveImage(t *testing.T) {
	for _, tc := range []struct {
		name, hint, expected, refusal string
		project, unapproved           bool
	}{
		{name: "approved image wins over hint", project: true, hint: fixtureLockedImage, expected: fixtureBuiltImage},
		{name: "matching hint cannot hide drift", project: true, hint: fixtureLockedImage, expected: fixtureLockedImage, refusal: "image selected"},
		{name: "matching image needs approval", project: true, unapproved: true, hint: fixtureBuiltImage, expected: fixtureBuiltImage, refusal: "explicit build"},
		{name: "qualified base matches", hint: fixtureBuiltImage, expected: fixtureLockedImage},
		{name: "qualified base changed", hint: fixtureBuiltImage, expected: fixtureBuiltImage, refusal: "image selected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, daemon := filteredFixture(t)
			closure := derivedImageFixture(t, daemon)
			repo := t.TempDir()
			if tc.project {
				repo, _ = gitProject(t, reusableDockerfile)
				definition, _, _, err := lockedImageDefinition(closure.Platform)
				if err != nil {
					t.Fatal(err)
				}
				daemon.images[definition.Tag] = daemon.images[fixtureLockedImage]
				if !tc.unapproved {
					tag := filteredProjectTag(repo, fixtureLockedImage)
					inputs := projectBuildInputs(projectContextDigest(t, repo), fixtureCandidate(), closure, definition.Tag, tag, ".agent/Dockerfile")
					if err := f.store.ApproveProjectBuild(tag, inputs, fixtureBuiltImage); err != nil {
						t.Fatal(err)
					}
				}
			}
			err := f.selectImage(context.Background(), runtime.Runtime{}, &config.Config{}, RunSpec{
				Repo: repo, Image: tc.hint, ExpectedImageID: tc.expected, Quiet: true,
			}, fixtureCandidate(), true)
			if tc.refusal != "" {
				if err == nil || !strings.Contains(err.Error(), tc.refusal) {
					t.Fatalf("selection = %v; want refusal naming %q", err, tc.refusal)
				}
			} else if err != nil || f.image != tc.expected {
				t.Fatalf("selected %q, %v; want %q", f.image, err, tc.expected)
			}
			for _, event := range daemon.log {
				if strings.HasPrefix(event, "start:") {
					t.Fatalf("image selection started a workload: %s", event)
				}
			}
		})
	}
}
