package sessionsvc

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The three-line git/path readers every git-touching package's tests grow. They are per-package
// on purpose: internal/cli has its own, and a shared leaf for `git rev-parse` plus os.Lstat would
// be a dependency nobody gains anything from. The hermetic repo they run against IS shared —
// that one has real setup worth getting wrong once (internal/testutil/gitrepo).

func gitOut(dir string, args ...string) string {
	out, err := exec.Command("git", gitArgs(dir, args)...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// reviewSourceSnapshot is everything a review must leave untouched in a repo it reads: the commit,
// the branch, the working tree, and every ref.
func reviewSourceSnapshot(dir string) string {
	return strings.Join([]string{
		gitOut(dir, "rev-parse", "HEAD"),
		gitOut(dir, "rev-parse", "--abbrev-ref", "HEAD"),
		gitOut(dir, "status", "--porcelain"),
		gitOut(dir, "show-ref"),
	}, "\n")
}

// sessionUnixHTTPClient dials the control socket the way `coop sessions doctor` does, so a test can
// prove ListenSocket produced a socket a plain HTTP client reaches. (The doctor command owns the
// real one — it prints the verdict, so it lives with the CLI.)
func sessionUnixHTTPClient(socket string) *http.Client {
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}
	return &http.Client{Transport: transport, Timeout: 5 * time.Second}
}

func assertReviewScratchEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("review scratch leaked entries: %v", entries)
	}
}
