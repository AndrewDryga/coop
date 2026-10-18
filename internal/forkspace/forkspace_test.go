package forkspace

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/wait"
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

func TestForkDiscoveryDistinguishesMissingAndBrokenRoots(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	if names, err := Names(repo); err != nil || len(names) != 0 {
		t.Fatalf("Names with missing root = %v, %v; want empty", names, err)
	}
	if err := os.WriteFile(Home(repo), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Names(repo); err == nil || !strings.Contains(err.Error(), Home(repo)) {
		t.Fatalf("Names with broken root error = %v", err)
	}
}

// A box can put a FIFO, or a link, where a folder of the forks tree belongs. Every listing and
// folder sync there fails at once with a plain error instead of waiting for a writer, which would
// freeze coop fork ls, stop and merge.
func TestForksTreeFoldersRefuseAPlantedFIFOOrLinkAtOnce(t *testing.T) {
	for _, planted := range []string{"fifo", "link"} {
		for _, at := range []string{"home", "state"} {
			t.Run(planted+" at "+at, func(t *testing.T) {
				repo := filepath.Join(t.TempDir(), "repo")
				if err := os.Mkdir(repo, 0o755); err != nil {
					t.Fatal(err)
				}
				path := Home(repo)
				if at == "state" {
					if err := os.Mkdir(Home(repo), 0o755); err != nil {
						t.Fatal(err)
					}
					path = StateDir(repo)
				}
				if planted == "fifo" {
					if err := syscall.Mkfifo(path, 0o600); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Symlink(t.TempDir(), path); err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() {
					_, err := LifecycleNames(repo)
					if at == "state" && err == nil {
						var root *os.Root
						if root, err = OpenStateRoot(repo, "", false); err == nil {
							err = root.Close()
						}
					}
					done <- err
				}()
				select {
				case err := <-done:
					if err == nil {
						t.Fatalf("the forks tree with a %s at its %s folder was read as fine", planted, at)
					}
				case <-time.After(wait.Deadline):
					t.Fatalf("reading the forks tree waited on a %s at its %s folder", planted, at)
				}
			})
		}
	}
}
