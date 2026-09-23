//go:build darwin || linux

package box

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
)

func TestImageInputReadsRefuseRepositoryFIFOsWithoutBlocking(t *testing.T) {
	for _, name := range []string{".agent/Dockerfile", ".tool-versions"} {
		t.Run(name, func(t *testing.T) {
			repo := t.TempDir()
			if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
				t.Fatal(err)
			}
			if name != ".agent/Dockerfile" {
				if err := os.WriteFile(filepath.Join(repo, ".agent", "Dockerfile"), []byte("FROM scratch\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			fifo := filepath.Join(repo, filepath.FromSlash(name))
			if err := syscall.Mkfifo(fifo, 0o600); err != nil {
				t.Fatal(err)
			}
			done := make(chan bool, 1)
			go func() {
				_, ok := inputsHash(repo)
				done <- ok
			}()
			select {
			case ok := <-done:
				if name == ".agent/Dockerfile" && ok {
					t.Fatal("FIFO Dockerfile produced an image input hash")
				}
			case <-time.After(2 * time.Second):
				if unblock, err := os.OpenFile(fifo, os.O_RDWR|syscall.O_NONBLOCK, 0); err == nil {
					_ = unblock.Close()
				}
				t.Fatal("image staleness check blocked on a repository FIFO")
			}
		})
	}
}

func TestPlanBuildRefusesFIFODockerfileWithoutBlocking(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(repo, ".agent", "Dockerfile")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(t.TempDir(), "runtime")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := PlanBuild(runtime.Runtime{Name: shim}, &config.Config{BaseImage: "coop-box:test"}, repo, false)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("build plan accepted a FIFO Dockerfile")
		}
	case <-time.After(2 * time.Second):
		if unblock, err := os.OpenFile(fifo, os.O_RDWR|syscall.O_NONBLOCK, 0); err == nil {
			_ = unblock.Close()
		}
		t.Fatal("build plan blocked on a FIFO Dockerfile")
	}
}
