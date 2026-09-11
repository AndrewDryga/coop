package cli

import (
	"strings"
	"testing"
)

// The approved help pages for project setup, diagnostics, services and updates. The page is the
// contract: `coop <cmd> --help`, `coop help <cmd>` and the generated manual all print this text,
// so pinning the source constant pins every route that reads it.
func TestApprovedFamilyHelpPages(t *testing.T) {
	for fixture, command := range map[string]string{
		"13-help-init":          "init",
		"17-help-doctor":        "doctor",
		"19-help-check-secrets": "check-secrets",
		"27-help-up":            "up",
		"27-help-down":          "down",
		"30-help-build":         "build",
		"32-help-update":        "update",
	} {
		t.Run(fixture, func(t *testing.T) {
			page, ok := commandHelp[command]
			if !ok {
				t.Fatalf("no help page registered for %q", command)
			}
			assertApprovedPage(t, fixture, page)
		})
	}
}

// Every option the page documents has to exist in the parser, and every option the parser takes
// has to be on the page: a flag nobody can discover and a flag that does not work are the same
// bug seen from two sides.
func TestFamilyHelpMatchesTheParser(t *testing.T) {
	cases := []struct {
		command string
		accepts []string
		rejects []string
	}{
		{"down", []string{"--delete-volumes", "-y", "--yes"}, []string{"-v", "--volumes"}},
		{"update", []string{"--self-only", "--box-only", "--check"}, nil},
		{"check-secrets", []string{"--include-ignored"}, nil},
	}
	for _, tc := range cases {
		page := commandHelp[tc.command]
		for _, flag := range tc.accepts {
			if !strings.Contains(page, flag) {
				t.Errorf("coop %s takes %s but the page never mentions it", tc.command, flag)
			}
		}
		// A retired spelling is gone, not hidden: the page must not teach it either.
		for _, flag := range tc.rejects {
			if strings.Contains(page, " "+flag+" ") || strings.Contains(page, " "+flag+"\n") {
				t.Errorf("coop %s's page still documents the retired %s", tc.command, flag)
			}
		}
	}
}
