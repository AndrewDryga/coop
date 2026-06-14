package cli

import (
	"slices"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

func TestParseGovernor(t *testing.T) {
	a := &app{cfg: &config.Config{FusionGovernor: "codex"}}
	cases := []struct {
		name     string
		args     []string
		wantGov  string
		wantRest []string
	}{
		{"default governor, no args", nil, "codex", nil},
		{"--governor flag", []string{"--governor", "claude"}, "claude", nil},
		{"--governor=value", []string{"--governor=gemini"}, "gemini", nil},
		{"passthrough args keep order", []string{"exec", "foo"}, "codex", []string{"exec", "foo"}},
		{"governor + passthrough", []string{"--governor", "claude", "exec", "foo"}, "claude", []string{"exec", "foo"}},
		{"-- passes the rest through verbatim", []string{"--governor=claude", "--", "-p", "hi"}, "claude", []string{"-p", "hi"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gov, rest := a.parseGovernor(c.args)
			if gov != c.wantGov {
				t.Errorf("governor = %q, want %q", gov, c.wantGov)
			}
			if !slices.Equal(rest, c.wantRest) {
				t.Errorf("rest = %v, want %v", rest, c.wantRest)
			}
		})
	}
}
