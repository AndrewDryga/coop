package gitdiff

import (
	"reflect"
	"testing"
)

func TestExactDiffPaths(t *testing.T) {
	data := "M\x00日本語/AGENTS.md\x00R100\x00old\nname\x00new\tname \x00C75\x00source\x00quote\"back\\slash\x00D\x00gone\x00"
	want := []Change{
		{Status: "M", Path: "日本語/AGENTS.md"},
		{Status: "R100", OldPath: "old\nname", Path: "new\tname "},
		{Status: "C75", OldPath: "source", Path: "quote\"back\\slash"},
		{Status: "D", Path: "gone"},
	}
	got, err := NameStatus(data)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("changes = %+v, %v; want %+v", got, err, want)
	}
	counts, err := Numstat("1\t2\t日本語/AGENTS.md\x003\t4\t\x00old\nname\x00new\tname \x00-\t-\timage.png\x005\t0\ttab\tname\x00")
	wantCounts := map[string][2]int{"日本語/AGENTS.md": {1, 2}, "new\tname ": {3, 4}, "image.png": {-1, -1}, "tab\tname": {5, 0}}
	if err != nil || !reflect.DeepEqual(counts, wantCounts) {
		t.Fatalf("counts = %v, %v; want %v", counts, err, wantCounts)
	}
	if changes, err := NameStatus(""); changes != nil || err != nil {
		t.Fatalf("empty changes = %v, %v", changes, err)
	}
	if counts, err := Numstat(""); len(counts) != 0 || err != nil {
		t.Fatalf("empty counts = %v, %v", counts, err)
	}
}

func TestDiffRejectsIncompleteRecords(t *testing.T) {
	for _, data := range []string{"M", "M\x00", "M\x00path", "M\x00\x00", "R100\x00old\x00", "C80\x00old\x00new", "Q\x00path\x00", "Mbad\x00path\x00", "A10\x00path\x00", "R101\x00old\x00new\x00", "M\x00path\x00\x00"} {
		if got, err := NameStatus(data); err == nil || got != nil {
			t.Errorf("NameStatus(%q) = %v, %v; want refusal without partial result", data, got, err)
		}
	}
	for _, data := range []string{"1\t2\tpath", "1\t2\x00", "1\t2\t\x00", "1\t2\t\x00old\x00", "1\t2\t\x00old\x00\x00", "-1\t0\tpath\x00", "-\t0\tpath\x00", "0\tbad\tpath\x00"} {
		if got, err := Numstat(data); err == nil || got != nil {
			t.Errorf("Numstat(%q) = %v, %v; want refusal without partial result", data, got, err)
		}
	}
}

// AddedLines reads only the new side of each hunk header: what a pure deletion removes is no
// added line, a header without a count adds one line, and anything else that starts like a header
// is refused rather than read as no change.
func TestAddedLines(t *testing.T) {
	patch := "diff --git a/f b/f\nindex 1..2 100644\n--- a/f\n+++ b/f\n" +
		"@@ -3 +3 @@ func main() {\n-old\n+new\n" +
		"@@ -10,2 +9,0 @@\n-gone\n-gone\n" +
		"@@ -20,0 +20,3 @@\n+a\n+b\n+c\n"
	added, err := AddedLines(patch)
	if err != nil || !reflect.DeepEqual(added, LineRanges{{3, 3}, {20, 22}}) {
		t.Fatalf("AddedLines = %v, %v", added, err)
	}
	for n, want := range map[int]bool{2: false, 3: true, 9: false, 19: false, 20: true, 22: true, 23: false} {
		if added.Contains(n) != want {
			t.Errorf("Contains(%d) = %v, want %v", n, !want, want)
		}
	}
	if added, err := AddedLines(""); added != nil || err != nil {
		t.Fatalf("empty patch = %v, %v", added, err)
	}
	if added, err := AddedLines("@@ -1 +x @@\n"); err == nil || added != nil {
		t.Fatalf("malformed header = %v, %v; want a refusal", added, err)
	}
}
