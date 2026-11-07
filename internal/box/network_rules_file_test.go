package box

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/ui"
)

const networkRulesFixture = "egress_rules:\n  - to: {domain: EXAMPLE.com}\n    protocol: tls\n    ports: [443]\n"

func TestNetworkRulesFileCannotLaunderRulesThroughOrdinaryMode(t *testing.T) {
	for _, mode := range []egress.Mode{egress.Open, egress.None} {
		t.Run(string(mode), func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			path := filepath.Join(t.TempDir(), "rules.yaml")
			if err := os.WriteFile(path, []byte(networkRulesFixture), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{ConfigDir: t.TempDir()}
			capture, err := AdmitNetwork(cfg, runtime.Runtime{Name: "must-not-execute"}, RunSpec{Repo: t.TempDir()},
				NetworkAdmission{InvocationMode: &mode, RulesFile: path})
			if capture != nil || err == nil || !strings.Contains(err.Error(), "--egress-rules only adds to filtered access") {
				t.Fatal("ordinary mode accepted rules or reached the runtime", err)
			}
			state, err := NetworkStatePath()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(state); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("conflicting rules published state", err)
			}
		})
	}
}

// Only a private file outside every agent mount grants. Any other is refused by name: the agent
// could have written it, and no review exists that could approve rules for one run.
func TestCaptureNetworkRulesFileClassifiesTheSource(t *testing.T) {
	for kind, want := range map[string]string{
		"private":         "",
		"repository":      "keep the rules file outside the project",
		"traversed-alias": "keep the rules file outside the project",
		"hard-link":       "must be yours alone: owned by you, with no other hard links, and not writable by others",
		"shared-writable": "must be yours alone",
	} {
		t.Run(kind, func(t *testing.T) {
			repo, private := t.TempDir(), t.TempDir()
			path := filepath.Join(private, "rules.yaml")
			if kind == "repository" {
				path = filepath.Join(repo, "rules.yaml")
			}
			if err := os.WriteFile(path, []byte(networkRulesFixture), 0o600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "traversed-alias":
				bridge := filepath.Join(repo, "bridge")
				if err := os.Symlink(private, bridge); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(bridge, "rules.yaml")
			case "hard-link":
				if err := os.Link(path, filepath.Join(repo, "linked.yaml")); err != nil {
					t.Fatal(err)
				}
			case "shared-writable":
				if err := os.Chmod(path, 0o666); err != nil {
					t.Fatal(err)
				}
			}
			capture, err := captureNetworkRulesFile(path, []string{repo})
			if want != "" {
				if capture != nil || err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), path) {
					t.Fatalf("capture = %#v, %v; want a refusal naming %s and %q", capture, err, path, want)
				}
				return
			}
			if err != nil || capture == nil || len(capture.rules) != 1 || capture.rules[0].To.Domain != "example.com" {
				t.Fatalf("capture = %#v error %v", capture, err)
			}
			rules := capture.rules
			// A later review or launch consumes the captured data, not the path.
			if err := os.WriteFile(path, []byte("egress_rules: []\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if len(rules) != 1 || rules[0].To.Domain != "example.com" {
				t.Fatal("capture changed with its source")
			}
		})
	}
}

func TestCaptureNetworkRulesFileRefusesInvalidSources(t *testing.T) {
	for _, kind := range []string{"absent", "directory", "fifo", "symlink", "oversize", "malformed", "parent-traversal"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "rules.yaml")
			var err error
			switch kind {
			case "directory":
				err = os.Mkdir(path, 0o700)
			case "fifo":
				err = syscall.Mkfifo(path, 0o600)
			case "symlink":
				err = os.WriteFile(filepath.Join(root, "target"), []byte(networkRulesFixture), 0o600)
				if err == nil {
					err = os.Symlink("target", path)
				}
			case "oversize":
				err = os.WriteFile(path, []byte(strings.Repeat(" ", egress.MaxDocumentBytes+1)), 0o600)
			case "malformed":
				err = os.WriteFile(path, []byte("egress_rules: [{to: {domain: example.com}, trusted: true}]"), 0o600)
			case "parent-traversal":
				path = root + "/child/../rules.yaml"
			}
			if err != nil {
				t.Fatal(err)
			}
			if capture, err := captureNetworkRulesFile(path, nil); err == nil || capture != nil {
				t.Fatalf("invalid source granted authority: %v %v", capture, err)
			}
		})
	}
}

func TestNetworkRulesFileRepositoryRequestRefusesBeforeRuntime(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := t.TempDir()
	path := filepath.Join(repo, "network.yaml")
	if err := os.WriteFile(path, []byte(networkRulesFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ConfigDir: t.TempDir()}
	capture, err := AdmitNetwork(cfg, runtime.Runtime{Name: "must-not-execute"}, RunSpec{Repo: repo}, NetworkAdmission{RulesFile: path})
	if err == nil {
		_ = capture.Close()
		t.Fatal("repository rules bypassed approval")
	}
	// No review can approve rules for one run, so sending the person to coop approve would be a dead end.
	if want := "egress rules file " + path + " is inside "; !strings.HasPrefix(err.Error(), want) ||
		!strings.HasSuffix(err.Error(), ", which the box can reach — keep the rules file outside the project") {
		t.Fatal("wrong refusal", err)
	}
	root, err := NetworkStatePath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unapproved rules created authority state", err)
	}
}

// The authority root itself must be outside every agent mount, and the check
// happens before any state, key or runtime inspection exists.
func TestNetworkAuthorityRootInsideAnAgentMountRefusesBeforeState(t *testing.T) {
	repo := t.TempDir()
	t.Setenv("XDG_STATE_HOME", repo)
	mode := egress.Filtered
	cfg := &config.Config{ConfigDir: t.TempDir()}
	capture, err := AdmitNetwork(cfg, runtime.Runtime{Name: "must-not-execute"}, RunSpec{Repo: repo}, NetworkAdmission{InvocationMode: &mode})
	if err == nil {
		_ = capture.Close()
		t.Fatal("authority inside an agent mount was accepted")
	}
	if !strings.Contains(err.Error(), "outside every directory an agent can reach") {
		t.Fatal("wrong refusal", err)
	}
	root, err := NetworkStatePath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("exposed authority root was created", err)
	}
}

// A rule flag on a run something else made open or offline is refused at the launch, as a failure
// (exit 1) rather than a usage error, and the refusal names what chose the mode: the person may not
// know COOP_EGRESS is set, or what the project or its approval says.
func TestRuleFlagsNameWhatMadeTheRunOpenOrOffline(t *testing.T) {
	open, offline, filtered := egress.Open, egress.None, egress.Filtered
	spec, flags := RunSpec{Cmd: []string{"true"}}, NetworkAdmission{Domains: []string{"example.com"}}
	for _, tc := range []struct {
		input networkstate.Admission
		mode  egress.Mode
		want  string
	}{
		{networkstate.Admission{InvocationMode: &open}, open, "and --egress open makes this run open"},
		{networkstate.Admission{HostPreference: &open, ProjectMode: &offline}, open, "and COOP_EGRESS=open makes this run open"},
		{networkstate.Admission{HostPreference: &offline}, offline, "and COOP_EGRESS=offline makes this run offline"},
		{networkstate.Admission{ProjectMode: &offline}, offline, "and box.egress: offline in .agent/project.yaml makes this run offline"},
		{networkstate.Admission{HostPreference: &filtered}, open, "and this project's approval makes this run open"},
	} {
		err := checkRuleFlags(spec, flags, tc.input, tc.mode)
		var usage *ui.UsageError
		if err == nil || errors.As(err, &usage) || !strings.Contains(err.Error(), "--allow-domain only adds to filtered access, "+tc.want+"\n") {
			t.Errorf("%s run refused as %v", tc.mode, err)
		}
	}
	if err := checkRuleFlags(spec, flags, networkstate.Admission{}, filtered); err != nil {
		t.Fatalf("a filtered run refused its rule flags: %v", err)
	}
}

// Debian, Ubuntu and Fedora create files at 0664 in the user's own group, where only the owner can use
// the write bit. Any sign that someone else could use it, or that the files cannot say, is a refusal.
func TestPrivateGroupIsOnlyTheOwnersOwnGroup(t *testing.T) {
	const me = "root:x:0:0::/root:/bin/sh\nme:x:1000:1000::/home/me:/bin/bash\nother:x:1001:1001::/home/other:/bin/bash\n"
	const groups = "root:x:0:\nme:x:1000:\nother:x:1001:\nstaff:x:50:me,other\n"
	for _, tc := range []struct {
		name          string
		passwd, group string
		uid           int
		gid           uint32
		want          bool
	}{
		{"own group", me, groups, 1000, 1000, true},
		{"own group listing only the owner", me, "me:x:1000:me\n", 1000, 1000, true},
		{"a shared group", me, groups, 1000, 50, false},
		{"someone else's group", me, groups, 1000, 1001, false},
		{"another member", me, "me:x:1000:me,other\n", 1000, 1000, false},
		{"another user's primary group", me + "guest:x:1002:1000::/home/guest:/bin/sh\n", groups, 1000, 1000, false},
		{"a group not named after the user", me, "devs:x:1000:\n", 1000, 1000, false},
		{"a second entry for the group", me, groups + "devs:x:1000:\n", 1000, 1000, false},
		{"a user the file does not hold", me, groups, 2000, 1000, false},
		{"a group the file does not hold", me, "root:x:0:\n", 1000, 1000, false},
		{"a second entry for the user", me + "me2:x:1000:1000::/home/me:/bin/sh\n", groups, 1000, 1000, false},
		{"a primary group that differs", "me:x:1000:50::/home/me:/bin/bash\n", groups, 1000, 1000, false},
	} {
		if got := privateGroup(tc.passwd, tc.group, tc.uid, tc.gid); got != tc.want {
			t.Errorf("%s: privateGroup = %v, want %v", tc.name, got, tc.want)
		}
	}
	for _, malformed := range []string{"+::::::\n", "guest:x:1002:01000::/home/guest\n", "guest:x:abc:1000::/home/guest:/bin/sh\n"} {
		if privateGroup(me+malformed, groups, 1000, 1000) {
			t.Errorf("an unreadable passwd line %q did not refuse", malformed)
		}
	}
	for _, malformed := range []string{"+:::\n", "me2:x:1000\n", "me2:x:01000x:\n"} {
		if privateGroup(me, groups+malformed, 1000, 1000) {
			t.Errorf("an unreadable group line %q did not refuse", malformed)
		}
	}
	for text, want := range map[string]bool{
		"passwd: files systemd\ngroup: files systemd\nhosts: files dns\n": true,
		"# comment\npasswd: files\ngroup: files [NOTFOUND=return]\n":      true,
		"passwd: files sss systemd\ngroup: files systemd\n":               false,
		"passwd: files\ngroup: files ldap\n":                              false,
		"passwd: compat\ngroup: compat\n":                                 false,
		"passwd: files\n":                                                 false,
		"":                                                                false,
	} {
		if got := localAccountsOnly(text); got != want {
			t.Errorf("localAccountsOnly(%q) = %v, want %v", text, got, want)
		}
	}
	file, err := os.Create(filepath.Join(t.TempDir(), "rules.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for _, tc := range []struct {
		perm os.FileMode
		want bool
	}{{0o644, true}, {0o600, true}, {0o646, false}, {0o666, false}, {0o602, false}} {
		if got := rulesFileWritableOnlyByOwner(file, tc.perm, 1<<31); got != tc.want {
			t.Errorf("mode %o: writable only by owner = %v, want %v", tc.perm, got, tc.want)
		}
	}
	if rulesFileWritableOnlyByOwner(file, 0o664, 1<<31) {
		t.Error("group write in a group the host's files do not hold was allowed")
	}
}
