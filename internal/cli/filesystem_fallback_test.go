package cli

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/ui"
)

func TestFilesystemFallbackRefusalRetainsQualification(t *testing.T) {
	err := &box.FilesystemFallbackError{
		Reason: "Could not check Docker for kernel filesystem protection: permission denied",
		Err:    &box.UncoveredSpellings{Paths: []string{"credentials.json"}},
	}
	out := usageBlock(t, err)
	for _, want := range []string{"Coop cannot hide credentials.json", err.Reason, "another spelling", "coop approve"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in refusal:\n%s", want, out)
		}
	}
	if asUsage(err).ExitCode != 1 {
		t.Fatal("runtime refusal changed exit status")
	}
}

func TestFilesystemFallbackDiagnosticIsBoundedAndKeepsInnerUsage(t *testing.T) {
	inner := &ui.UsageError{
		Headline: "Cannot prepare files", Cause: "file is unreadable", ExitCode: 1,
		Choices: []string{"first", "second"}, Rows: [][2]string{{"Review:", "coop doctor"}},
	}
	before := *inner
	err := &box.FilesystemFallbackError{Reason: "\x1b[31mqualification failed\x1b[0m\n\r\x00" + strings.Repeat("é", 5000), Err: inner}
	got := asUsage(err)
	if !reflect.DeepEqual(*inner, before) || got == inner {
		t.Fatal("rendering mutated the inner usage error")
	}
	if got.Headline != inner.Headline || got.ExitCode != inner.ExitCode || !reflect.DeepEqual(got.Choices, inner.Choices) || !reflect.DeepEqual(got.Rows, inner.Rows) {
		t.Fatalf("lost inner usage: %+v", got)
	}
	if len(got.Cause) > 4200 || !utf8.ValidString(got.Cause) || strings.ContainsAny(got.Cause, "\x1b\r\x00") || !strings.HasPrefix(got.Cause, "Filesystem protection: qualification failed") || !strings.HasSuffix(got.Cause, inner.Cause) {
		t.Fatalf("unusable diagnostic: length %d, prefix %.100q", len(got.Cause), got.Cause)
	}
	if !errors.Is(err, inner) {
		t.Fatal("lost underlying error identity")
	}
}

func TestFilesystemFallbackGenericFailureKeepsBothCauses(t *testing.T) {
	err := &box.FilesystemFallbackError{Reason: "Kernel filesystem protection requires Docker cgroupfs with cgroup v2", Err: errors.New("remember hidden files: disk full")}
	if asUsage(err) != nil || !strings.Contains(err.Error(), err.Reason) || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("generic error lost a cause: %v", err)
	}
}

func TestFilesystemFallbackRetainsCleanupFailures(t *testing.T) {
	refusal := &box.UncoveredSpellings{Paths: []string{"credentials.json"}}
	fallback := &box.FilesystemFallbackError{Reason: "Kernel filesystem protection requires Docker cgroupfs with cgroup v2", Err: refusal}
	cleanup := errors.New("credential cleanup remains pending")
	for _, err := range []error{
		errors.Join(fallback, cleanup),
		fmt.Errorf("launch: %w", errors.Join(fallback, cleanup)),
		&box.FilesystemFallbackError{Reason: fallback.Reason, Err: errors.Join(refusal, cleanup)},
	} {
		var code int
		out := captureStderr(t, func() { code = reportExit(-1, err) })
		if code != 1 || !strings.Contains(out, fallback.Reason) || !strings.Contains(out, cleanup.Error()) || !strings.Contains(out, "credentials.json") {
			t.Fatalf("rendering lost joined causes: exit %d, %s", code, out)
		}
	}
	if out := usageBlock(t, fmt.Errorf("launch: %w", errors.Join(fallback))); !strings.Contains(out, fallback.Reason) || !strings.Contains(out, "coop approve") {
		t.Fatalf("single-cause wrapper lost rich rendering: %s", out)
	}
}
