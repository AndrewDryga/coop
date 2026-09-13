package box

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

var hiddenAncestorCases = []struct {
	name, policy, rules, parent string
}{
	{"root basename", ".coopignore", "private/\n", "private"},
	{"root path", ".coopignore", "config/private/\n", "config/private"},
	{"nested basename", "nested/.coopignore", "private/\n", "nested/private"},
	{"nested path", "nested/.coopignore", "config/private/\n", "nested/config/private"},
	{"built in", ".coopignore", "# defaults only\n", ".SSH"},
}

func TestShadowDeciderHiddenAncestors(t *testing.T) {
	for _, tc := range hiddenAncestorCases {
		t.Run(tc.name, func(t *testing.T) {
			repo, _ := hiddenAncestorFixture(t, tc.policy, tc.rules, tc.parent)
			mounts, err := ComputeMounts(repo, "/workspace")
			if err != nil {
				t.Fatal(err)
			}
			parent := find(mounts, "/workspace/"+tc.parent)
			if parent == nil || parent.Kind != DirDecoy || !parent.RO {
				t.Fatal("control: primary box must hide the parent directory")
			}
			shadowed := NewShadowDecider(repo)
			for _, child := range []string{"notes.txt", "sub/notes.txt", "cacerts.pem", ".env.example"} {
				if !shadowed(tc.parent + "/" + child) {
					t.Errorf("%s: descendant of a hidden directory must stay hidden", child)
				}
			}
			for _, public := range []string{"public/notes.txt", "public/cacerts.pem", ".env.example/notes.txt", "private-sibling/notes.txt"} {
				if shadowed(public) {
					t.Errorf("%s: ordinary sibling or allowed directory became hidden", public)
				}
			}
			if tc.policy != ".coopignore" && shadowed("private/notes.txt") {
				t.Error("nested policy escaped its subtree")
			}
			if tc.rules == "config/private/\n" && shadowed(filepath.ToSlash(filepath.Join(filepath.Dir(tc.policy), "other/config/private/notes.txt"))) {
				t.Error("path rule lost its directory-relative anchor")
			}
		})
	}
}

func TestServiceShadowHiddenAncestors(t *testing.T) {
	for _, tc := range hiddenAncestorCases {
		t.Run(tc.name, func(t *testing.T) {
			repo, file := hiddenAncestorFixture(t, tc.policy, tc.rules, tc.parent)
			data, err := readValidatedCompose(file, repo, false)
			if err != nil {
				t.Fatal(err)
			}
			decoys, hidden, err := serviceShadowPlan(repo, file, data)
			if err != nil {
				t.Fatal(err)
			}
			want := []serviceDecoy{
				{target: "/direct", source: tc.parent + "/notes.txt"},
				{target: "/directory", source: tc.parent + "/sub", dir: true},
				{target: "/alias", source: tc.parent + "/notes.txt"},
			}
			if !slices.Equal(decoys["probe"], want) || !slices.Equal(hidden, []string{tc.parent + "/notes.txt", tc.parent + "/sub"}) {
				t.Fatalf("decoys=%+v hidden=%v, want direct/dir/alias protection only", decoys, hidden)
			}
		})
	}
}

// A service gets direct file/directory binds below the hidden parent, a symlink
// alias to that file, and an ordinary writable sibling. All bytes are public markers.
func hiddenAncestorFixture(t *testing.T, policy, rules, parent string) (repo, file string) {
	t.Helper()
	repo = t.TempDir()
	for rel, body := range map[string]string{
		policy: rules, parent + "/notes.txt": "hidden marker\n",
		parent + "/sub/notes.txt": "hidden marker\n", "public/notes.txt": "public marker\n",
	} {
		path := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(parent+"/notes.txt", filepath.Join(repo, "alias")); err != nil {
		t.Fatal(err)
	}
	file = filepath.Join(repo, "compose.yml")
	body := fmt.Sprintf(`services:
  probe:
    image: alpine:3.21
    user: "%d:%d"
    mem_limit: 128m
    cpus: 1
    volumes:
      - "./%s/notes.txt:/direct"
      - "./%s/sub:/directory"
      - "./alias:/alias"
      - "./public:/public"
`, os.Getuid(), os.Getgid(), parent, parent)
	if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo, file
}
