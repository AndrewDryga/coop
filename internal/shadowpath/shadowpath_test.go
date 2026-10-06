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
	tree, snapshot, err := OpenTree(root, "")
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
// of a tree with secrets at several depths, nested .coopignore rules, allow-listed names and
// paths inside hidden directories gets the same answer from both, asked in shuffled order so the
// memo cannot lean on a walk's order.
func TestDeciderMatchesShadowed(t *testing.T) {
	repo := t.TempDir()
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
		data, err := safefile.ReadRegular(root, filepath.Join(filepath.FromSlash(dirRel), CoopIgnoreFile), ignoreLimit)
		if err != nil {
			return UserGlobs{}
		}
		return ParseUserGlobs(data)
	}
	rand.New(rand.NewPCG(1, 2)).Shuffle(len(paths), func(i, j int) { paths[i], paths[j] = paths[j], paths[i] })
	decide := NewDecider(repo)
	hidden := 0
	for _, rel := range paths {
		want := shadowed(rel, reference)
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
