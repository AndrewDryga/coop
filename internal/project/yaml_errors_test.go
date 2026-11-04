package project

import (
	"strings"
	"testing"
)

func TestParseNamesSettingsNotGoTypes(t *testing.T) {
	for _, tc := range []struct{ file, want string }{
		{"nosuchkey: 1\n", `line 1: unknown setting "nosuchkey"`},
		{"serve:\n  ports: [abc]\n", `line 2: "abc" should be a number`},
		{"box: nope\n", `line 1: "nope" should be a group of settings`},
		{"box:\n  egress_rules: nope\n", `line 2: "nope" should be a list`},
	} {
		_, err := Parse([]byte(tc.file))
		if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "project.Project") || strings.Contains(err.Error(), "!!") {
			t.Errorf("Parse(%q) = %v, want %q", tc.file, err, tc.want)
		}
	}
}
