package box

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// With an access ACL a file's group bits show the ACL's mask, so a file another user can write reads
// as a plain 0664. The check must see the ACL before it looks at who is in the group.
func TestRulesFileRefusesGroupWriteThroughAnACL(t *testing.T) {
	setfacl, err := exec.LookPath("setfacl")
	if err != nil {
		t.Skip("setfacl is not installed")
	}
	path := filepath.Join(t.TempDir(), "rules.yaml")
	if err := os.WriteFile(path, []byte(networkRulesFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(setfacl, "-m", "u:65534:rw", path).CombinedOutput(); err != nil {
		t.Skipf("this filesystem takes no ACL: %v: %s", err, out)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := unix.Fgetxattr(int(file.Fd()), "system.posix_acl_access", nil); err != nil {
		t.Fatalf("the fixture has no access ACL: %v", err)
	}
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o020 == 0 {
		t.Fatalf("the ACL mask should read as group write: %v", info.Mode())
	}
	if rulesFileWritableOnlyByOwner(file, info.Mode().Perm(), uint32(os.Getgid())) {
		t.Fatal("a file another user can write through an ACL was accepted")
	}
}
