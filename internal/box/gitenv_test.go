package box

import (
	"strings"
	"testing"
)

func TestBuildGitConfig(t *testing.T) {
	// Signing is always disabled — the box holds no GPG/SSH key.
	if !strings.Contains(buildGitConfig("", ""), "gpgsign = false") {
		t.Error("buildGitConfig must always disable gpgsign")
	}
	// Identity is included when present.
	gc := buildGitConfig("Ada Lovelace", "ada@example.com")
	if !strings.Contains(gc, "name = Ada Lovelace") || !strings.Contains(gc, "email = ada@example.com") {
		t.Errorf("buildGitConfig identity missing:\n%s", gc)
	}
	// No [user] block when there is no identity to write.
	if strings.Contains(buildGitConfig("", ""), "[user]") {
		t.Error("buildGitConfig should omit [user] when no identity is set")
	}
}
