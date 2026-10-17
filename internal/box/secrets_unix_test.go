//go:build darwin || linux

package box

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
)

// unreadableCoopignores are the .coopignore files coop cannot read: plant makes one at path, and
// reason is how the refusal describes it.
var unreadableCoopignores = []struct {
	name, reason string
	plant        func(*testing.T, string)
}{
	{"link", "is a link", func(t *testing.T, path string) {
		outside := filepath.Join(t.TempDir(), "policy")
		if err := os.WriteFile(outside, []byte("private.txt\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, path); err != nil {
			t.Fatal(err)
		}
	}},
	{"folder", "is a folder", func(t *testing.T, path string) {
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}},
	{"fifo", "is not a regular file", func(t *testing.T, path string) {
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
	}},
	{"oversized", "is larger than 1 MiB", func(t *testing.T, path string) {
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteString("private.txt\n"); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		if err := file.Truncate(coopIgnoreSnapshotLimit + 1); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		_ = file.Close()
	}},
}

// A .coopignore coop cannot read leaves what it hides unknown, so every plan of what a box sees
// refuses it, naming it and why, rather than drop its rules: the mounts, a service's binds and the
// build context, for one in the repository and one in a folder above it. None waits on a FIFO.
func TestAnUnreadableCoopignoreStopsWhatTheBoxSees(t *testing.T) {
	for _, kind := range unreadableCoopignores {
		for _, place := range []string{"repository", "folder above"} {
			t.Run(kind.name+" in the "+place, func(t *testing.T) {
				pinHome(t)
				outer := filepath.Join(os.Getenv("HOME"), "work")
				repo := filepath.Join(outer, "app")
				for rel, body := range map[string]string{"src/main.go": "package main\n", "private.txt": "hidden\n"} {
					mkdirs(t, filepath.Dir(filepath.Join(repo, rel)))
					if err := os.WriteFile(filepath.Join(repo, rel), []byte(body), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				compose := filepath.Join(repo, "compose.yml")
				body := fmt.Sprintf("services:\n  probe:\n    image: alpine:3.24.2\n    user: \"%d:%d\"\n    mem_limit: 128m\n    cpus: 1\n    volumes:\n      - \"./src:/src\"\n",
					os.Getuid(), os.Getgid())
				if err := os.WriteFile(compose, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
				data, err := readValidatedCompose(compose, repo, false)
				if err != nil {
					t.Fatal(err)
				}
				policy, want := filepath.Join(repo, CoopIgnoreFile), UnreadableCoopignore{File: CoopIgnoreFile, Reason: kind.reason}
				if place == "folder above" {
					policy, want.File = filepath.Join(outer, CoopIgnoreFile), filepath.Join(realPath(t, outer), CoopIgnoreFile)
				}
				kind.plant(t, policy)
				done := make(chan []error, 1)
				go func() {
					mounts, mountErr := ComputeMounts(repo, "/workspace")
					if mounts != nil {
						mountErr = errors.Join(mountErr, fmt.Errorf("the refusal came with a mount plan: %+v", mounts))
					}
					_, _, serviceErr := serviceShadowPlan(repo, compose, data)
					_, buildErr := buildContextSelection(context.Background(), repo)
					done <- []error{mountErr, serviceErr, buildErr}
				}()
				select {
				case errs := <-done:
					for i, plan := range []string{"mounts", "service binds", "build context"} {
						var got UnreadableCoopignore
						if !errors.As(errs[i], &got) || got != want {
							t.Errorf("%s: %v; want %q", plan, errs[i], want)
						}
					}
				case <-time.After(5 * time.Second):
					if unblock, err := os.OpenFile(policy, os.O_RDWR|syscall.O_NONBLOCK, 0); err == nil {
						_ = unblock.Close()
					}
					t.Fatal("an unreadable .coopignore blocked what the box sees")
				}
			})
		}
	}
	pinHome(t)
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, CoopIgnoreFile), []byte("private.txt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !mustDecide(t, repo)("private.txt") {
		t.Fatal("valid project policy did not hide its requested file")
	}
}

// Below a .coopignore coop cannot read, what the other rules hide is decided and carries no
// refusal, but what they leave visible still does, so what the box sees stops there: the mounts and
// a service's binds meet a secret name and a file a root rule hides before it, and bind the
// oversized .coopignore itself without asking about it.
func TestAnUnreadableCoopignoreStopsTheBoxPastWhatTheRulesHide(t *testing.T) {
	pinHome(t)
	repo := realPath(t, t.TempDir())
	for rel, body := range map[string]string{
		CoopIgnoreFile:       "*.prod.json\n",
		"config/.env":        "TOKEN=x\n",
		"config/a.prod.json": "{}\n",
		"config/app.yml":     "name: app\n",
	} {
		mkdirs(t, filepath.Dir(filepath.Join(repo, rel)))
		if err := os.WriteFile(filepath.Join(repo, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	oversized := unreadableCoopignores[3]
	if oversized.name != "oversized" {
		t.Fatalf("unreadableCoopignores[3] is %q; this test needs the regular file the mounts bind", oversized.name)
	}
	oversized.plant(t, filepath.Join(repo, "config", CoopIgnoreFile))
	want := UnreadableCoopignore{File: "config/.coopignore", Reason: oversized.reason}
	decide := NewShadowDecider(repo)
	for rel, err := range map[string]error{"config/.env": nil, "config/a.prod.json": nil, "config/app.yml": want} {
		if hidden, got := decide(rel); !hidden || got != err {
			t.Fatalf("%s: hidden=%t, %v; want hidden, %v", rel, hidden, got, err)
		}
	}
	compose := filepath.Join(repo, "compose.yml")
	body := fmt.Sprintf("services:\n  probe:\n    image: alpine:3.24.2\n    user: \"%d:%d\"\n    mem_limit: 128m\n    cpus: 1\n    volumes:\n      - \"./config:/config\"\n",
		os.Getuid(), os.Getgid())
	if err := os.WriteFile(compose, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := readValidatedCompose(compose, repo, false)
	if err != nil {
		t.Fatal(err)
	}
	_, mountErr := ComputeMounts(repo, "/workspace")
	_, _, serviceErr := serviceShadowPlan(repo, compose, data)
	_, buildErr := buildContextSelection(context.Background(), repo)
	for i, plan := range []string{"mounts", "service binds", "build context"} {
		var got UnreadableCoopignore
		if err := []error{mountErr, serviceErr, buildErr}[i]; !errors.As(err, &got) || got != want {
			t.Errorf("%s: %v; want %q", plan, err, want)
		}
	}
}

// The launch itself stops at the refusal: Docker is never asked to run a box without the rules.
func TestRunRefusesAnUnreadableCoopignore(t *testing.T) {
	pinHome(t)
	repo := realPath(t, t.TempDir())
	for rel, body := range map[string]string{"private.txt": "hidden\n", "src/app.js": "ok"} {
		mkdirs(t, filepath.Dir(filepath.Join(repo, rel)))
		if err := os.WriteFile(filepath.Join(repo, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "rules"), filepath.Join(repo, CoopIgnoreFile)); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "none"}
	recorder := filepath.Join(t.TempDir(), "runtime-args")
	spec := RunSpec{Image: "i", Repo: repo, Cmd: []string{"true"}, Batch: true, Quiet: true}
	_, err := Run(cfg, dockerRecorder(t, recorder), spec)
	if want := (UnreadableCoopignore{File: CoopIgnoreFile, Reason: "is a link"}); !errors.Is(err, want) {
		t.Fatalf("Run = %v; want %q", err, want)
	}
	if err.Error() != "coop cannot tell what .coopignore hides, because it is a link" {
		t.Errorf("the refusal reads %q", err)
	}
	recorded, _ := os.ReadFile(recorder)
	for _, line := range strings.Split(string(recorded), "\n") {
		if strings.HasPrefix(line, "run\x00") {
			t.Fatalf("Docker was asked to run a box: %q", line)
		}
	}
}
