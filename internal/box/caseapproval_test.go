package box

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

func TestCaseExposureApprovalPersistsOnlyReviewedPaths(t *testing.T) {
	pinHome(t)
	t.Setenv(ServiceStateRootEnv, t.TempDir())
	repo := t.TempDir()
	writeRepoFile(t, filepath.Join(repo, "credentials.json"), "inert")
	if !foldsCase(filepath.Join(repo, "credentials.json")) {
		t.Skip("requires a disk that folds case")
	}
	review, err := ReviewCaseExposure(t.Context(), nil, repo)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(review.Pending, []string{"credentials.json"}) {
		t.Fatal(review.Pending)
	}
	// Approval must not incorporate files that appeared while the user read it.
	writeRepoFile(t, filepath.Join(repo, "production-secret.pem"), "inert")
	if err := review.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	pending := UnapprovedCaseExposures(repo, []string{"credentials.json", "production-secret.pem", ".coopignore"})
	if !slices.Equal(pending, []string{"production-secret.pem", ".coopignore"}) {
		t.Fatal(pending)
	}
	_, _, err = launchMounts(&config.Config{}, repo, "/workspace")
	var uncovered *UncoveredSpellings
	if !errors.As(err, &uncovered) || !slices.Equal(uncovered.Paths, []string{"production-secret.pem"}) {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(repo, "production-secret.pem")); err != nil {
		t.Fatal(err)
	}
	mounts, exposed, err := launchMounts(&config.Config{}, repo, "/workspace")
	if err != nil || !slices.Equal(exposed, []string{"credentials.json"}) || find(mounts, "/workspace/credentials.json") == nil {
		t.Fatalf("launch: %v %v", exposed, err)
	}
	again, err := ReviewCaseExposure(t.Context(), nil, repo)
	if err != nil || len(again.Pending) != 0 {
		t.Fatalf("repeat: %+v %v", again, err)
	}
	other := t.TempDir()
	marker, err := os.ReadFile(filepath.Join(repo, serviceApprovalMarker))
	if err != nil {
		t.Fatal(err)
	}
	writeRepoFile(t, filepath.Join(other, serviceApprovalMarker), string(marker))
	if len(UnapprovedCaseExposures(other, []string{"credentials.json"})) != 1 {
		t.Fatal("copied marker inherited approval")
	}
	// Replacing the marker at the original path also loses authority.
	if err := os.Remove(filepath.Join(repo, serviceApprovalMarker)); err != nil {
		t.Fatal(err)
	}
	writeRepoFile(t, filepath.Join(repo, serviceApprovalMarker), string(marker))
	if len(UnapprovedCaseExposures(repo, []string{"credentials.json"})) != 1 {
		t.Fatal("replaced marker inherited approval")
	}
}

func TestCaseExposureApprovalRejectsCancellationAndCorruptState(t *testing.T) {
	pinHome(t)
	t.Setenv(ServiceStateRootEnv, t.TempDir())
	repo := t.TempDir()
	anchor, err := serviceApprovalAnchor(repo, true)
	if err != nil {
		t.Fatal(err)
	}
	review := &CaseExposureReview{Paths: []string{"credentials.json"}, Pending: []string{"credentials.json"}, repo: repo, anchor: anchor}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := review.Commit(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(UnapprovedCaseExposures(repo, review.Paths)) != 1 {
		t.Fatal("cancelled approval persisted")
	}
	if err := review.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(UnapprovedCaseExposures(repo, review.Paths)) != 0 {
		t.Fatal("approval missing")
	}
	file, _ := caseExposurePath(anchor)
	if err := os.WriteFile(file, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if len(UnapprovedCaseExposures(repo, review.Paths)) != 1 {
		t.Fatal("corrupt approval accepted")
	}
}

func TestCaseExposureApprovalIncludesServiceOnlyPaths(t *testing.T) {
	pinHome(t)
	t.Setenv(ServiceStateRootEnv, t.TempDir())
	repo := t.TempDir()
	writeRepoFile(t, filepath.Join(repo, "secrets.pem"), "inert")
	if !foldsCase(filepath.Join(repo, "secrets.pem")) {
		t.Skip("requires a disk that folds case")
	}
	compose := "services:\n  web:\n    image: nginx:1\n    volumes:\n"
	for i := 0; i < 9; i++ {
		compose += fmt.Sprintf("      - ../:/app%d:ro\n", i)
	}
	writeRepoFile(t, filepath.Join(repo, ".agent/compose.yml"), compose)
	if _, err := ComputeMounts(repo, Workdir(nil, repo)); err != nil {
		t.Fatal("fixture must fit agent mount budget", err)
	}
	review, err := ReviewCaseExposure(t.Context(), nil, repo)
	if err != nil || !slices.Equal(review.Paths, []string{"secrets.pem"}) {
		t.Fatalf("service exposure: %+v %v", review, err)
	}
	if err := review.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(UnapprovedCaseExposures(repo, review.Paths)) != 0 {
		t.Fatal("service path not approved")
	}
}
