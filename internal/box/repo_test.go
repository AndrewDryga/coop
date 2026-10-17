package box

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

// ComposeProject is per-workspace: two checkouts with the SAME basename at DIFFERENT paths share an
// image tag but get DISTINCT compose projects (the clone/fork collision fix), and it's stable for a
// path so a workspace's sidecar volumes persist across runs.
func TestComposeProject(t *testing.T) {
	p1 := filepath.Join(t.TempDir(), "myrepo")
	p2 := filepath.Join(t.TempDir(), "myrepo")
	for _, p := range []string{p1, p2} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if ServicesProject(p1) != ServicesProject(p2) {
		t.Fatal("same basename must share the image tag")
	}
	if ComposeProject(p1) == ComposeProject(p2) {
		t.Errorf("same basename at different paths must get DISTINCT compose projects, both %q", ComposeProject(p1))
	}
	if got := ComposeProject(p1); got != ComposeProject(p1) {
		t.Error("compose project must be stable for a path")
	}
	if !strings.HasPrefix(ComposeProject(p1), ServicesProject(p1)+"-") {
		t.Errorf("compose project should be <image-tag>-<hash>, got %q", ComposeProject(p1))
	}
}

func TestComposeProjectForSeparatesLogicalOwnersWithoutRenamingDevelopment(t *testing.T) {
	repo := t.TempDir()
	if got := ComposeProjectFor(repo, ""); got != ComposeProject(repo) {
		t.Fatalf("development project changed from %q to %q", ComposeProject(repo), got)
	}
	first := ComposeProjectFor(repo, "loop-one")
	if first == ComposeProject(repo) || first == ComposeProjectFor(repo, "loop-two") || first != ComposeProjectFor(repo, "loop-one") {
		t.Fatalf("logical owner project names are not stable and distinct: dev=%q first=%q second=%q", ComposeProject(repo), first, ComposeProjectFor(repo, "loop-two"))
	}
}

func TestServicesProject(t *testing.T) {
	cases := map[string]string{
		"/tmp/My_Repo.Name": "coop-my_reponame",
		"/a/b/agent":        "coop-agent",
		"/x/Project 99!":    "coop-project99",
	}
	for in, want := range cases {
		if got := ServicesProject(in); got != want {
			t.Errorf("ServicesProject(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestImageForRepo(t *testing.T) {
	dir := t.TempDir()
	if got := ImageForRepo(dir, "coop-box", ""); got != "coop-box" {
		t.Errorf("no .agent/Dockerfile -> %q, want coop-box", got)
	}
	if got := ImageForRepo(dir, "coop-box", "custom"); got != "custom" {
		t.Errorf("override -> %q, want custom", got)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, ".agent", "Dockerfile"), []byte("FROM scratch"), 0o644)
	if want, got := boxAgentIdentity().projectImage(ServicesProject(dir)), ImageForRepo(dir, "coop-box", ""); got != want {
		t.Errorf(".agent/Dockerfile -> %q, want %q", got, want)
	}
}

func TestComposeFile(t *testing.T) {
	dir := t.TempDir()
	if ComposeFile(dir, dir) != "" {
		t.Error("no compose file should yield empty string")
	}
	f := filepath.Join(dir, filepath.FromSlash(project.DefaultCompose))
	os.MkdirAll(filepath.Dir(f), 0o755)
	os.WriteFile(f, []byte("services: {}"), 0o644)
	if ComposeFile(dir, dir) != f {
		t.Errorf("ComposeFile = %q, want %q", ComposeFile(dir, dir), f)
	}
	// A root compose.agent.yml is NOT picked up — only .agent/compose.yml is.
	os.Remove(f)
	os.WriteFile(filepath.Join(dir, "compose.agent.yml"), []byte("services: {}"), 0o644)
	if ComposeFile(dir, dir) != "" {
		t.Error("the retired root compose.agent.yml must not be recognized")
	}
	os.WriteFile(f, []byte("services: {}"), 0o644) // restore .agent/compose.yml for the zero-byte check
	// A zero-byte file counts as none — it declares no services, so `compose up` would only error.
	os.WriteFile(f, nil, 0o644)
	if ComposeFile(dir, dir) != "" {
		t.Error("an empty compose file should count as no compose file")
	}

	// The config/runtime split: the relative PATH comes from the POLICY repo, the FILE from the
	// workspace at that path — so a fork uses the parent's committed compose LOCATION but its own file.
	policy := t.TempDir()
	os.MkdirAll(filepath.Join(policy, ".agent"), 0o755)
	os.WriteFile(filepath.Join(policy, filepath.FromSlash(project.File)), []byte("box:\n  compose: build/svc.yml\n"), 0o644)
	ws := t.TempDir()
	wf := filepath.Join(ws, "build", "svc.yml")
	os.MkdirAll(filepath.Dir(wf), 0o755)
	os.WriteFile(wf, []byte("services: {}"), 0o644)
	if got := ComposeFile(ws, policy); got != wf {
		t.Errorf("path must come from policy repo, file from workspace: got %q, want %q", got, wf)
	}
}

func realPath(t *testing.T, path string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// The repository is the nearest folder holding .git, whatever its config says. git itself follows
// a core.worktree a box planted, naming the folder above, a sibling or the home folder, so each
// would have become the next launch's workspace.
func TestResolveRepoIgnoresAPlantedWorktree(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sibling := t.TempDir()
	for _, tc := range []struct {
		name   string
		target func(repo string) string
	}{
		{"the folder above", filepath.Dir},
		{"a sibling", func(string) string { return sibling }},
		{"the home folder", func(string) string { return home }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, git := gitrepo.New(t)
			sub := filepath.Join(repo, "internal", "pkg")
			mkdirs(t, sub)
			git("config", "core.worktree", tc.target(repo))
			cmd := exec.Command("git", "rev-parse", "--show-toplevel")
			cmd.Dir, cmd.Env = repo, append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
			if out, err := cmd.Output(); err != nil || strings.TrimSpace(string(out)) == realPath(t, repo) {
				t.Fatalf("git rev-parse = %q, %v; the planted worktree should have moved it", out, err)
			}
			for _, dir := range []string{repo, sub} {
				t.Chdir(dir)
				if got, err := ResolveRepo(""); err != nil || got != realPath(t, repo) {
					t.Fatalf("ResolveRepo from %s = %q, %v; want %s", dir, got, err, realPath(t, repo))
				}
			}
		})
	}
}

// A linked worktree and a submodule keep .git as a file and resolve to their own checkout, the
// nearest .git winning over the repository around it; outside any repository the working directory
// is the project, and FindRepo says there is none; an explicit override names its own folder.
func TestResolveRepoFindsLinkedCheckoutsAndFallsBack(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo, git := gitrepo.NewCommitted(t)
	linked := filepath.Join(t.TempDir(), "linked")
	git("worktree", "add", "-q", linked)
	submodule := filepath.Join(repo, "vendor", "lib")
	mkdirs(t, filepath.Join(linked, "sub"), filepath.Join(submodule, "src"), filepath.Join(repo, ".git", "modules", "lib"))
	if err := os.WriteFile(filepath.Join(submodule, ".git"), []byte("gitdir: ../../.git/modules/lib\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for dir, want := range map[string]string{filepath.Join(linked, "sub"): linked, filepath.Join(submodule, "src"): submodule} {
		t.Chdir(dir)
		if got, err := ResolveRepo(""); err != nil || got != realPath(t, want) {
			t.Fatalf("ResolveRepo from %s = %q, %v; want %s", dir, got, err, realPath(t, want))
		}
	}
	plain := t.TempDir()
	t.Chdir(plain)
	if got, err := ResolveRepo(""); err != nil || got != realPath(t, plain) {
		t.Fatalf("ResolveRepo outside a repository = %q, %v", got, err)
	}
	if got, ok, err := FindRepo(); err != nil || ok {
		t.Fatalf("FindRepo outside a repository = %q, %v, %v", got, ok, err)
	}
	if got, err := ResolveRepo(linked); err != nil || got != realPath(t, linked) {
		t.Fatalf("ResolveRepo(override) = %q, %v", got, err)
	}
}

// A box can make a folder of the project a repository by planting a .git in it, a folder or a
// gitfile, and a command run from inside then takes that folder for the project. The root
// .coopignore still hides what it names there, an exact path and a wildcard alike: the launch
// decoys both, and a repository file linking to one is not copied out. A real submodule resolves
// to itself as before and keeps the same rules.
func TestNestedRepositoryKeepsTheRulesAboveIt(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	module, _ := gitrepo.NewCommitted(t)
	for _, tc := range []struct {
		name string
		nest func(t *testing.T, repo string, git func(...string))
	}{
		{"a planted .git folder", func(t *testing.T, repo string, _ func(...string)) {
			mkdirs(t, filepath.Join(repo, "sub", ".git"))
		}},
		{"a planted gitfile", func(t *testing.T, repo string, _ func(...string)) {
			mkdirs(t, filepath.Join(repo, "sub"))
			if err := os.WriteFile(filepath.Join(repo, "sub", ".git"), []byte("gitdir: ../.git\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"a real submodule", func(_ *testing.T, _ string, git func(...string)) {
			git("-c", "protocol.file.allow=always", "submodule", "add", "-q", module, "sub")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, git := gitrepo.New(t)
			tc.nest(t, repo, git)
			sub := filepath.Join(repo, "sub")
			for path, body := range map[string]string{
				filepath.Join(repo, CoopIgnoreFile):      "sub/secret.yml\nsub/config/*.yml\n",
				filepath.Join(sub, "secret.yml"):         "token: hunter2\n",
				filepath.Join(sub, "config", "prod.yml"): "token: hunter2\n",
				filepath.Join(sub, "config", "app.json"): "{}\n",
				filepath.Join(sub, "src", "main.go"):     "package main\n",
			} {
				mkdirs(t, filepath.Dir(path))
				if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink("../config/prod.yml", filepath.Join(sub, "src", "notes.md")); err != nil {
				t.Fatal(err)
			}
			t.Chdir(filepath.Join(sub, "src"))
			got, err := ResolveRepo("")
			if err != nil || got != realPath(t, sub) {
				t.Fatalf("ResolveRepo from inside the nested repository = %q, %v; want %s", got, err, realPath(t, sub))
			}
			mounts, err := ComputeMounts(got, "/workspace")
			if err != nil {
				t.Fatal(err)
			}
			for target, hidden := range map[string]bool{
				"/workspace/secret.yml": true, "/workspace/config/prod.yml": true,
				"/workspace/config/app.json": false, "/workspace/src/main.go": false,
			} {
				if m := find(mounts, target); hidden && (m == nil || m.Kind != Decoy) || !hidden && m != nil {
					t.Errorf("%s: mount %+v, want hidden=%t", target, m, hidden)
				}
			}
			sources, err := openRepositorySources(got)
			if err != nil {
				t.Fatal(err)
			}
			defer sources.Close()
			if data, err := sources.readFile(filepath.Join("src", "notes.md")); err == nil || !strings.Contains(err.Error(), "hidden by Coop's secret policy") {
				t.Fatalf("a link to a hidden file in the nested repository was read: %q, %v", data, err)
			}
		})
	}
}

// An override that is a link, or passes through one, names the folder the link points to by the
// path a walk from inside that folder gives, so the secret scan looks inside it and everything
// keyed by the repository's path sees one project however it was named. An override that does
// not exist, or a link to nothing, is kept as given for the command using it to report.
func TestResolveRepoFollowsALinkedOverride(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo, _ := gitrepo.New(t)
	real := realPath(t, repo)
	mkdirs(t, filepath.Join(repo, "sub"))
	links := t.TempDir()
	link, dangling, missing := filepath.Join(links, "project"), filepath.Join(links, "gone"), filepath.Join(links, "missing")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(links, "nowhere"), dangling); err != nil {
		t.Fatal(err)
	}
	t.Chdir(links)
	for override, want := range map[string]string{
		link:                       real,
		"project":                  real,
		filepath.Join(link, "sub"): filepath.Join(real, "sub"),
		missing:                    missing,
		dangling:                   dangling,
	} {
		if got, err := ResolveRepo(override); err != nil || got != want {
			t.Errorf("ResolveRepo(%q) = %q, %v; want %s", override, got, err, want)
		}
	}
	t.Chdir(repo)
	if got, err := ResolveRepo(""); err != nil || got != real {
		t.Fatalf("ResolveRepo from inside the repository = %q, %v; want the same %s the linked override names", got, err, real)
	}
}

// A box that can write the folder above an override can swap it for a link to the home folder or
// above it, and the next launch would mount all the user's files, so an override a link takes there
// is refused, whether the link is its last part or an earlier one, and the refusal names the folder
// the link reached. The home folder as HOME spells it or by its own path, a folder above it named
// directly and a link into it are still taken. HOME passes through a link here, as /home does on
// some Linux systems, so naming it changes its path too.
func TestResolveRepoRefusesALinkToTheHomeFolder(t *testing.T) {
	base := t.TempDir()
	users := filepath.Join(base, "users")
	me, project, outer := filepath.Join(users, "me"), filepath.Join(users, "me", "project"), filepath.Join(users, "me", "p")
	mkdirs(t, project, outer)
	if err := os.Symlink(users, filepath.Join(base, "home")); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(base, "home", "me")
	t.Setenv("HOME", home)
	links := t.TempDir()
	for link, target := range map[string]string{
		filepath.Join(links, "me"): me, filepath.Join(links, "users"): users, filepath.Join(links, "project"): project,
		filepath.Join(outer, "wt"): home, // a box working in ~/p swapped the checkout ~/p/wt for a link home
	} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	for override, reached := range map[string]string{
		filepath.Join(links, "me"):          me,
		filepath.Join(links, "users"):       users,
		filepath.Join(links, "users", "me"): me,
		filepath.Join(home, "p", "wt"):      me,
	} {
		want := "COOP_REPO " + override + " leads through a link to " + realPath(t, reached) + ", your home folder or above"
		if got, err := ResolveRepo(override); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ResolveRepo(%s) = %q, %v; want a refusal saying %q", override, got, err, want)
		}
	}
	for override, want := range map[string]string{
		home:                            me,
		realPath(t, me):                 me, // no link on the way, so nothing to refuse
		filepath.Join(base, "home"):     users,
		filepath.Join(links, "project"): project,
	} {
		if got, err := ResolveRepo(override); err != nil || got != realPath(t, want) {
			t.Errorf("ResolveRepo(%s) = %q, %v; want %s", override, got, err, realPath(t, want))
		}
	}
}

// A path that reaches the repository through a link, a shell's working directory say, is placed by
// the folder it reaches. A path already under repo keeps its lexical answer, a link inside it
// included, and a path outside stays outside.
func TestRepoRelativeMatchesFoldersNotSpellings(t *testing.T) {
	repo := realPath(t, t.TempDir())
	outside := realPath(t, t.TempDir())
	mkdirs(t, filepath.Join(repo, "a", "b"))
	link := filepath.Join(t.TempDir(), "project")
	for target, name := range map[string]string{repo: link, outside: filepath.Join(repo, "out")} {
		if err := os.Symlink(target, name); err != nil {
			t.Fatal(err)
		}
	}
	for path, want := range map[string]string{
		filepath.Join(repo, "a", "b"):      filepath.Join("a", "b"),
		filepath.Join(link, "a", "b"):      filepath.Join("a", "b"),
		filepath.Join(link, "not-yet"):     "not-yet",
		link:                               ".",
		filepath.Join(repo, "out", "file"): filepath.Join("out", "file"),
	} {
		if got, err := RepoRelative(repo, path); err != nil || got != want {
			t.Errorf("RepoRelative(%s) = %q, %v; want %q", path, got, err, want)
		}
	}
	if got, err := RepoRelative(repo, filepath.Join(outside, "x")); err != nil || filepath.IsLocal(got) {
		t.Errorf("RepoRelative(a path outside) = %q, %v; want it outside", got, err)
	}
}

// A recorded path spelled through a link names the same folder as the one Coop derives now; a
// different folder, or a missing one spelled differently, does not.
func TestSameFolderMatchesFoldersNotSpellings(t *testing.T) {
	dir, other := realPath(t, t.TempDir()), realPath(t, t.TempDir())
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{dir, dir + string(filepath.Separator), true},
		{link, dir, true},
		{dir, other, false},
		{filepath.Join(link, "missing"), filepath.Join(dir, "missing"), false},
	} {
		if got := SameFolder(c.a, c.b); got != c.want {
			t.Errorf("SameFolder(%s, %s) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// A repository at the home folder or above is never chosen by walking up: a box that removes its
// own .git would otherwise move the next launch's workspace there. The refusal names the
// repository it found, and only the override can name it.
func TestResolveRepoNeverChoosesTheHomeFolder(t *testing.T) {
	home, _ := gitrepo.New(t)
	t.Setenv("HOME", home)
	project := filepath.Join(home, "notes")
	mkdirs(t, project)
	t.Chdir(project)
	if got, err := ResolveRepo(""); err == nil || !strings.Contains(err.Error(), "is "+realPath(t, home)+", at your home folder") {
		t.Fatalf("ResolveRepo below a home-folder repository = %q, %v; want a refusal naming it", got, err)
	}
	if got, err := ResolveRepo(home); err != nil || got != realPath(t, home) {
		t.Fatalf("ResolveRepo(override home) = %q, %v", got, err)
	}

	// A repository above the home folder, met from a folder beside it, is refused just the same.
	above, _ := gitrepo.New(t)
	t.Setenv("HOME", filepath.Join(above, "home", "me"))
	beside := filepath.Join(above, "opt", "tool")
	mkdirs(t, beside, filepath.Join(above, "home", "me"))
	t.Chdir(beside)
	if got, err := ResolveRepo(""); err == nil || !strings.Contains(err.Error(), "is "+realPath(t, above)+", at your home folder") {
		t.Fatalf("ResolveRepo beside the home folder under a repository = %q, %v; want a refusal naming it", got, err)
	}
}

// A box can make .git anything. One that is neither a folder nor a regular file, a FIFO or a
// device, is passed over as git passes it, and the walk never waits on it; a .git too large to be
// a pointer to a git dir is refused rather than read.
func TestResolveRepoNeverWaitsOnAPlantedGitEntry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo, _ := gitrepo.New(t)
	fifo, device, large := filepath.Join(repo, "fifo"), filepath.Join(repo, "device"), filepath.Join(repo, "large")
	mkdirs(t, fifo, device, large)
	if err := syscall.Mkfifo(filepath.Join(fifo, ".git"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/dev/null", filepath.Join(device, ".git")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(large, ".git"), []byte("gitdir: "+strings.Repeat("x", 1<<20)), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{fifo, device} {
		t.Chdir(dir)
		found := make(chan string, 1)
		go func() {
			got, err := ResolveRepo("")
			if err != nil {
				got = err.Error()
			}
			found <- got
		}()
		select {
		case got := <-found:
			if got != realPath(t, repo) {
				t.Fatalf("ResolveRepo from %s = %q; want the repository around it, %s", dir, got, realPath(t, repo))
			}
		case <-time.After(wait.Deadline):
			t.Fatalf("ResolveRepo from %s did not return within %v", dir, wait.Deadline)
		}
	}
	t.Chdir(large)
	if got, err := ResolveRepo(""); err == nil {
		t.Fatalf("ResolveRepo beside a 1 MiB .git = %q; want a refusal", got)
	}
}

// On a case-insensitive disk the shell keeps the case the user typed. The repository still comes
// back in the disk's own spelling, the one everything keyed by its path uses, and a home folder
// spelled another way is still the home folder.
func TestResolveRepoUsesTheDisksSpelling(t *testing.T) {
	base := realPath(t, t.TempDir())
	repo := filepath.Join(base, "Proj")
	mkdirs(t, filepath.Join(repo, ".git"), filepath.Join(repo, "sub"))
	typed := filepath.Join(base, "proj", "sub")
	if _, err := os.Stat(typed); err != nil {
		t.Skip("this disk is case-sensitive")
	}
	t.Setenv("HOME", t.TempDir())
	t.Chdir(typed)
	t.Setenv("PWD", typed)
	if got, err := ResolveRepo(""); err != nil || got != repo {
		t.Fatalf("ResolveRepo from %s = %q, %v; want %s", typed, got, err, repo)
	}
	home := filepath.Join(base, "Home")
	mkdirs(t, filepath.Join(home, ".git"), filepath.Join(home, "notes"))
	t.Setenv("HOME", filepath.Join(base, "home"))
	t.Chdir(filepath.Join(home, "notes"))
	if got, err := ResolveRepo(""); err == nil || !strings.Contains(err.Error(), "is "+home+", at your home folder") {
		t.Fatalf("ResolveRepo below a home folder spelled another way = %q, %v; want a refusal naming it", got, err)
	}
}

// A repository whose folder another user owns is refused, as git refuses it: the filesystem root,
// which root owns, stands in for another user's folder.
func TestFindRepoRefusesARepositoryAnotherUserOwns(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root owns the filesystem root")
	}
	if err := ownedRepository("/"); err == nil || !strings.Contains(err.Error(), "belongs to another user") {
		t.Fatalf("ownedRepository(/) = %v; want a refusal", err)
	}
	repo, _ := gitrepo.New(t)
	if err := ownedRepository(repo); err != nil {
		t.Fatalf("ownedRepository(your own repository) = %v", err)
	}
}
