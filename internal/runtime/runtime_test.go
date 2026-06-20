package runtime

import (
	"os/exec"
	"strings"
	"testing"
)

// Detect validates a COOP_RUNTIME override up front, so a bogus value fails clearly here instead
// of later as a misleading "image not built".
func TestDetectOverride(t *testing.T) {
	// A known runtime that's actually installed is accepted on PATH alone.
	accepted := false
	for _, rt := range []string{"docker", "podman", "container"} {
		if _, err := exec.LookPath(rt); err == nil {
			if got, err := Detect(rt); err != nil || got.Name != rt {
				t.Errorf("Detect(%q) = (%+v, %v), want it accepted", rt, got, err)
			}
			accepted = true
			break
		}
	}
	if !accepted {
		t.Skip("no container runtime installed to test the accept path")
	}
	// A name that doesn't resolve on PATH → a clear "not found".
	if _, err := Detect("definitely-not-a-runtime-xyz"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("a bogus override should be 'not found', got %v", err)
	}
	// An executable that resolves but isn't a runtime (fails --version) → "not usable".
	if _, err := exec.LookPath("false"); err == nil {
		if _, err := Detect("false"); err == nil || !strings.Contains(err.Error(), "usable") {
			t.Errorf("a non-runtime override (`false`) should be 'not usable', got %v", err)
		}
	}
}

func TestRunExitCodes(t *testing.T) {
	// Use coreutils true/false as stand-in runtimes to exercise exit handling
	// without needing a real container daemon.
	if code, err := (Runtime{Name: "true"}).Run(nil, nil, nil); err != nil || code != 0 {
		t.Errorf("true -> code=%d err=%v, want 0,nil", code, err)
	}
	if code, err := (Runtime{Name: "false"}).Run(nil, nil, nil); err != nil || code != 1 {
		t.Errorf("false -> code=%d err=%v, want 1,nil", code, err)
	}
	if _, err := (Runtime{Name: "agent-no-such-binary-xyz"}).Run(nil, nil, nil); err == nil {
		t.Error("missing binary should return a start error")
	}
}

func TestSilent(t *testing.T) {
	if !(Runtime{Name: "true"}).Silent() {
		t.Error("true should be silent-success")
	}
	if (Runtime{Name: "false"}).Silent() {
		t.Error("false should be silent-failure")
	}
}
