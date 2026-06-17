package cli

import (
	"slices"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

func TestLoopAgent(t *testing.T) {
	if got, err := loopAgent(nil); err != nil || got != "claude" {
		t.Errorf("loopAgent(nil) = (%q, %v), want claude", got, err)
	}
	for _, ag := range []string{"claude", "codex", "gemini"} {
		if got, err := loopAgent([]string{ag}); err != nil || got != ag {
			t.Errorf("loopAgent(%q) = (%q, %v), want %q", ag, got, err, ag)
		}
	}
	if _, err := loopAgent([]string{"bogus"}); err == nil {
		t.Error("loopAgent(bogus): want error")
	}
}

func TestParseGovernor(t *testing.T) {
	a := &app{cfg: &config.Config{FusionGovernor: "codex"}}
	cases := []struct {
		name     string
		args     []string
		wantGov  string
		wantRest []string
	}{
		{"default governor, no args", nil, "codex", nil},
		{"positional governor", []string{"claude"}, "claude", nil},
		{"positional governor + passthrough", []string{"gemini", "exec"}, "gemini", []string{"exec"}},
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

func TestExtractConsult(t *testing.T) {
	cases := []struct {
		args     []string
		want     bool
		wantRest []string
	}{
		{nil, false, nil},
		{[]string{"-p", "hi"}, false, []string{"-p", "hi"}},
		{[]string{"--consult"}, true, nil},
		{[]string{"--consult", "-p", "hi"}, true, []string{"-p", "hi"}},
		{[]string{"-p", "hi", "--consult"}, true, []string{"-p", "hi"}},
	}
	for _, c := range cases {
		got, rest := extractConsult(c.args)
		if got != c.want || !slices.Equal(rest, c.wantRest) {
			t.Errorf("extractConsult(%v) = (%v, %v), want (%v, %v)", c.args, got, rest, c.want, c.wantRest)
		}
	}
}
