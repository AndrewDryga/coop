package config

import (
	"path/filepath"
	"testing"
)

func TestNativeHomesNeverRetargetCredentialAuthority(t *testing.T) {
	cfg := &Config{ConfigDir: t.TempDir()}
	cfg.SetActiveProfile("codex", "work")
	authority := cfg.AgentDir("codex")
	home := filepath.Join(t.TempDir(), "home")
	selections := map[string]string{"codex": home}
	run := cfg.WithNativeHomes(selections)
	selections["codex"] = t.TempDir()
	if got, err := run.NativeHome("codex"); err != nil || got != home {
		t.Fatalf("native selection changed: %q %v", got, err)
	}
	if run.AgentDir("codex") != authority || run.AgentProfileDir("codex", "work") != authority {
		t.Fatal("runtime home changed host credential authority")
	}
	if _, err := run.AgentSettingsDir("claude"); err == nil {
		t.Fatal("unselected runtime provider fell back to host credentials")
	}
	if _, err := cfg.NativeHome("codex"); err == nil {
		t.Fatal("host config implicitly authorizes a native mount")
	}
	copy := run.Clone()
	copy.nativeHomes["codex"] = t.TempDir()
	if got, _ := run.NativeHome("codex"); got != home {
		t.Fatal("cloned config aliases native selections")
	}
	if _, err := cfg.WithNativeHomes(nil).AgentSettingsDir("codex"); err == nil {
		t.Fatal("empty runtime selection fell back to host credentials")
	}
}
