package fsfilter

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func guardSource(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("assets", "filter.c"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// guardHooks maps each LSM hook to the body of its HOOK(...) guard.
func guardHooks(t *testing.T) map[string]string {
	t.Helper()
	hooks := map[string]string{}
	for _, part := range strings.Split(guardSource(t), "\nHOOK(")[1:] {
		// The split ate the newline after a one-line guard; a multi-line guard closes on its own line.
		name, body, _ := strings.Cut(part+"\n", ",")
		end := strings.Index(body, "}\n")
		if end < 0 {
			t.Fatalf("unterminated guard for %s", name)
		}
		hooks[name] = body[:end]
	}
	return hooks
}

// The guard is compiled into the helper image, so its contract is pinned at the source. Every attached
// hook has a guard. A guard that only gives a new object a name, or changes an object without showing
// its bytes, passes BLIND, so the box can create a file, folder, symlink or node named like a secret.
// A guard that can reveal bytes, a symlink's target or a second name never does.
func TestGuardsPassBlindOnlyWhereNothingIsRevealed(t *testing.T) {
	hooks := guardHooks(t)
	for _, name := range hookNames {
		if _, ok := hooks[name]; !ok {
			t.Errorf("no guard for the %s hook", name)
		}
	}
	for _, name := range []string{"inode_create", "inode_mkdir", "inode_symlink", "inode_mknod", "inode_setattr", "inode_unlink", "inode_rmdir", "file_truncate", "inode_setxattr", "inode_removexattr", "inode_set_acl", "inode_remove_acl", "inode_file_setattr"} {
		if !strings.Contains(hooks[name], "BLIND") {
			t.Errorf("the %s guard refuses a new file named like a secret:%s", name, hooks[name])
		}
	}
	for _, name := range []string{"inode_follow_link", "inode_readlink", "inode_link", "mmap_file", "file_mprotect", "inode_getxattr", "inode_listxattr", "inode_get_acl", "inode_file_getattr"} {
		if strings.Contains(hooks[name], "BLIND") {
			t.Errorf("the %s guard can reveal a file named like a secret:%s", name, hooks[name])
		}
	}
}

// The live name check knows a file by its own name only. No folder name exempts what it holds, .git
// included: the box can rename any folder into one (mv sub .git/sub) and read a secret the host saved
// under it after launch.
func TestGuardWalkExemptsNoFolderName(t *testing.T) {
	source := guardSource(t)
	start, end := strings.Index(source, "static long walk_step("), strings.Index(source, "struct path_context")
	if start < 0 || end < start {
		t.Fatal("walk_step not found in filter.c")
	}
	walk := source[start:end]
	if sets := regexp.MustCompile(`c->named\s*=`).FindAllString(walk, -1); len(sets) != 1 || !strings.Contains(walk, "if (!i && !directory) c->named = named;") {
		t.Fatalf("walk_step sets named from something other than the leaf's own name:\n%s", walk)
	}
}
