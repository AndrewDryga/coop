package cli

import (
	"strings"
	"testing"
)

// Pin documented spellings. Actual parser acceptance/refusal has its own owners.
func TestFamilyHelpDocumentsSupportedSpellings(t *testing.T) {
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
