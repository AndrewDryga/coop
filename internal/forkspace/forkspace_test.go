package forkspace

import (
	"slices"
	"testing"
)

func TestForkWorkspace(t *testing.T) {
	repo := "/home/me/proj"
	if got, want := Home(repo), "/home/me/proj-forks"; got != want {
		t.Errorf("Home = %q, want %q", got, want)
	}
	if got, want := Workspace(repo, "perf"), "/home/me/proj-forks/perf"; got != want {
		t.Errorf("Workspace = %q, want %q", got, want)
	}
}

func TestValidForkName(t *testing.T) {
	for _, n := range []string{"perf", "deps", "fix-1", "fix_2", "a.b"} {
		if !ValidName(n) {
			t.Errorf("ValidName(%q) = false, want true", n)
		}
	}
	for _, n := range []string{"", "ls", "review", "merge", "rm", "open", "path", "acp", "a/b", `a\b`, "..", ".", ".hidden", "a..b", "a.", "a.lock", "-x",
		"my fork", "a\tb", "a\nb", "a=b", "a;b", "a$(id)", "a`id`", "a|b", "a&b", `a'b`, `a"b`} {
		if ValidName(n) {
			t.Errorf("ValidName(%q) = true, want false", n)
		}
	}
}

// Every fork subcommand is a reserved name (can't be a fork), so no fork shadows a subcommand;
// ordinary names are not reserved.
func TestForkReserved(t *testing.T) {
	for _, r := range []string{"ls", "review", "merge", "rm", "open", "logs", "stop", "path", "acp"} {
		if !Reserved(r) {
			t.Errorf("Reserved(%q) = false, want true", r)
		}
		if ValidName(r) {
			t.Errorf("ValidName(%q) = true, want false (a fork can't be named a reserved verb)", r)
		}
	}
	// v3 dropped the list/remove aliases, so they are ordinary (allowed) fork names now.
	for _, ok := range []string{"api", "web", "feature-x", "wip2", "list", "remove", "watch"} {
		if Reserved(ok) {
			t.Errorf("Reserved(%q) = true, want false", ok)
		}
		if !ValidName(ok) {
			t.Errorf("ValidName(%q) = false, want true", ok)
		}
	}
}

// VerbList is the did-you-mean source for a mistyped `coop fork <verb>`, so its order and content
// are part of the contract, not an implementation detail.
func TestVerbList(t *testing.T) {
	if got := VerbList(); !slices.Equal(got, []string{"acp", "logs", "ls", "merge", "open", "path", "review", "rm", "stop"}) {
		t.Errorf("VerbList = %v", got)
	}
}
