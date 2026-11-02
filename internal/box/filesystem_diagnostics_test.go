package box

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
)

func TestFilesystemBuildDiagnosticSurvivesCleanup(t *testing.T) {
	file := filepath.Join(t.TempDir(), "build.log")
	if err := os.WriteFile(file, []byte(strings.Repeat("old output\n", 10000)+"\x1b[31mcompiler failed\x1b[0m\n"), 0600); err != nil {
		t.Fatal(err)
	}
	err := filesystemBuildError(context.Canceled, file)
	if removeErr := os.Remove(file); removeErr != nil {
		t.Fatal(removeErr)
	}
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "compiler failed") || strings.Contains(err.Error(), "\x1b") || len(err.Error()) > 17000 || strings.Count(err.Error(), "old output") > 19 {
		t.Fatalf("unusable bounded diagnostic: %v", err)
	}
	if err := filesystemBuildError(context.Canceled, file); !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "read filesystem build diagnostic") {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := filesystemBuildError(context.Canceled, file); err != context.Canceled {
		t.Fatal(err)
	}
}

func TestFilesystemSelectionReportsQuietFallback(t *testing.T) {
	var reports []FilesystemReport
	spec := RunSpec{Quiet: true, OnFilesystemReport: func(report FilesystemReport) { reports = append(reports, report) }}
	a, err := selectFilesystemAdmission(t.Context(), runtime.Runtime{Name: "podman"}, &config.Config{}, spec, nil, nil, nil)
	if err != nil || a != nil || len(reports) != 1 || reports[0].Backend != "mounts" || reports[0].Reason == "" {
		t.Fatalf("fallback: %v %v %+v", a, err, reports)
	}
}

func TestRunPreservesFilesystemFallbackReason(t *testing.T) {
	repo := t.TempDir()
	if !filesystemProfileCandidate(repo) {
		t.Skip("requires a case-insensitive APFS share")
	}
	recorder := filepath.Join(t.TempDir(), "calls")
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\necho \"$@\" >> "+strconv.Quote(recorder)+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_HOST", "tcp://unsupported.invalid:2375")
	cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "none", NoNewPrivileges: true}
	for _, name := range []string{"credentials.json", ".env"} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(repo, name), []byte("inert fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Remove(filepath.Join(repo, name)) })
			if !foldsCase(filepath.Join(repo, name)) {
				t.Skip("requires a case-insensitive share")
			}
			var reports []FilesystemReport
			var code int
			var err error
			out := captureStderr(t, func() {
				code, err = Run(cfg, runtime.Runtime{Name: "docker"}, RunSpec{
					Image: "i", Repo: repo, Workdir: "/workspace", Cmd: []string{"true"}, Batch: true, Quiet: true,
					OnFilesystemReport: func(report FilesystemReport) { reports = append(reports, report) },
				})
			})
			if len(reports) != 1 || reports[0].Backend != "mounts" || !strings.Contains(reports[0].Reason, "absolute local unix:// Docker endpoint") {
				t.Fatalf("qualification report = %+v; Run = %d, %v", reports, code, err)
			}
			if name == "credentials.json" {
				var fallback *FilesystemFallbackError
				var uncovered *UncoveredSpellings
				if code != -1 || !errors.As(err, &fallback) || !errors.As(err, &uncovered) || !strings.Contains(err.Error(), reports[0].Reason) {
					t.Fatalf("lost qualification or fallback error: Run = %d, %v", code, err)
				}
				if calls, _ := os.ReadFile(recorder); strings.Contains(string(calls), "run ") {
					t.Fatalf("refused workload ran: %s", calls)
				}
			} else if code != 0 || err != nil || out != "" {
				t.Fatalf("supported fallback = %d, %v; output %q", code, err, out)
			} else if calls, _ := os.ReadFile(recorder); !strings.Contains(string(calls), "run ") {
				t.Fatalf("supported fallback did not launch: %s", calls)
			}
		})
	}
}

func TestFilesystemPreparationNarrationStartsBeforeBuild(t *testing.T) {
	for _, quiet := range []bool{false, true} {
		out := captureStderr(t, func() {
			sections := newLaunchSections(RunSpec{Quiet: quiet})
			sections.prepareSecrets()
			sections.filesystemProgress("Building the filesystem protection helper")
			sections.secrets(2)
		})
		if quiet {
			if out != "" {
				t.Fatal(out)
			}
			continue
		}
		if strings.Count(out, "Protecting secrets") != 1 || strings.Index(out, "Protecting secrets") > strings.Index(out, "Building the filesystem") || strings.Index(out, "Building the filesystem") > strings.Index(out, "2 secret paths hidden") {
			t.Fatal(out)
		}
	}
}

func TestFilesystemFallbackRefusalHasOneRenderer(t *testing.T) {
	fallback := &FilesystemFallbackError{Reason: "share qualification failed", Err: &UncoveredSpellings{Paths: []string{"credentials.json"}}}
	for _, err := range []error{fallback, errors.Join(fallback, errors.New("cleanup remains pending"))} {
		sections := newLaunchSections(RunSpec{Agent: "claude"})
		captureStderr(t, func() { sections.prepareSecrets() })
		out := captureStderr(t, func() {
			if got := sections.failed(err); got != err {
				t.Fatalf("mount refusal changed before CLI rendering: %v", got)
			}
		})
		if out != "" {
			t.Fatalf("mount refusal was rendered before its remedy: %s", out)
		}
	}
}
