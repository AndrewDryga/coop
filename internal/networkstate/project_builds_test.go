package networkstate

import (
	"path/filepath"
	"strings"
	"testing"
)

// A remembered build answers only for its tag and exactly the inputs it was built from. A tag holds
// one record, so newer inputs replace it; everything else — other inputs, another tag, another
// host's store — is a miss, and a miss builds.
func TestProjectBuildAnswersForOneTagAndExactlyItsInputs(t *testing.T) {
	s := openStore(t)
	tag, inputs, image := "coop-app-filtered:0123456789abcdef", strings.Repeat("a", 64), "sha256:"+strings.Repeat("1", 64)
	if got := s.ProjectBuild(tag, inputs); got != "" {
		t.Fatalf("a store that remembered nothing answered %q", got)
	}
	if err := s.RememberProjectBuild(tag, inputs, image); err != nil {
		t.Fatal(err)
	}
	if got := s.ProjectBuild(tag, inputs); got != image {
		t.Fatalf("remembered %q, want %q", got, image)
	}
	if got := s.ProjectBuild(tag, strings.Repeat("b", 64)); got != "" {
		t.Errorf("other inputs reused this build: %q", got)
	}
	if got := s.ProjectBuild("coop-other-filtered:0123456789abcdef", inputs); got != "" {
		t.Errorf("another tag reused this build: %q", got)
	}
	newer, rebuilt := strings.Repeat("c", 64), "sha256:"+strings.Repeat("2", 64)
	if err := s.RememberProjectBuild(tag, newer, rebuilt); err != nil {
		t.Fatal(err)
	}
	if got := s.ProjectBuild(tag, inputs); got != "" {
		t.Errorf("a replaced record still answered for its old inputs: %q", got)
	}
	if got := s.ProjectBuild(tag, newer); got != rebuilt {
		t.Errorf("the newer build = %q, want %q", got, rebuilt)
	}
	other, err := Open(filepath.Join(t.TempDir(), "network"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	mine, err := s.projectBuildID(tag)
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := other.projectBuildID(tag)
	if err != nil || mine == theirs {
		t.Fatalf("two stores named one record %q/%q (%v)", mine, theirs, err)
	}
}

// A damaged record is nothing remembered, and a record that could name no image is never written.
func TestProjectBuildTreatsADamagedRecordAsAMiss(t *testing.T) {
	tag, inputs, image := "coop-app-filtered:0123456789abcdef", strings.Repeat("a", 64), "sha256:"+strings.Repeat("1", 64)
	for name, damage := range map[string]string{
		"not json":        "not json at all",
		"empty object":    `{}`,
		"another version": `{"version":2,"tag":"` + tag + `","inputs":"` + inputs + `","image":"` + image + `"}`,
		"not an image id": `{"version":1,"tag":"` + tag + `","inputs":"` + inputs + `","image":"coop-box:latest"}`,
	} {
		t.Run(name, func(t *testing.T) {
			s := openStore(t)
			if err := s.RememberProjectBuild(tag, inputs, image); err != nil {
				t.Fatal(err)
			}
			id, err := s.projectBuildID(tag)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.publish(projectBuildRecordName(id), []byte(damage), true); err != nil {
				t.Fatal(err)
			}
			if got := s.ProjectBuild(tag, inputs); got != "" {
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
		if err := s.RememberProjectBuild(record[0], record[1], record[2]); err == nil {
			t.Errorf("%s: a record that names no build was written", name)
		}
	}
}
