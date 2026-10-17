//go:build !providere2e && !providerlivee2e

package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/teststate"
)

// hostCLIs are the programs no unit test may run from the developer's PATH: the provider CLIs Coop
// drives, the gcloud the Gemini usage report asks for the developer's Google token, and the
// container runtimes Coop detects. A test that ran one depended on what that machine has installed
// and signed in to: `coop models` ran the real `codex debug models`, which reads ~/.codex, and
// runtime detection ran the real `docker info`. Apple's container is stubbed too: detection asks
// Docker's daemon only when both runtimes are installed, so with both stubs a test that leaves
// detection to the host is caught on every machine.
var hostCLIs = []string{"codex", "claude", "gemini", "grok", "gcloud", "docker", "container"}

// hostCLIStub is a stub's script, formatted with the program's name and the quoted path of the
// call log: it records its call there, then fails.
const hostCLIStub = `#!/bin/sh
printf '%%s\n' "%[1]s $*" >> %[2]s
echo '%[1]s is a stub: unit tests must not run the real one' >&2
exit 1
`

// guardHostCLIs runs the tests with a failing stub for each host CLI first on PATH. A stub records
// its call, and the binary fails after its tests when any stub ran. The tagged suites drive the
// real programs or their own fixtures on purpose, so they build without the guard.
func guardHostCLIs(tests func() int) int {
	stubs, calls := hostCLIStubs(), filepath.Join(teststate.Dir(), "host-cli-calls")
	if _, err := os.Stat(stubs); err == nil {
		// A test ran this binary again as a helper with its state folder, so the stubs on the
		// inherited PATH are the parent's, which reports their calls.
		return tests()
	}
	if err := writeHostCLIStubs(stubs, calls); err != nil {
		fmt.Fprintln(os.Stderr, "host CLI guard:", err)
		return 1
	}
	if err := os.Setenv("PATH", stubs+string(os.PathListSeparator)+os.Getenv("PATH")); err != nil {
		fmt.Fprintln(os.Stderr, "host CLI guard:", err)
		return 1
	}
	code := tests()
	ran, err := os.ReadFile(calls)
	if os.IsNotExist(err) {
		return code
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "host CLI guard:", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "\nUnit tests ran programs they must not take from this machine's PATH (hostclis_test.go):\n"+
		"  %s\nGive each test that ran one a seam: an app with rt and rtSet, a.acpModels or a models\n"+
		"cache that isn't due, or a PATH of its own.\n", strings.ReplaceAll(strings.TrimSpace(string(ran)), "\n", "\n  "))
	return 1
}

// hostCLIStubs is the folder of the guard's stubs, inside the test binary's state folder.
func hostCLIStubs() string { return filepath.Join(teststate.Dir(), "host-clis") }

func writeHostCLIStubs(dir, calls string) error {
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	log := "'" + strings.ReplaceAll(calls, "'", `'\''`) + "'"
	for _, name := range hostCLIs {
		if err := os.WriteFile(filepath.Join(dir, name), fmt.Appendf(nil, hostCLIStub, name, log), 0o700); err != nil {
			return err
		}
	}
	return nil
}

// The guard holds only while every host CLI resolves to its stub; a test binary that lost the
// stubs from PATH would pass while running the real programs.
func TestHostCLIsResolveToStubs(t *testing.T) {
	for _, name := range hostCLIs {
		if path, err := exec.LookPath(name); err != nil || filepath.Dir(path) != hostCLIStubs() {
			t.Errorf("%s resolves to %q (%v), want the stub in %s", name, path, err, hostCLIStubs())
		}
	}
}
