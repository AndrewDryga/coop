package cli

import (
	"strings"
	"testing"
)

// The runtime and integration family's pages — run, shell, ACP, every sessions page, sign,
// completion, prompt and version — are what `coop help <path>` prints, with no all-commands
// footer: these pages end with their own pointer or need none, so the generic footer would be a
// second answer. The page bytes themselves are pinned by docs/cli.md (make docs-check).
func TestApprovedRuntimeHelpRoutes(t *testing.T) {
	cfg := freshConfig(t)
	for _, path := range [][]string{
		{"run"}, {"shell"}, {"acp"}, {"sign"}, {"completion"}, {"prompt"}, {"version"},
		{"sessions"}, {"sessions", "connect"},
		{"sessions", "doctor"}, {"sessions", "compact"},
	} {
		name := strings.Join(path, " ")
		t.Run(name, func(t *testing.T) {
			out := captureStdout(t, func() {
				if code, err := helpForPath(path, cfg, true); code != 0 || err != nil {
					t.Fatalf("coop help %s exited (%d, %v)", name, code, err)
				}
			})
			if strings.Contains(out, "Run 'coop help' for all commands.") {
				t.Errorf("coop help %s appended the generic all-commands footer:\n%s", name, out)
			}
			if want := manualPage(name); !strings.Contains(out, want) {
				t.Errorf("coop help %s did not print its manual page:\n%s", name, out)
			}
		})
	}
}
