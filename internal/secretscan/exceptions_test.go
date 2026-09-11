package secretscan

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// .coopsecretsignore is a file a person edits by hand, so every way of getting it wrong has to
// come back as the line to fix — and anything the parser is unsure about must stop the check
// rather than silently excuse a finding nobody reviewed.

const testID = "fp-v1:44c6d2d984741e8b10626640fd934d4558095dc8f23e7b29db086994517d6348"

// otherID is a second, unmistakably valid id — built by the real function so the fixture cannot
// drift from the format the parser accepts.
var otherID = Fingerprint("fixtures/login.json", DetectorAssignedHighEntro, "a reviewed public value")

func TestParseExceptionsAcceptsThePrintedBlock(t *testing.T) {
	// Exactly what a scan prints, with the reasons filled in: blank lines, the labelling comment,
	// and one entry per finding.
	content := "# config/client.go — OpenAI API key\n" + testID + " # Public test fixture\n\n" +
		"# fixtures/login.json — credential value\n" + otherID + " # Deliberately invalid test value\n"
	got, err := ParseExceptions(content)
	if err != nil {
		t.Fatalf("the block a scan printed was refused: %v", err)
	}
	if got.Len() != 2 {
		t.Errorf("parsed %d entries, want 2", got.Len())
	}
	if !got.Excuses(testID) {
		t.Error("a reviewed finding was not excused")
	}
	if got.Excuses("fp-v1:" + strings.Repeat("0", 64)) {
		t.Error("an id nobody wrote down was excused")
	}
	// A repeat of the same decision is not a conflict.
	if _, err := ParseExceptions(testID + " # same\n" + testID + " # same\n"); err != nil {
		t.Errorf("an identical repeat was refused: %v", err)
	}
}

func TestParseExceptionsRejections(t *testing.T) {
	for name, tc := range map[string]struct {
		content string
		line    int
		problem string
	}{
		"a glob":            {"config/*.go # they are all fixtures\n", 1, "finding ID"},
		"a bare path":       {"config/client.go # a fixture\n", 1, "finding ID"},
		"a rule name":       {"openai_api_key # too noisy\n", 1, "finding ID"},
		"an unknown format": {"fp-v9:abc # from the future\n", 1, "fp-v9"},
		"a truncated id":    {"fp-v1:abc # too short\n", 1, "finding ID"},
		"no reason":         {testID + "\n", 1, "reason after #"},
		"an empty reason":   {testID + " #   \n", 1, "reason after #"},
		"the placeholder":   {testID + " # <reason>\n", 1, "placeholder"},
		"a conflict":        {testID + " # reviewed\n" + testID + " # actually not\n", 2, "different reason"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseExceptions(tc.content)
			var bad *ExceptionsError
			if !errors.As(err, &bad) {
				t.Fatalf("%s was accepted (err=%v)", name, err)
			}
			if bad.Line != tc.line {
				t.Errorf("reported line %d, want %d", bad.Line, tc.line)
			}
			if !strings.Contains(bad.Error(), tc.problem) {
				t.Errorf("problem = %q, want it to mention %q", bad.Error(), tc.problem)
			}
		})
	}
}

// An entry that matches nothing today is a record of a decision, not a lie: it is kept, it is
// never rewritten, and it does not make the file invalid.
func TestStaleEntriesAreInertNotErrors(t *testing.T) {
	content := "# a finding that has since been fixed\n" + testID + " # Reviewed in 2026-01\n"
	got, err := ParseExceptions(content)
	if err != nil {
		t.Fatalf("a stale entry was refused: %v", err)
	}
	if got.Len() != 1 {
		t.Errorf("a stale entry was dropped: %d entries", got.Len())
	}
}

func TestLoadExceptions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ExceptionsFile)

	// A missing file is the normal case: most projects never need one.
	got, err := LoadExceptions(path)
	if err != nil || got.Len() != 0 {
		t.Fatalf("missing file = (%v, %v), want no exceptions and no error", got, err)
	}

	if err := os.WriteFile(path, []byte(testID+" # Public test fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = LoadExceptions(path)
	if err != nil || !got.Excuses(testID) {
		t.Fatalf("readable file = (%v, %v), want the entry", got, err)
	}

	// Unreadable is NOT "no exceptions": coop cannot tell what the person decided, so it stops.
	if os.Geteuid() != 0 {
		if err := os.Chmod(path, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
		if _, err := LoadExceptions(path); err == nil {
			t.Error("an unreadable exception file was treated as no exceptions")
		}
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Neither is a directory, or anything else that is not a regular file.
	notRegular := filepath.Join(t.TempDir(), ExceptionsFile)
	if err := os.Mkdir(notRegular, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadExceptions(notRegular); err == nil {
		t.Error("a directory was accepted as an exception file")
	}

	// Excessive input is refused rather than parsed: the file is a hand-edited list.
	huge := filepath.Join(t.TempDir(), ExceptionsFile)
	if err := os.WriteFile(huge, []byte(strings.Repeat("# padding\n", maxExceptionsBytes/5)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadExceptions(huge); err == nil {
		t.Error("an oversized exception file was parsed")
	}
}

// A nil Exceptions is the missing-file case and must behave like an empty one everywhere.
func TestNilExceptionsExcuseNothing(t *testing.T) {
	var none *Exceptions
	if none.Len() != 0 || none.Excuses(testID) {
		t.Error("a missing exception file excused a finding")
	}
	if (&Exceptions{}).Excuses("") {
		t.Error("an empty fingerprint was excused")
	}
}
