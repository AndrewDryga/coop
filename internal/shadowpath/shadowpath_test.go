package shadowpath

import (
	"errors"
	"io/fs"
	"math/rand/v2"
	"os"
	"os/user"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/AndrewDryga/coop/internal/safefile"
)

func TestReadRegularClosesNestedDescriptors(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no .coopignore above the machine's temp folder reaches the repository
	repo := t.TempDir()
	dir := filepath.Join(repo, "a", "b")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"ok.txt": "ok", CoopIgnoreFile: "deny.txt\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.Open(repo)
	if err != nil {
		t.Fatal(err)
	}
	tree, snapshot, err := OpenTree(root, repo, "")
	_ = root.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	// Darwin can reject os.ReadDir("/dev/fd") inside Go even though the directory exists.
	// Fstat inventories this process directly on both supported hosts; new descriptors use
	// the lowest free numbers, so a range beyond the pinned tree covers traversal leaks.
	scanLimit := int(tree.Fd()) + 1024
	countFDs := func() int {
		t.Helper()
		var count int
		for fd := range scanLimit {
			var stat syscall.Stat_t
			err := syscall.Fstat(fd, &stat)
			if err == nil {
				count++
			} else if !errors.Is(err, syscall.EBADF) {
				t.Fatalf("inspect descriptor %d: %v", fd, err)
			}
		}
		return count
	}

	// A finalizer must not be needed to release each nested directory descriptor.
	priorGC := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(priorGC)
	baseline := countFDs()
	for range 16 {
		body, err := snapshot.ReadRegular(tree, "", filepath.Join("a", "b", "ok.txt"), 32)
		if err != nil || string(body) != "ok" {
			t.Fatalf("nested read = %q, %v", body, err)
		}
		if _, err := snapshot.ReadRegular(tree, "", filepath.Join("a", "b", "deny.txt"), 32); !errors.Is(err, ErrProtected) {
			t.Fatalf("nested protected read = %v, want ErrProtected", err)
		}
		if _, err := snapshot.ReadRegular(tree, "", filepath.Join("a", "b", "missing.txt"), 32); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("nested missing read = %v, want os.ErrNotExist", err)
		}
		if _, err := snapshot.ReadRegular(tree, "", filepath.Join("a", "absent", "ok.txt"), 32); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("nested missing directory = %v, want os.ErrNotExist", err)
		}
	}
	if got := countFDs(); got > baseline+4 {
		t.Fatalf("nested reads retained %d descriptors (baseline %d, now %d)", got-baseline, baseline, got)
	}
}

// Only a line that starts with # is a comment, so an entry with a comment after its pattern is one
// pattern that hides nothing its author meant; CommentedEntries finds those, and only those.
func TestCommentedEntries(t *testing.T) {
	data := []byte("# a whole-line comment\nprod.yml   # basename\nvault/\tthe folder # note\nissue#12.txt\n  # indented comment\nkeys/*.pem\n")
	got := CommentedEntries(data)
	want := []string{"prod.yml   # basename", "vault/\tthe folder # note"}
	if !slices.Equal(got, want) {
		t.Fatalf("CommentedEntries = %q, want %q", got, want)
	}
	// and such an entry really does hide nothing: the pattern keeps its comment
	g := ParseUserGlobs([]byte("prod.yml   # basename\n"))
	if slices.Contains(g.Base, "prod.yml") {
		t.Fatalf("a commented entry parsed as %q; the warning would be wrong", g.Base)
	}
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, CoopIgnoreFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := RepoCommentedEntries(repo); !slices.Equal(got, want) {
		t.Fatalf("RepoCommentedEntries = %q, want %q", got, want)
	}
	if got := RepoCommentedEntries(t.TempDir()); got != nil {
		t.Fatalf("a repository without .coopignore reported %q", got)
	}
}

// NewDecider remembers directory verdicts for speed; shadowed is the plain definition. Every path
// of a tree with secrets at several depths, nested .coopignore rules, rules in the folder above
// the repository, allow-listed names and paths inside hidden directories gets the same answer from
// both, asked in shuffled order so the memo cannot lean on a walk's order. Below a .coopignore the
// safe read refuses, a path shadowed hides, reading that file as empty, is hidden with no error,
// and every other path carries the refusal.
func TestDeciderMatchesShadowed(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // a sibling of outer: the walk above the repository stops below their parent
	outer := t.TempDir()
	repo := filepath.Join(outer, "proj")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outer, CoopIgnoreFile), []byte("proj/src/certs/*.pem\nproj/pkg/x/conf\napp.json\nother/*.go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		".coopignore":                    "vault/\nconfig/stripe.live.json\n*.dump\n",
		"src/app.go":                     "",
		"src/.env":                       "",
		"src/.env.example":               "",
		"src/certs/ca-bundle.pem":        "",
		"src/certs/server.pem":           "",
		"src/keys.template":              "",
		"src/id_rsa.template":            "",
		"config/stripe.live.json":        "",
		"config/app.json":                "",
		"vault/token":                    "",
		"vault/deep/x/y.txt":             "",
		"secrets/a/b/c.txt":              "",
		"db/backup.dump":                 "",
		"pkg/.coopignore":                "local/\nsettings.json\nconf/*.yml\n",
		"pkg/local/notes.md":             "",
		"pkg/settings.json":              "",
		"pkg/sub/settings.json":          "",
		"pkg/conf/a.yml":                 "",
		"pkg/x/conf/a.yml":               "",
		"pkg/deep/er/settings.json":      "",
		"pkg/deep/er/conf/b.yml":         "",
		"pkg/sub/.coopignore":            "*\n",
		"pkg/sub/anything.go":            "",
		"tasks/t1/tmp/clone/.env":        "",
		"tasks/t1/tmp/clone/src/main.go": "",
		"tasks/t1/tmp/clone/.ssh/id":     "",
		"tasks/t1/artifacts/report.md":   "",
		"Credentials/README.md":          "",
		"docs/kubeconfig.yaml.example":   "",
		"docs/guide/credentials.yml":     "",
		// below linked/.coopignore, a link planted next: hidden by a name, a folder's name, a rule
		// in the root, above the repository or below the link, or by nothing coop can read
		"linked/plain.txt":        "",
		"linked/.env":             "",
		"linked/db.dump":          "",
		"linked/app.json":         "",
		"linked/vault/key.txt":    "",
		"linked/secrets/a/b.txt":  "",
		"linked/sub/.coopignore":  "x.txt\ncfg/*.yml\n",
		"linked/sub/x.txt":        "",
		"linked/sub/deep/x.txt":   "",
		"linked/sub/cfg/a.yml":    "",
		"linked/sub/cfg/b.json":   "",
		"linked/sub/y.txt":        "",
		"linked/sub/.env.example": "",
	}
	for rel, body := range files {
		p := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Links the safe read refuses: a .coopignore that is a symlink, which leaves the paths below it
	// that nothing readable hides undecided, and a directory reached through one, which has no rules
	// of its own (the folder it reaches is judged by its own path). The decider's lstat shortcut must
	// not change either.
	if err := os.WriteFile(filepath.Join(repo, "outside-rules"), []byte("*\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../outside-rules", filepath.Join(repo, "linked", CoopIgnoreFile)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("pkg", filepath.Join(repo, "pkglink")); err != nil {
		t.Fatal(err)
	}
	paths := []string{"pkglink/settings.json", "pkglink/local/notes.md", "pkglink/sub/anything.go", "missing/dir/.env", "missing/file.go"}
	if err := filepath.WalkDir(repo, func(p string, _ fs.DirEntry, err error) error {
		if err != nil || p == repo {
			return err
		}
		rel, err := filepath.Rel(repo, p)
		paths = append(paths, filepath.ToSlash(rel))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	reference := func(dirRel string) UserGlobs { // the safe read, for every directory, uncached
		root, err := safefile.OpenRoot(repo)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		data, err := safefile.ReadRegular(root, filepath.Join(filepath.FromSlash(dirRel), CoopIgnoreFile), ignoreLimit)
		if err != nil {
			return UserGlobs{}
		}
		return ParseUserGlobs(data)
	}
	enclosing, _, err := enclosingPolicies(repo)
	if err != nil || len(enclosing) != 1 {
		t.Fatalf("rules above the repository = %+v, %v; want the one .coopignore in %s", enclosing, err, outer)
	}
	rand.New(rand.NewPCG(1, 2)).Shuffle(len(paths), func(i, j int) { paths[i], paths[j] = paths[j], paths[i] })
	decide := NewDecider(repo)
	unreadable := UnreadableCoopignore{File: "linked/.coopignore", Reason: "is a link"}
	hidden, decidedBelow, undecided := 0, 0, 0
	for _, rel := range paths {
		got, err := decide(rel)
		want, wantErr := shadowed(rel, enclosing, reference), error(nil)
		switch {
		case !strings.HasPrefix(rel, "linked/"):
		case want:
			decidedBelow++
		default:
			want, wantErr = true, unreadable
			undecided++
		}
		if got != want || err != wantErr {
			t.Errorf("%s: decider says hidden=%t, %v; want hidden=%t, %v", rel, got, err, want, wantErr)
		}
		if want && wantErr == nil {
			hidden++
		}
	}
	if hidden == 0 || hidden+undecided == len(paths) {
		t.Fatalf("the tree should mix hidden and visible paths, got %d hidden and %d undecided of %d", hidden, undecided, len(paths))
	}
	if decidedBelow < 8 || undecided < 3 {
		t.Fatalf("below linked/.coopignore %d paths are hidden by what coop can read and %d undecided; the tree no longer tests both", decidedBelow, undecided)
	}
}

// A box can make any folder it writes a repository by planting a .git there, and a command run
// from inside takes that folder for the project. The .coopignore above it still hides what it
// names there, path patterns matched from its own folder, wildcards included, in the decider and
// in a snapshot alike. A path rule for a sibling, the built-in names of the folders above and the
// rules at the home folder or above it hide nothing.
func TestRulesAboveTheRepositoryStillApply(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	t.Setenv("HOME", home)
	outer := filepath.Join(home, "work")
	repo := filepath.Join(outer, "secrets", "app")
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(base, CoopIgnoreFile), "*\n")
	write(filepath.Join(home, CoopIgnoreFile), "*\n")
	write(filepath.Join(outer, CoopIgnoreFile), "secrets/app/secret.yml\nsecrets/app/config/*.yml\nother/*.json\n")
	want := map[string]bool{"secret.yml": true, "config/prod.yml": true, "config/app.json": false, "config": false, "src/main.go": false}
	for rel := range want {
		if rel != "config" {
			write(filepath.Join(repo, filepath.FromSlash(rel)), "")
		}
	}
	root, err := safefile.OpenRoot(repo)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	tree, snapshot, err := OpenTree(root, repo, "")
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	decide := NewDecider(repo)
	for rel, hidden := range want {
		if got, err := decide(rel); got != hidden || err != nil {
			t.Errorf("decider: %s hidden=%t, %v; want %t", rel, got, err, hidden)
		}
		if got := snapshot.Shadowed(rel); got != hidden {
			t.Errorf("snapshot: %s hidden=%t, want %t", rel, got, hidden)
		}
	}
	if _, err := snapshot.ReadRegular(tree, "", "secret.yml", 32); !errors.Is(err, ErrProtected) {
		t.Fatalf("snapshot read of secret.yml = %v, want ErrProtected", err)
	}
}

// A .coopignore that is there but that coop cannot read leaves what it hides unknown, so the decider
// and a snapshot both refuse it, naming it and why, rather than drop its rules: one in the
// repository's root, one in a folder of the repository (which leaves the paths outside that folder
// decided) and one in a folder above the repository. A missing one is just no rules.
func TestAnUnreadableCoopignoreIsRefused(t *testing.T) {
	for _, kind := range []struct {
		name, reason string
		plant        func(t *testing.T, path string)
	}{
		{"a link", "is a link", func(t *testing.T, path string) {
			if err := os.WriteFile(path+".target", []byte("other.txt\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Base(path)+".target", path); err != nil {
				t.Fatal(err)
			}
		}},
		{"a folder", "is a folder", func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{"a FIFO", "is not a regular file", func(t *testing.T, path string) {
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"a file over 1 MiB", "is larger than 1 MiB", func(t *testing.T, path string) {
			if err := os.WriteFile(path, make([]byte, ignoreLimit+1), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"a file without read permission", "cannot be read: permission denied", func(t *testing.T, path string) {
			if os.Geteuid() == 0 {
				t.Skip("root reads a file without read permission")
			}
			if err := os.WriteFile(path, []byte("other.txt\n"), 0); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		for _, place := range []string{"repository", "folder", "folder above"} {
			t.Run(kind.name+" in the "+place, func(t *testing.T) {
				home, err := filepath.EvalSymlinks(t.TempDir()) // the spelling the walk above names files by
				if err != nil {
					t.Fatal(err)
				}
				t.Setenv("HOME", home)
				outer := filepath.Join(home, "work")
				repo := filepath.Join(outer, "app")
				for _, dir := range []string{filepath.Join(repo, "config"), filepath.Join(repo, "src")} {
					if err := os.MkdirAll(dir, 0o755); err != nil {
						t.Fatal(err)
					}
				}
				policy, file := filepath.Join(repo, "config", CoopIgnoreFile), "config/.coopignore"
				switch place {
				case "repository":
					policy, file = filepath.Join(repo, CoopIgnoreFile), ".coopignore"
				case "folder above":
					policy = filepath.Join(outer, CoopIgnoreFile)
					file = policy
				}
				kind.plant(t, policy)
				want := UnreadableCoopignore{File: file, Reason: kind.reason}
				decide := NewDecider(repo)
				if hidden, err := decide("config/app.yml"); !hidden || err != want {
					t.Errorf("config/app.yml: hidden=%t, %v; want hidden with %q", hidden, err, want)
				}
				if hidden, err := decide(".env"); !hidden || err != nil {
					t.Errorf(".env: hidden=%t, %v; a built-in secret name is hidden whatever the rules say", hidden, err)
				}
				if hidden, err := decide("src/main.go"); place == "folder" && (hidden || err != nil) {
					t.Errorf("src/main.go: hidden=%t, %v; config/.coopignore does not cover it", hidden, err)
				}
				root, err := safefile.OpenRoot(repo)
				if err != nil {
					t.Fatal(err)
				}
				defer root.Close()
				tree, _, err := OpenTree(root, repo, "config")
				if err == nil {
					_ = tree.Close()
				}
				if !errors.Is(err, want) {
					t.Errorf("snapshot of config = %v; want %q", err, want)
				}
			})
		}
	}
	repo := t.TempDir()
	t.Setenv("HOME", repo)
	if hidden, err := NewDecider(repo)("config/app.yml"); hidden || err != nil {
		t.Errorf("no .coopignore: hidden=%t, %v; want visible", hidden, err)
	}
}

// Rules only ever hide, so a .coopignore coop cannot read leaves undecided only what the rest
// leaves visible. A path that a readable rule hides, whether the rule sits above the unreadable
// file, in a folder above the repository (nearer it or farther than an unreadable one there) or
// below it, past a second unreadable file too, or that its own name or a folder's name above it
// marks as a secret, is hidden with no error, so a scan can name it. Every other path below the
// unreadable file, the file itself and the folders it covers included, carries the error of the
// nearest one above the repository, else the top-most inside it, which still fails a snapshot. Each path is asked of one decider, a
// folder before what it holds, and of a fresh one, so the memo cannot answer for what a folder holds.
func TestReadableRulesStillHideBelowAnUnreadableCoopignore(t *testing.T) {
	type answer struct {
		rel    string
		hidden bool
		err    error
	}
	write := func(t *testing.T, path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	unreadable := func(t *testing.T, path string) {
		t.Helper()
		if err := os.MkdirAll(path, 0o755); err != nil { // a folder where the rules belong
			t.Fatal(err)
		}
	}
	check := func(t *testing.T, repo string, want []answer, refused error) {
		t.Helper()
		shared := NewDecider(repo)
		for _, w := range want {
			for name, decide := range map[string]func(string) (bool, error){"a shared decider": shared, "a fresh decider": NewDecider(repo)} {
				if hidden, err := decide(w.rel); hidden != w.hidden || err != w.err {
					t.Errorf("%s: %s says hidden=%t, %v; want hidden=%t, %v", w.rel, name, hidden, err, w.hidden, w.err)
				}
			}
		}
		root, err := safefile.OpenRoot(repo)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		tree, _, err := OpenTree(root, repo, "config")
		if err == nil {
			_ = tree.Close()
		}
		if !errors.Is(err, refused) {
			t.Errorf("snapshot of config = %v; want %q", err, refused)
		}
	}

	t.Run("in the repository", func(t *testing.T) {
		home, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("HOME", home)
		outer := filepath.Join(home, "work")
		repo := filepath.Join(outer, "app")
		write(t, filepath.Join(outer, CoopIgnoreFile), "*.dump\napp/config/sub/outer.yml\n")
		write(t, filepath.Join(repo, CoopIgnoreFile), "*.prod.json\nvault/\n")
		unreadable(t, filepath.Join(repo, "config", CoopIgnoreFile))
		write(t, filepath.Join(repo, "config", "sub", CoopIgnoreFile), "local.yml\ncfg/*.yml\n")
		write(t, filepath.Join(repo, "config", "sub", "deep", "er", CoopIgnoreFile), "*.log\n")
		// A second unreadable one, below the first and above the rules in er/.
		if err := os.Symlink("elsewhere", filepath.Join(repo, "config", "sub", "deep", CoopIgnoreFile)); err != nil {
			t.Fatal(err)
		}
		first := UnreadableCoopignore{File: "config/.coopignore", Reason: "is a folder"}
		check(t, repo, []answer{
			// A folder's .coopignore covers what the folder holds, not the folder.
			{"config", false, nil},
			// Left visible by the readable rules: the unreadable file, a folder below it and files.
			{"config/.coopignore", true, first},
			{"config/sub", true, first},
			{"config/sub/app.yml", true, first},
			{"config/sub/cfg/b.json", true, first},
			// Hidden by the root rule above the unreadable file, a name rule and a path rule above
			// the repository, a name rule and a path rule below the unreadable file, a secret name,
			// its own or a folder's, and a folder a readable rule hides.
			{"config/sub/db.prod.json", true, nil},
			{"config/sub/backup.dump", true, nil},
			{"config/sub/outer.yml", true, nil},
			{"config/sub/local.yml", true, nil},
			{"config/sub/cfg/a.yml", true, nil},
			{"config/sub/.env", true, nil},
			{"config/sub/.aws", true, nil},
			{"config/sub/.aws/config", true, nil},
			{"config/vault", true, nil},
			{"config/vault/key.txt", true, nil},
			// Past the second unreadable file the rules still count, and the error is the first one's.
			{"config/sub/deep", true, first},
			{"config/sub/deep/notes.txt", true, first},
			{"config/sub/deep/local.yml", true, nil},
			{"config/sub/deep/er/run.log", true, nil},
			{"config/sub/deep/er/run.txt", true, first},
			// Outside what the unreadable file covers.
			{"src/main.go", false, nil},
			{"db.prod.json", true, nil},
		}, first)
	})

	t.Run("above the repository", func(t *testing.T) {
		home, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("HOME", home)
		team := filepath.Join(home, "work", "team")
		repo := filepath.Join(team, "group", "app")
		// Readable rules farther from the repository than the unreadable file, and nearer it.
		write(t, filepath.Join(home, "work", CoopIgnoreFile), "*.bak\n")
		unreadable(t, filepath.Join(team, CoopIgnoreFile))
		write(t, filepath.Join(team, "group", CoopIgnoreFile), "*.tmp\napp/config/*.json\n")
		write(t, filepath.Join(repo, CoopIgnoreFile), "*.prod.yml\n")
		write(t, filepath.Join(repo, "config", CoopIgnoreFile), "local.yml\n")
		first := UnreadableCoopignore{File: filepath.Join(team, CoopIgnoreFile), Reason: "is a folder"}
		check(t, repo, []answer{
			{"notes.bak", true, nil},
			{"notes.tmp", true, nil},
			{"config/app.json", true, nil},
			{"config/db.prod.yml", true, nil},
			{"config/local.yml", true, nil},
			{".env", true, nil},
			{"secrets/notes.md", true, nil},
			{"config", true, first},
			{"config/app.yml", true, first},
			{"src/main.go", true, first},
		}, first)
	})
}

// The owner of the folder holding a .coopignore above the repository decides whether it applies,
// whoever owns the file: one saved with sudo in your own folder applies, and one in a folder
// another user owns is skipped, since anyone can create /tmp/.coopignore or hard-link a file of
// yours there. Only root can give a file or folder away, so ownerOf stands in both owners, for the
// decider and a snapshot alike; the real ownerOf is checked on the filesystem root, which root owns.
func TestRulesAboveFollowTheFolderOwner(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir()) // the walk's own spelling, which ownerOf is asked about
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", filepath.Join(base, "home"))
	outer := filepath.Join(base, "home", "work")
	repo := filepath.Join(outer, "app")
	for path, body := range map[string]string{filepath.Join(outer, CoopIgnoreFile): "secret.yml\n", filepath.Join(repo, "secret.yml"): ""} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	me, other := os.Getuid(), os.Getuid()+1 // other stands in root, or anyone else
	for _, tc := range []struct {
		name   string
		owner  func(path string) int
		hidden bool
	}{
		{"a .coopignore saved with sudo in your folder applies", func(path string) int {
			if filepath.Base(path) == CoopIgnoreFile {
				return other
			}
			return me
		}, true},
		{"your .coopignore in another user's folder is skipped", func(path string) int {
			if path == outer {
				return other
			}
			return me
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saved := ownerOf
			t.Cleanup(func() { ownerOf = saved })
			ownerOf = func(path string) (int, bool) { return tc.owner(path), true }
			if got, err := NewDecider(repo)("secret.yml"); got != tc.hidden || err != nil {
				t.Errorf("decider: secret.yml hidden=%t, %v; want %t", got, err, tc.hidden)
			}
			root, err := safefile.OpenRoot(repo)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			tree, snapshot, err := OpenTree(root, repo, "")
			if err != nil {
				t.Fatal(err)
			}
			defer tree.Close()
			if got := snapshot.Shadowed("secret.yml"); got != tc.hidden {
				t.Errorf("snapshot: secret.yml hidden=%t, want %t", got, tc.hidden)
			}
		})
	}
	if uid, known := ownerOf("/"); !known || uid != 0 {
		t.Errorf("ownerOf(/) = %d, %t; want root's 0", uid, known)
	}
}

// A globSet skips filepath.Match for names that lack a pattern's literal start or end, so it must
// never skip one that matches. Every built-in list, plus patterns with escapes, classes, a literal
// "]" and malformed ones, is asked about names made from the patterns themselves (wildcards filled
// in, cut short, extended, upper-cased) and random names over the patterns' own bytes.
func TestGlobSetMatchesFilepathMatch(t *testing.T) {
	tricky := []string{`a\*b`, `\*.pem`, `*\[x`, `x[a-c]y`, `[!.]env`, `a]b`, `*.[kp]e[my]`, `id_??a*`, `[`, `a[b`, `*\`, `?`, `*`, ``}
	lists := map[string][]string{
		"secret": SecretGlobs, "allow": AllowGlobs, "template": allowTemplateGlobs, "hard": hardSecretGlobs, "tricky": tricky,
	}
	var names []string
	alphabet := ""
	for _, globs := range lists {
		for _, glob := range globs {
			alphabet += glob
			for _, fill := range []string{"", "x", "a.b", "pem"} {
				filled := []byte{}
				for i := 0; i < len(glob); i++ {
					switch glob[i] {
					case '*':
						filled = append(filled, fill...)
					case '?':
						filled = append(filled, 'q')
					default:
						filled = append(filled, glob[i])
					}
				}
				names = append(names, string(filled), "x"+string(filled), string(filled)+".bak", string(filled)+"x")
			}
			for i := 0; i <= len(glob); i++ {
				names = append(names, glob[:i], glob[i:])
			}
			names = append(names, glob, strings.ToUpper(glob))
		}
	}
	random := rand.New(rand.NewPCG(3, 4))
	for range 20000 {
		name := make([]byte, random.IntN(14))
		for i := range name {
			name[i] = alphabet[random.IntN(len(alphabet))]
		}
		names = append(names, string(name))
	}
	matched := 0
	for list, globs := range lists {
		set := compileGlobs(globs)
		for _, name := range names {
			want := MatchesAny(name, globs)
			if got := set.matches(name); got != want {
				t.Errorf("%s patterns, name %q: globSet says %t, filepath.Match says %t", list, name, got, want)
			}
			if want {
				matched++
			}
		}
	}
	if matched < 1000 {
		t.Fatalf("only %d matching names: the corpus no longer exercises the patterns", matched)
	}
}

// A folder counts as the home folder or above it only when it holds home: a folder beside home does
// not, and neither does one that reaches home through a link, which a box can plant.
func TestHomeOrAboveCountsOnlyFoldersThatHoldHome(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "real", "Users", "me")
	for _, dir := range []string{home, filepath.Join(root, "real", "Users", "you"), filepath.Join(root, "other")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "real", "Users"), filepath.Join(root, "other", "Users")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	for dir, want := range map[string]bool{
		home:                                 true,
		filepath.Join(root, "real", "Users"): true,
		filepath.Join(root, "real"):          true,
		filepath.Join(root, "real", "Users", "you"): false, // beside home
		filepath.Join(root, "other"):                false, // other/Users/me is home only through a link
	} {
		if _, got := HomeOrAbove(dir); got != want {
			t.Errorf("HomeOrAbove(%s) = %t, want %t", dir, got, want)
		}
	}
}

// macOS reaches every home folder through the data volume too: /Users/me is also
// /System/Volumes/Data/Users/me, so the volume's mount point and every folder above it hold home.
func TestHomeOrAboveCountsTheMacOSDataVolume(t *testing.T) {
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		t.Skip("no account home folder")
	}
	real, err := filepath.EvalSymlinks(u.HomeDir)
	if err != nil {
		t.Skip("account home folder cannot be resolved")
	}
	const data = "/System/Volumes/Data"
	if info, err := os.Lstat(filepath.Join(data, real)); err != nil || !info.IsDir() {
		t.Skip("no data volume path to the home folder (not macOS)")
	}
	t.Setenv("HOME", real)
	for dir, want := range map[string]bool{
		data:                           true,
		"/System/Volumes":              true,
		"/System":                      true,
		filepath.Join(data, "private"): false, // on the volume, beside the folders that hold home
	} {
		if _, got := HomeOrAbove(dir); got != want {
			t.Errorf("HomeOrAbove(%s) = %t, want %t: home %s is also %s", dir, got, want, real, filepath.Join(data, real))
		}
	}
}
