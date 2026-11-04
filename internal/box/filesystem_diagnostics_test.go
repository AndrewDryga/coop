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

// The fixture is the log of a real failed helper build. Its first compiler error is above the last
// ten step lines BuildKit repeats in its footer, and the footer alone fills the old last 20 lines.
func TestFilesystemBuildDiagnosticKeepsTheCompilerError(t *testing.T) {
	err := filesystemBuildError(context.Canceled, filepath.Join("testdata", "filesystem_helper_build_failure.log"))
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Filesystem helper build output:",
		"loader.c: In function 'recover':",
		"loader.c:334:105: error: '%s' directive output may be truncated",
		"cc1: all warnings being treated as errors",
		"did not complete successfully: exit code: 1",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%v", want, err)
		}
	}
	for _, wrapper := range []string{"Dockerfile:15", ">>>", "#8 "} {
		if strings.Contains(err.Error(), wrapper) {
			t.Errorf("kept the wrapper %q in:\n%v", wrapper, err)
		}
	}
	if len(err.Error()) > 17000 {
		t.Fatalf("unbounded diagnostic: %d bytes", len(err.Error()))
	}
}

func TestFilesystemBuildDiagnosticSelectsTheCause(t *testing.T) {
	footer := "------\n > [build 5/5] RUN make:\n------\nDockerfile:15\n--------------------\n" +
		strings.Repeat("  15 | >>> "+strings.Repeat("x", 1024)+"\n", 40) +
		"--------------------\nERROR: failed to build: failed to solve: process \"/bin/sh -c make\" did not complete successfully: exit code: 137\n"
	var compile strings.Builder
	compile.WriteString("#8 [build 5/5] RUN make\n#8 0.1 go: downloading golang.org/x/sys v0.49.0\n#8 0.2 loader.c:10:3: error: unknown type name 'first_cause'\n")
	for i := range 60 {
		compile.WriteString("#8 0.3 loader.c:" + strconv.Itoa(i+20) + ":3: note: cascade " + strconv.Itoa(i) + "\n")
	}
	compile.WriteString("#8 0.4 cc1: all warnings being treated as errors\n#8 ERROR: process \"/bin/sh -c make\" did not complete successfully: exit code: 1\n")
	for _, tc := range []struct {
		name   string
		log    string
		want   []string
		reject []string
	}{
		{
			name:   "the first of many compiler errors survives a footer larger than the old window",
			log:    compile.String() + footer,
			want:   []string{"error: unknown type name 'first_cause'", "note: cascade 59", "cc1: all warnings being treated as errors", "lines omitted", "exit code: 137"},
			reject: []string{"go: downloading", "note: cascade 30", ">>>"},
		},
		{
			name: "a package manager failure, with another stage's lines mixed in",
			log: "#5 [build 2/5] RUN apt-get update\n#6 [other 1/1] RUN sleep\n#5 0.1 Get:1 https://snapshot.debian.org/ bookworm InRelease\n#6 0.1 unrelated stage output\n" +
				"#5 0.2 E: Failed to fetch https://snapshot.debian.org/ 503 Service Unavailable\n#5 ERROR: process \"/bin/sh -c apt-get update\" did not complete successfully: exit code: 100\n#6 CANCELED\n" + footer,
			want:   []string{"Get:1 https://snapshot.debian.org/", "E: Failed to fetch https://snapshot.debian.org/ 503 Service Unavailable", "exit code: 137"},
			reject: []string{"unrelated stage output", ">>>", "lines omitted"},
		},
		{
			name: "a step that failed without output reads from the end of the log",
			log: "#3 [internal] load metadata for docker.io/library/golang:1.27.3-bookworm\n#3 ERROR: pull access denied\n------\n > [internal] load metadata:\n------\n" +
				"ERROR: failed to build: failed to solve: golang:1.27.3-bookworm: pull access denied\n",
			want: []string{"ERROR: failed to build: failed to solve: golang:1.27.3-bookworm: pull access denied"},
		},
		{
			name:   "a long line keeps both of its ends",
			log:    "#8 0.1 start-of-line " + strings.Repeat("y", 8192) + " end-of-line\n#8 ERROR: exit code: 1\nERROR: failed to solve\n",
			want:   []string{"start-of-line", "end-of-line", "ERROR: failed to solve"},
			reject: []string{strings.Repeat("y", 1024)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "build.log")
			if err := os.WriteFile(file, []byte(tc.log), 0600); err != nil {
				t.Fatal(err)
			}
			err := filesystemBuildError(context.Canceled, file)
			if !errors.Is(err, context.Canceled) || len(err.Error()) > 17000 || strings.Count(err.Error(), "\n") > 24 {
				t.Fatalf("unusable bounded diagnostic (%d bytes): %v", len(err.Error()), err)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("missing %q in:\n%v", want, err)
				}
			}
			for _, reject := range tc.reject {
				if strings.Contains(err.Error(), reject) {
					t.Errorf("kept %q in:\n%v", reject, err)
				}
			}
		})
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
	fallback := &FilesystemFallbackError{Reason: "Kernel filesystem protection requires Docker cgroupfs with cgroup v2", Err: &UncoveredSpellings{Paths: []string{"credentials.json"}}}
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
