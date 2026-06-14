package runtime

import "testing"

func TestDetectOverride(t *testing.T) {
	r, err := Detect("podman")
	if err != nil || r.Name != "podman" {
		t.Fatalf("Detect(podman) = %+v, %v", r, err)
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
