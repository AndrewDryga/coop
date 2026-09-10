package networkstate

import (
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func imageFileFixture() (string, []string, map[string]ImageFile) {
	image := "sha256:" + strings.Repeat("1", 64)
	files := map[string]ImageFile{
		"/usr/local/bin/claude": {Mode: 0o755, Size: 1024, SHA256: strings.Repeat("a", 64)},
		"/usr/local/bin/node":   {Mode: 0o755, Size: 98765432, SHA256: strings.Repeat("b", 64)},
	}
	return image, slices.Sorted(maps.Keys(files)), files
}

// A remembered read answers only for the image id and the path set it was
// written for. Everything else — another image, one more pinned path, one fewer
// — is a miss, and a miss means the caller reads the image.
func TestImageFilesAnswerForOneImageAndExactlyItsPaths(t *testing.T) {
	s := openStore(t)
	image, paths, files := imageFileFixture()
	if got := s.ImageFileDigests(image, paths); got != nil {
		t.Fatalf("a store that remembered nothing answered %v", got)
	}
	if err := s.RememberImageFiles(image, paths, files); err != nil {
		t.Fatal(err)
	}
	if got := s.ImageFileDigests(image, paths); !reflect.DeepEqual(got, files) {
		t.Fatalf("remembered %v, want %v", got, files)
	}
	// The path set is a SET: the caller's order cannot change the answer.
	if got := s.ImageFileDigests(image, []string{paths[1], paths[0]}); !reflect.DeepEqual(got, files) {
		t.Errorf("a reordered path set missed its own record: %v", got)
	}
	for name, ask := range map[string][]string{
		"one more pinned path": append(slices.Clone(paths), "/usr/local/bin/codex"),
		"one fewer":            paths[:1],
	} {
		if got := s.ImageFileDigests(image, ask); got != nil {
			t.Errorf("%s was satisfied by an older record: %v", name, got)
		}
	}
	if got := s.ImageFileDigests("sha256:"+strings.Repeat("2", 64), paths); got != nil {
		t.Errorf("another image reused this record: %v", got)
	}
	// The record name is keyed with the owner key, so another host's store never
	// names — and therefore never reads or plants — this one's record.
	other, err := Open(filepath.Join(t.TempDir(), "network"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	mine, _, err := s.imageFilesID(image, paths)
	if err != nil {
		t.Fatal(err)
	}
	theirs, _, err := other.imageFilesID(image, paths)
	if err != nil || mine == theirs {
		t.Fatalf("two stores named one record %q/%q (%v)", mine, theirs, err)
	}
}

// A record that is unreadable, foreign or incomplete is a MISS. It must never
// become a partial answer: the caller would compare against digests nobody read
// out of the image it is about to run.
func TestImageFilesTreatADamagedRecordAsNothingRemembered(t *testing.T) {
	image, paths, files := imageFileFixture()
	id := func(s *Store) string {
		t.Helper()
		name, _, err := s.imageFilesID(image, paths)
		if err != nil {
			t.Fatal(err)
		}
		return imageFilesRecord(name)
	}
	for name, damage := range map[string]string{
		"not json":          "not json at all",
		"empty object":      `{}`,
		"another image":     `{"version":1,"image":"sha256:` + strings.Repeat("9", 64) + `","files":{}}`,
		"a path is missing": `{"version":1,"image":"` + image + `","files":{"/usr/local/bin/claude":{"mode":493,"size":1024,"sha256":"` + strings.Repeat("a", 64) + `"}}}`,
		"a digest is not a digest": `{"version":1,"image":"` + image + `","files":{"/usr/local/bin/claude":{"mode":493,"size":1024,"sha256":"nope"},` +
			`"/usr/local/bin/node":{"mode":493,"size":1,"sha256":"` + strings.Repeat("b", 64) + `"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			s := openStore(t)
			if err := s.RememberImageFiles(image, paths, files); err != nil {
				t.Fatal(err)
			}
			if err := s.publish(id(s), []byte(damage), true); err != nil {
				t.Fatal(err)
			}
			if got := s.ImageFileDigests(image, paths); got != nil {
				t.Fatalf("a damaged record answered %v", got)
			}
		})
	}
}

func TestImageFilesRefuseARecordThatProvesNothing(t *testing.T) {
	s := openStore(t)
	image, paths, files := imageFileFixture()
	short := maps.Clone(files)
	delete(short, paths[0])
	bad := maps.Clone(files)
	bad[paths[0]] = ImageFile{Mode: 0o755, Size: 1, SHA256: "not-a-digest"}
	for name, test := range map[string]struct {
		paths []string
		files map[string]ImageFile
	}{
		"no paths":          {nil, files},
		"a relative path":   {[]string{"usr/local/bin/claude"}, files},
		"a repeated path":   {[]string{paths[0], paths[0]}, files},
		"a missing digest":  {paths, short},
		"an invalid digest": {paths, bad},
	} {
		t.Run(name, func(t *testing.T) {
			if err := s.RememberImageFiles(image, test.paths, test.files); err == nil {
				t.Fatal("a record that proves nothing was published")
			}
			if got := s.ImageFileDigests(image, test.paths); got != nil {
				t.Fatalf("it answered anyway: %v", got)
			}
		})
	}
	many := make([]string, maxImageFilePaths+1)
	for i := range many {
		many[i] = "/usr/local/bin/tool" + strings.Repeat("x", i%8) + string(rune('a'+i%26)) + strings.Repeat("y", i/26)
	}
	if err := s.RememberImageFiles(image, many, files); err == nil {
		t.Error("an unbounded path set was published")
	}
}
