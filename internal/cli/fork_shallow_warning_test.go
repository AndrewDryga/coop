package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

// A fork made from a shallow checkout is still created, but says at once that it cannot be looped
// or merged until the checkout has the full history, instead of after an agent worked in it.
func TestForkOfAShallowCheckoutWarnsAtCreation(t *testing.T) {
	src := initRepo(t)
	clone, _ := gitrepo.ShallowClone(t, src, 1)
	shallow, err := filepath.EvalSymlinks(clone)
	if err != nil {
		t.Fatal(err)
	}
	for _, isolated := range []bool{false, true} {
		for _, repo := range []string{src, shallow} {
			name := "plain"
			if isolated {
				name = "iso"
			}
			var setupErr error
			notes := captureStderr(t, func() { setupErr = setupForkBoundary(nil, repo, name, isolated) })
			if setupErr != nil {
				t.Fatalf("create fork (isolated %v) from %s: %v", isolated, repo, setupErr)
			}
			warned := strings.Contains(notes, "this checkout is a shallow clone, so fork "+name+" cannot be looped or merged") &&
				strings.Contains(notes, "git fetch --unshallow")
			if warned != (repo == shallow) {
				t.Errorf("isolated %v, shallow %v: warned %v:\n%s", isolated, repo == shallow, warned, notes)
			}
		}
	}
}
