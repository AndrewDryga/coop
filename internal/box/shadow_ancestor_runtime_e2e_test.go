//go:build boxruntimee2e

package box

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/AndrewDryga/coop/internal/runtime"
)

func TestRuntimeComposeHiddenAncestors(t *testing.T) {
	rt, err := runtime.Detect(os.Getenv("COOP_RUNTIME"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(ServiceStateRootEnv, t.TempDir())
	for _, tc := range hiddenAncestorCases {
		t.Run(tc.name, func(t *testing.T) {
			repo, file := hiddenAncestorFixture(t, tc.policy, tc.rules, tc.parent)
			args, cleanup, _, err := snapshotComposeArgs(repo, file, false)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			defer func() {
				var out bytes.Buffer
				if err := runCompose(rt, &out, &out, "down", append(append([]string(nil), args...), "down")); err != nil {
					t.Errorf("fixture cleanup: %v\n%s", err, out.String())
				}
			}()
			const probe = `set -eu
for path in /direct /alias; do
  test -f "$path" && test ! -s "$path" || { echo "hidden file exposed: $path"; exit 1; }
  if (printf denied > "$path") 2>/dev/null; then echo "hidden file writable: $path"; exit 1; fi
done
test ! -e /directory/notes.txt || { echo "hidden directory exposed"; exit 1; }
if (printf denied > /directory/new) 2>/dev/null; then echo "hidden directory writable"; exit 1; fi
test "$(cat /public/notes.txt)" = 'public marker'
printf 'ordinary write\n' > /public/notes.txt
echo 'hidden descendants protected; ordinary write succeeded'
`
			var out bytes.Buffer
			runArgs := append(append([]string(nil), args...), "run", "--rm", "--no-deps", "--pull", "never", "--cap-drop", "ALL", "-T", "probe", "sh", "-ec", probe)
			if err := runCompose(rt, &out, &out, "run", runArgs); err != nil {
				t.Fatalf("descendant protection: %v\n%s", err, out.String())
			}
			for rel, want := range map[string]string{
				tc.parent + "/notes.txt": "hidden marker\n", tc.parent + "/sub/notes.txt": "hidden marker\n",
				"public/notes.txt": "ordinary write\n",
			} {
				body, err := os.ReadFile(filepath.Join(repo, rel))
				if err != nil || string(body) != want {
					t.Fatalf("host %s = %q, %v; want %q", rel, body, err, want)
				}
			}
			t.Log("direct file, directory and symlink alias protected; host writeback verified")
		})
	}
}
