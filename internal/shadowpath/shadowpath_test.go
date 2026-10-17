package shadowpath

import (
	"errors"
	"io/fs"
	"math/rand/v2"
	"os"
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
// both, asked in shuffled order so the memo cannot lean on a walk's order.
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
	// Links the safe read refuses: a .coopignore that is a symlink, and a directory reached
	// through one. The decider's lstat shortcut must not change what they mean.
	if err := os.WriteFile(filepath.Join(repo, "outside-rules"), []byte("*\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "linked"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../outside-rules", filepath.Join(repo, "linked", CoopIgnoreFile)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "linked", "plain.txt"), nil, 0o644); err != nil {
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
		data, err := safefile.ReadRegular(root, filepath.Join(filepath.FromSlash(dirRel), CoopIgnoreFile), MaxCoopIgnoreBytes)
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
	hidden := 0
	for _, rel := range paths {
		want := shadowed(rel, enclosing, reference)
		if got := decide(rel); got != want {
			t.Errorf("%s: decider says hidden=%t, shadowed says %t", rel, got, want)
		}
		if want {
			hidden++
		}
	}
	if hidden == 0 || hidden == len(paths) {
		t.Fatalf("the tree should mix hidden and visible paths, got %d of %d hidden", hidden, len(paths))
	}
}

// A box can make any folder it writes a repository by planting a .git there, and a command run
// from inside takes that folder for the project. The .coopignore above it still hides what it
// names there, path patterns matched from its own folder, wildcards included, in the decider and
// in a snapshot alike. A path rule for a sibling, the built-in names of the folders above and the
// rules at the home folder or above it hide nothing; a .coopignore above that cannot be read fails
// a snapshot, and the decider keeps the rules it could read.
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
		if got := decide(rel); got != hidden {
			t.Errorf("decider: %s hidden=%t, want %t", rel, got, hidden)
		}
		if got := snapshot.Shadowed(rel); got != hidden {
			t.Errorf("snapshot: %s hidden=%t, want %t", rel, got, hidden)
		}
	}
	if _, err := snapshot.ReadRegular(tree, "", "secret.yml", 32); !errors.Is(err, ErrProtected) {
		t.Fatalf("snapshot read of secret.yml = %v, want ErrProtected", err)
	}

	linked := filepath.Join(outer, "linked", "app")
	write(filepath.Join(linked, "secret.yml"), "")
	if err := os.Symlink(filepath.Join(base, CoopIgnoreFile), filepath.Join(outer, "linked", CoopIgnoreFile)); err != nil {
		t.Fatal(err)
	}
	if !NewDecider(linked)("secret.yml") {
		t.Error("an unreadable .coopignore above the repository must not drop the readable ones further up")
	}
	linkedRoot, err := safefile.OpenRoot(linked)
	if err != nil {
		t.Fatal(err)
	}
	defer linkedRoot.Close()
	if dir, _, err := OpenTree(linkedRoot, linked, ""); err == nil {
		_ = dir.Close()
		t.Fatal("a snapshot below an unreadable .coopignore must refuse, as one below an unreadable rule inside the repository does")
	}
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
			if got := NewDecider(repo)("secret.yml"); got != tc.hidden {
				t.Errorf("decider: secret.yml hidden=%t, want %t", got, tc.hidden)
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
