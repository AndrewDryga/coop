package box

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
