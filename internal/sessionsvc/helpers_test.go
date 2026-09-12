package sessionsvc

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/tasks"
)

// Ordinary fixtures need room for small temporary workspaces, not the production worker's
// share of the developer's disk. Storage-policy tests still set their own explicit limits.
func newSessionServiceWithTestStorage(t *testing.T, cfg Config) (*Service, error) {
	t.Helper()
	if cfg.StorageLimits == nil {
		limits := storageTestLimits(t)
		limits.GraceWindow = DefaultStorageGraceWindow
		limits.MeasureInterval = DefaultStorageMeasureInterval
		cfg.StorageLimits = &limits
	}
	return NewService(cfg)
}

// A recorded failure cannot become success by polling longer. Keep the real terminal
// diagnostic at the call site instead of waiting for the generic fixture deadline.
func sessionOperationReached(t interface {
	Helper()
	Fatalf(string, ...any)
}, op session.Operation, want session.OperationState) bool {
	t.Helper()
	if op.State == want {
		return true
	}
	switch op.State {
	case "", session.OperationReserved, session.OperationRunning:
		return false
	default:
		t.Fatalf("operation %s ended %s, want %s: %s: %s", op.ID, op.State, want, op.ErrorCode, op.ErrorDetail)
		return false
	}
}

type operationTestFailure struct{ message string }

func (*operationTestFailure) Helper() {}
func (f *operationTestFailure) Fatalf(format string, args ...any) {
	f.message = fmt.Sprintf(format, args...)
}

func TestSessionOperationWaitReportsTerminalFailures(t *testing.T) {
	for _, state := range []session.OperationState{
		"", session.OperationReserved, session.OperationRunning, session.OperationSucceeded,
		session.OperationFailed, session.OperationUncertain,
	} {
		t.Run(string(state), func(t *testing.T) {
			failure := &operationTestFailure{}
			op := session.Operation{ID: "fixture-create", State: state,
				ErrorCode: session.CodeStorageUnavailable, ErrorDetail: "fixture reserve exhausted"}
			if got := sessionOperationReached(failure, op, session.OperationSucceeded); got != (state == session.OperationSucceeded) {
				t.Fatalf("operation readiness = %v for %s", got, state)
			}
			if state == session.OperationFailed || state == session.OperationUncertain {
				for _, want := range []string{op.ID, string(state), string(op.ErrorCode), op.ErrorDetail} {
					if !strings.Contains(failure.message, want) {
						t.Fatalf("terminal diagnostic %q lacks %q", failure.message, want)
					}
				}
				failure.message = ""
				if !sessionOperationReached(failure, op, state) || failure.message != "" {
					t.Fatal("an explicitly expected failure did not satisfy the wait")
				}
			} else if failure.message != "" {
				t.Fatalf("non-failure diagnostic = %q", failure.message)
			}
		})
	}
}

func TestSessionFixturesConfigureStorageWithoutChangingProductionDefaults(t *testing.T) {
	cfg := Config{StateRoot: filepath.Join(t.TempDir(), "state"), Policies: bareTestPolicies()}
	ordinary, err := newSessionServiceWithTestStorage(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ordinary.Stop() })
	limits := ordinary.storageLimitsFor(500 << 30)
	if limits.ReserveBytes != 1<<20 || limits.GraceWindow != DefaultStorageGraceWindow ||
		limits.MeasureInterval != DefaultStorageMeasureInterval {
		t.Fatalf("ordinary fixture storage = %+v", limits)
	}
	// Bypass the fixture helper: omitting Config.StorageLimits must still select production policy.
	cfg.StateRoot = filepath.Join(t.TempDir(), "state")
	production, err := NewService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = production.Stop() })
	defaults := production.storageLimitsFor(500 << 30)
	if defaults != DefaultStorageLimits(500<<30) || defaults.ReserveBytes != 25<<30 ||
		defaults.LowWatermarkBytes != 450<<30 || defaults.HighWatermarkBytes != 475<<30 {
		t.Fatalf("production defaults = %+v", defaults)
	}
	cfg.StateRoot = filepath.Join(t.TempDir(), "state")
	cfg.StorageLimits = &defaults
	configured, err := newSessionServiceWithTestStorage(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = configured.Stop() })
	if got := configured.storageLimitsFor(500 << 30); got != defaults {
		t.Fatalf("fixture replaced explicit storage policy: %+v", got)
	}
}

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

func mustCurrentTask(t testing.TB, root, id string) (tasks.Item, bool) {
	t.Helper()
	item, ok, err := tasks.CurrentTask(root, id)
	if err != nil {
		t.Fatalf("CurrentTask(%q, %q): %v", root, id, err)
	}
	return item, ok
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
