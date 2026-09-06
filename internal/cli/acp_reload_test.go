package cli

import (
	"errors"
	"strings"
	"testing"
)

// A failed pre-exec sweep must not end the editor session: the reload proceeds with a warning and
// hands the old supervisor id to the next generation, which retries the sweep. A clean sweep
// carries nothing.
func TestACPReloadContinuesPastACleanupFailure(t *testing.T) {
	if warning, prior := acpReloadAfterCleanup("super-1", nil); warning != "" || prior != "" {
		t.Fatalf("clean sweep = (%q, %q); want nothing to warn about or carry", warning, prior)
	}
	warning, prior := acpReloadAfterCleanup("super-1", errors.New("docker ps: connection refused"))
	if prior != "super-1" || !strings.Contains(warning, "connection refused") || !strings.Contains(warning, "reloading anyway") {
		t.Fatalf("failed sweep = (%q, %q); want a warning naming the failure and the old supervisor carried", warning, prior)
	}
}
