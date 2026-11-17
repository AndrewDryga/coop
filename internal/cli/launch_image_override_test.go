package cli

import (
	"strings"
	"testing"
)

// A missing COOP_IMAGE is the operator's to pull or build: every coop build refuses under it, so
// the launch must not send them to one, and nothing is built.
func TestAMissingImageOverrideIsNotSentToCoopBuild(t *testing.T) {
	a, recorder := tagShimApp(t, newerBase, newerBase)
	a.cfg.RepoOverride = t.TempDir()
	a.cfg.ImageOverride = "operator/image:1"
	var err error
	captureStderr(t, func() {
		repo, img, resolveErr := a.resolveLaunchImage()
		err = resolveErr
		if err == nil {
			err = a.requireLaunchImage(repo, img)
		}
	})
	if err == nil || !strings.Contains(err.Error(), "COOP_IMAGE") || strings.Contains(err.Error(), "coop build") || builds(t, recorder) != 0 {
		t.Fatalf("a missing COOP_IMAGE = %v after %d builds; want it named, no coop build advice and nothing built", err, builds(t, recorder))
	}
}
