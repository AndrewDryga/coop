package cli

import (
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/contextc"
)

// `coop context` answers "which instructions apply to this work". These pin the exact blocks: one
// selected path, several, and each of the empty states.
func TestContextReport(t *testing.T) {
	sel := []contextc.Selected{
		{File: "AGENTS.md", Reason: "canonical"},
		{File: ".agent/kb/web.md", Reason: "route web/** → web/login.ts"},
	}

	got := captureStdout(t, func() { contextReport([]string{"web/login.ts"}, sel) })
	want := "Instructions for web/login.ts\n\n  AGENTS.md\n    Shared agent instructions\n\n" +
		"  .agent/kb/web.md\n    Matches web/**\n\n  2 files selected\n"
	if got != want {
		t.Errorf("one-path report =\n%q\nwant\n%q", got, want)
	}

	// With several paths, the route line says WHICH path matched — otherwise the reader has to
	// guess which of them pulled the file in.
	got = captureStdout(t, func() { contextReport([]string{"web/login.ts", "web/session.ts"}, sel) })
	want = "Instructions for selected work\n  web/login.ts\n  web/session.ts\n\n  AGENTS.md\n" +
		"    Shared agent instructions\n\n  .agent/kb/web.md\n    web/login.ts matches web/**\n\n  2 files selected\n"
	if got != want {
		t.Errorf("multi-path report =\n%q\nwant\n%q", got, want)
	}

	// No selected work: there are no routes to explain, so the shared files ARE the answer.
	got = captureStdout(t, func() { contextReport(nil, sel[:1]) })
	want = "Shared agent instructions\n\n  AGENTS.md\n\n  Select work: coop context <path>\n"
	if got != want {
		t.Errorf("no-scope report =\n%q\nwant\n%q", got, want)
	}

	got = captureStdout(t, func() { contextReport([]string{"web/login.ts"}, nil) })
	if got != "No instruction files found for this selection.\n" {
		t.Errorf("no-match report = %q", got)
	}

	got = captureStdout(t, func() { contextReport(nil, nil) })
	want = "No shared agent instructions found.\n\n  Select work: coop context <path>\n"
	if got != want {
		t.Errorf("empty report =\n%q\nwant\n%q", got, want)
	}
}

// The human wording is DERIVED from the machine reason; the reason itself is the --json contract
// and must keep its exact spelling.
func TestContextReasonsStayMachineReadable(t *testing.T) {
	if got := selectionReason("canonical", false); got != "Shared agent instructions" {
		t.Errorf("canonical reason = %q", got)
	}
	if got := selectionReason("route web/** → web/a.ts", false); got != "Matches web/**" {
		t.Errorf("single-path route reason = %q", got)
	}
	if got := selectionReason("route web/** → web/a.ts", true); got != "web/a.ts matches web/**" {
		t.Errorf("multi-path route reason = %q", got)
	}
	// An unknown reason is passed through rather than rewritten into a claim.
	if got := selectionReason("something else", false); got != "something else" {
		t.Errorf("unknown reason = %q", got)
	}
}

// A route that names a missing file is a configuration problem, stated as one — never an empty
// selection that reads like "nothing applies".
func TestContextFailureCause(t *testing.T) {
	got := contextFailureCause(errString("context: .agent/kb/web.md is listed in a context route but does not exist"))
	if got != ".agent/kb/web.md is listed in a context route but does not exist." {
		t.Errorf("cause = %q", got)
	}
	if !strings.HasSuffix(contextFailureCause(errString("context: could not read x")), ".") {
		t.Error("a cause reads as a sentence")
	}
}

type errString string

func (e errString) Error() string { return string(e) }
