package box

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestBuildContextHonorsDockerignore(t *testing.T) {
	pinHome(t)
	for _, tc := range []struct {
		name, root, specific, config string
		want                         []string
	}{
		{name: "root rules", root: "**\n!main.go\n", want: []string{".agent/Dockerfile", ".dockerignore", "main.go"}},
		{name: "specific overrides root", root: "main.go\n", specific: "**\n!main.go\n", want: []string{".agent/Dockerfile", ".agent/Dockerfile.dockerignore", "main.go"}},
		{name: "empty specific overrides root", root: "main.go\n", specific: "# empty\n", want: []string{".agent/Dockerfile", ".agent/Dockerfile.dockerignore", ".dockerignore", "main.go", "notes.txt"}},
		{name: "nested exception", root: "**\n!tools/nested/input.txt\n", want: []string{".agent/Dockerfile", ".dockerignore", "tools/nested/input.txt"}},
		{name: "custom Dockerfile", root: "main.go\n", specific: "**\n!main.go\n", config: "box:\n  dockerfile: docker/box.Dockerfile\n", want: []string{"docker/box.Dockerfile", "docker/box.Dockerfile.dockerignore", "main.go"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, write := gitProject(t, reusableDockerfile)
			write("notes.txt", "not a build input\n")
			write("tools/nested/input.txt", "selected\n")
			write(".dockerignore", tc.root)
			dockerfile := ".agent/Dockerfile"
			if tc.config != "" {
				write(".agent/project.yaml", tc.config)
				dockerfile = "docker/box.Dockerfile"
				write(dockerfile, reusableDockerfile)
			}
			if tc.specific != "" {
				write(dockerfile+".dockerignore", tc.specific)
			}
			entries, err := buildContextSelection(t.Context(), repo)
			if err != nil {
				t.Fatal(err)
			}
			var files []string
			for _, entry := range entries {
				if !entry.mode.IsDir() {
					files = append(files, filepath.ToSlash(entry.rel))
				}
			}
			if tc.name == "empty specific overrides root" {
				tc.want = append(tc.want, ".gitignore", "tools/nested/input.txt")
			}
			slices.Sort(tc.want)
			if !slices.Equal(files, tc.want) {
				t.Fatalf("selected files = %v, want %v", files, tc.want)
			}
			dir, digest, cleanup, err := stageContextEntries(t.Context(), repo, entries)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			if err := validateProjectBuildFiles(dir, entries, dockerfile); err != nil {
				t.Fatal(err)
			}
			if live := projectContextDigest(t, repo); live != digest {
				t.Fatalf("staged digest %s != live %s", digest, live)
			}
			for _, file := range files {
				if _, err := os.Stat(filepath.Join(dir, file)); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestDockerignoredEditsDoNotChangeBuildInputs(t *testing.T) {
	pinHome(t)
	repo, write := gitProject(t, reusableDockerfile)
	write(".agent/Dockerfile.dockerignore", "**\n!main.go\n")
	write("docs/notes.txt", "before\n")
	digest := projectContextDigest(t, repo)
	write("docs/notes.txt", "after\n")
	write("new/source/file.go", "package source\n")
	if err := os.Chmod(filepath.Join(repo, "docs/notes.txt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := projectContextDigest(t, repo); got != digest {
		t.Fatal("excluded source edits or directories changed the build fingerprint")
	}
	if err := os.Remove(filepath.Join(repo, "docs/notes.txt")); err != nil {
		t.Fatal(err)
	}
	if got := projectContextDigest(t, repo); got != digest {
		t.Fatal("deleting excluded source changed the build fingerprint")
	}
	write("main.go", "package main // changed build input\n")
	if got := projectContextDigest(t, repo); got == digest {
		t.Fatal("changed selected build input did not change the fingerprint")
	}
}

func TestDockerignoreCannotRestoreSanitizedInputs(t *testing.T) {
	pinHome(t)
	repo, write := gitProject(t, reusableDockerfile)
	write(".dockerignore", "**\n!main.go\n!.env\n!.git/**\n!build/**\n")
	write(".env", "SECRET=hidden\n")
	write("build/generated", "ignored output\n")
	entries, err := buildContextSelection(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		rel := filepath.ToSlash(entry.rel)
		if rel == ".env" || strings.HasPrefix(rel, ".git/") || strings.HasPrefix(rel, "build/") {
			t.Fatalf("Dockerignore restored sanitized input %s", rel)
		}
	}
}

func TestBuildContextRejectsInvalidDockerignore(t *testing.T) {
	for _, kind := range []string{"syntax", "symlink", "oversized", "gitignored", "shadowed"} {
		t.Run(kind, func(t *testing.T) {
			repo, write := gitProject(t, reusableDockerfile)
			path := filepath.Join(repo, ".agent/Dockerfile.dockerignore")
			switch kind {
			case "syntax":
				write(".agent/Dockerfile.dockerignore", "[\n")
			case "symlink":
				write("outside.txt", "**\n")
				if err := os.Symlink("../outside.txt", path); err != nil {
					t.Fatal(err)
				}
			case "oversized":
				write(".agent/Dockerfile.dockerignore", strings.Repeat("#", imageInputFileLimit+1))
			case "gitignored":
				write(".gitignore", ".agent/Dockerfile.dockerignore\n")
				write(".agent/Dockerfile.dockerignore", "**\n")
			case "shadowed":
				write(".coopignore", ".agent/Dockerfile.dockerignore\n")
				write(".agent/Dockerfile.dockerignore", "**\n")
			}
			if _, err := buildContextSelection(t.Context(), repo); err == nil {
				t.Fatal("invalid active Dockerignore was accepted")
			}
		})
	}
}
