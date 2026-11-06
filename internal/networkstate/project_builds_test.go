package networkstate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"
)

func TestAutomaticProjectBuildRecordIsNotApproval(t *testing.T) {
	s := openStore(t)
	tag, inputs, image := "coop-app-filtered:0123456789abcdef", strings.Repeat("a", 64), "sha256:"+strings.Repeat("1", 64)
	id, err := s.projectBuildID(tag)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(projectBuildRecord{Version: 1, ID: id, Tag: tag, Inputs: inputs, Image: image})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.publish(projectBuildRecordName(id), data, true); err != nil {
		t.Fatal(err)
	}
	if got := s.ApprovedProjectBuild(tag, inputs); got != "" {
		t.Fatalf("legacy automatic-build cache granted approval: %q", got)
	}
	if err := s.ApproveProjectBuild(tag, inputs, image); err != nil {
		t.Fatal(err)
	}
	if got := s.ApprovedProjectBuild(tag, inputs); got != image {
		t.Fatalf("explicit recovery = %q", got)
	}
}

// A remembered build answers only for its tag and exactly the inputs it was built from. A tag holds
// one record, so newer inputs replace it; everything else — other inputs, another tag, another
// host's store — is a miss, and a miss builds.
func TestProjectBuildAnswersForOneTagAndExactlyItsInputs(t *testing.T) {
	s := openStore(t)
	tag, inputs, image := "coop-app-filtered:0123456789abcdef", strings.Repeat("a", 64), "sha256:"+strings.Repeat("1", 64)
	if got := s.ApprovedProjectBuild(tag, inputs); got != "" {
		t.Fatalf("a store that remembered nothing answered %q", got)
	}
	if err := s.ApproveProjectBuild(tag, inputs, image); err != nil {
		t.Fatal(err)
	}
	if got := s.ApprovedProjectBuild(tag, inputs); got != image {
		t.Fatalf("remembered %q, want %q", got, image)
	}
	if got := s.ApprovedProjectBuild(tag, strings.Repeat("b", 64)); got != "" {
		t.Errorf("other inputs reused this build: %q", got)
	}
	if got := s.ApprovedProjectBuild("coop-other-filtered:0123456789abcdef", inputs); got != "" {
		t.Errorf("another tag reused this build: %q", got)
	}
	newer, rebuilt := strings.Repeat("c", 64), "sha256:"+strings.Repeat("2", 64)
	if err := s.ApproveProjectBuild(tag, newer, rebuilt); err != nil {
		t.Fatal(err)
	}
	if got := s.ApprovedProjectBuild(tag, inputs); got != "" {
		t.Errorf("a replaced record still answered for its old inputs: %q", got)
	}
	if got := s.ApprovedProjectBuild(tag, newer); got != rebuilt {
		t.Errorf("the newer build = %q, want %q", got, rebuilt)
	}
}

// A damaged record is nothing remembered, and a record that could name no image is never written.
func TestProjectBuildTreatsADamagedRecordAsAMiss(t *testing.T) {
	tag, inputs, image := "coop-app-filtered:0123456789abcdef", strings.Repeat("a", 64), "sha256:"+strings.Repeat("1", 64)
	for name, damage := range map[string]string{
		"not json":        "not json at all",
		"empty object":    `{}`,
		"another version": `{"version":3,"tag":"` + tag + `","inputs":"` + inputs + `","image":"` + image + `"}`,
		"not an image id": `{"version":1,"tag":"` + tag + `","inputs":"` + inputs + `","image":"coop-box:latest"}`,
	} {
		t.Run(name, func(t *testing.T) {
			s := openStore(t)
			if err := s.ApproveProjectBuild(tag, inputs, image); err != nil {
				t.Fatal(err)
			}
			id, err := s.projectBuildID(tag)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.publish(projectBuildRecordName(id), []byte(damage), true); err != nil {
				t.Fatal(err)
			}
			if got := s.ApprovedProjectBuild(tag, inputs); got != "" {
				t.Fatalf("a damaged record answered %q", got)
			}
		})
	}
	s := openStore(t)
	for name, record := range map[string][3]string{
		"no tag":           {"", inputs, image},
		"no inputs digest": {tag, "short", image},
		"a tag as image":   {tag, inputs, "coop-box:latest"},
		"uppercase digest": {tag, inputs, "sha256:" + strings.Repeat("A", 64)},
	} {
		if err := s.ApproveProjectBuild(record[0], record[1], record[2]); err == nil {
			t.Errorf("%s: a record that names no build was written", name)
		}
	}
}

// A review keeps the files it covered, so a refusal can name what changed. The list explains and
// approves nothing: one too big to keep is dropped, and the review it came with still answers.
func TestProjectBuildReviewKeepsTheFilesItCovered(t *testing.T) {
	s := openStore(t)
	project, review := "/work/app", strings.Repeat("a", 64)
	if _, ok := s.ProjectBuildReviewedFiles(project); ok {
		t.Fatal("files were listed before any review")
	}
	files := map[string]string{"main.go": strings.Repeat("1", 32), ".agent/Dockerfile": strings.Repeat("2", 32)}
	if err := s.RecordProjectBuildReview(project, review, files); err != nil {
		t.Fatal(err)
	}
	if got, ok := s.ProjectBuildReviewedFiles(project); !ok || !maps.Equal(got, files) {
		t.Fatalf("reviewed files = %v, %v; want %v", got, ok, files)
	}
	huge := map[string]string{}
	for i := range 40000 {
		huge[fmt.Sprintf("portal/lib/module_%05d.ex", i)] = strings.Repeat("3", 32)
	}
	if err := s.RecordProjectBuildReview(project, review, huge); err != nil {
		t.Fatal(err)
	}
	if !s.ProjectBuildReviewed(project, review) {
		t.Fatal("a review whose file list was too big to keep did not answer")
	}
	if _, ok := s.ProjectBuildReviewedFiles(project); ok {
		t.Fatal("a file list too big to keep was kept, or an older one stayed")
	}
}

// A review answers for one project and exactly one digest. Anything else is a miss, and a miss waits
// for a person.
func TestProjectBuildReviewAnswersForOneProjectAndOneDigest(t *testing.T) {
	s := openStore(t)
	project, review, newer := "/work/app", strings.Repeat("a", 64), strings.Repeat("b", 64)
	if s.ProjectBuildReviewed(project, review) {
		t.Fatal("nothing was recorded yet")
	}
	if err := s.RecordProjectBuildReview(project, review, nil); err != nil {
		t.Fatal(err)
	}
	if !s.ProjectBuildReviewed(project, review) {
		t.Fatal("the recorded review did not answer")
	}
	if s.ProjectBuildReviewed(project, newer) || s.ProjectBuildReviewed("/work/other", review) {
		t.Fatal("a review answered for other files or another project")
	}
	if err := s.RecordProjectBuildReview(project, newer, nil); err != nil {
		t.Fatal(err)
	}
	if s.ProjectBuildReviewed(project, review) || !s.ProjectBuildReviewed(project, newer) {
		t.Fatal("a later review did not replace the earlier one")
	}
	for name, record := range map[string][2]string{
		"a relative project": {"work/app", review},
		"a short digest":     {project, "short"},
		"an uppercase one":   {project, strings.Repeat("A", 64)},
	} {
		if err := s.RecordProjectBuildReview(record[0], record[1], nil); err == nil {
			t.Errorf("%s: recorded", name)
		}
		if s.ProjectBuildReviewed(record[0], record[1]) {
			t.Errorf("%s: answered", name)
		}
	}
	id := projectBuildReviewID(project)
	for name, damage := range map[string]string{
		"not json":        "{",
		"another project": `{"version":1,"id":"` + id + `","project":"/work/other","review":"` + newer + `"}`,
		"another version": `{"version":2,"id":"` + id + `","project":"/work/app","review":"` + newer + `"}`,
		"another id":      `{"version":1,"id":"00","project":"/work/app","review":"` + newer + `"}`,
	} {
		if err := s.publish(projectBuildReviewName(id), []byte(damage), true); err != nil {
			t.Fatal(err)
		}
		if s.ProjectBuildReviewed(project, newer) {
			t.Errorf("%s: a damaged record answered", name)
		}
	}
}

func TestLockProjectBuildRunsOneBuildAtATime(t *testing.T) {
	s := openStore(t)
	const project = "/work/app"
	holding, release, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		finished <- s.LockProjectBuild(t.Context(), project, nil, func() error {
			close(holding)
			<-release
			return nil
		})
	}()
	<-holding

	// Another project does not wait.
	ran := false
	if err := s.LockProjectBuild(t.Context(), "/work/other", func() { t.Error("another project waited") }, func() error {
		ran = true
		return nil
	}); err != nil || !ran {
		t.Fatalf("another project: ran=%v, %v", ran, err)
	}

	// The same project says it is waiting, and stops waiting when cancelled.
	ctx, cancel := context.WithCancel(t.Context())
	said := make(chan struct{})
	err := s.LockProjectBuild(ctx, project, func() { close(said); cancel() }, func() error {
		t.Error("ran while another build held the lock")
		return nil
	})
	<-said
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled wait = %v", err)
	}

	// A waiter runs once the holder finishes, and says so exactly once.
	saidOnce, ranAfter := 0, false
	done := make(chan error, 1)
	go func() {
		done <- s.LockProjectBuild(t.Context(), project, func() { saidOnce++; close(release) }, func() error {
			ranAfter = true
			return nil
		})
	}()
	if err := <-done; err != nil || !ranAfter || saidOnce != 1 {
		t.Fatalf("a waiter: ran=%v, said %d times, %v", ranAfter, saidOnce, err)
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}

	// A build's own deadline is its failure, not a reason to take the lock again and run it twice.
	runs := 0
	err = s.LockProjectBuild(t.Context(), project, nil, func() error {
		runs++
		return context.DeadlineExceeded
	})
	if !errors.Is(err, context.DeadlineExceeded) || runs != 1 {
		t.Fatalf("a build that timed out: %v after %d runs", err, runs)
	}
}
