package forkspace

import (
	"slices"
	"strings"
	"testing"
)

// Mirror the argv built by workerconnector/job_sources.go and publication.go,
// including the diagnostic probes used by their integration tests. Different
// commits, paths and authorized remotes are data, not different command policies.
func TestWorkerGitCommandPolicy(t *testing.T) {
	const head = "1111111111111111111111111111111111111111"
	const base = "2222222222222222222222222222222222222222"
	const remote = "https://github.com/example/repo.git"
	const endpoint = remote + "/info/lfs"
	const seed = "refs/coop/staged-seed"
	cases := []struct {
		name string
		args []string
	}{
		{"init", []string{"init", "--quiet", "--template=", "--object-format=sha1", "/private/source"}},
		{"remote", []string{"remote", "add", "origin", remote}},
		{"download default", []string{"fetch", "--progress", "--no-tags", "origin", "+refs/heads/main:refs/remotes/origin/job-default"}},
		{"fetch default", []string{"fetch", "--quiet", "--no-tags", "origin", "+refs/heads/main:refs/remotes/origin/job-default"}},
		{"fetch selected", []string{"fetch", "--quiet", "--no-tags", "origin", "+refs/heads/topic:refs/remotes/origin/job-selected"}},
		{"fetch commit or child", []string{"fetch", "--quiet", "--no-tags", "origin", head}},
		{"refresh lookup", []string{"ls-remote", "--exit-code", "--refs", "--", remote, "refs/heads/main"}},
		{"refresh object", []string{"cat-file", "-e", head + "^{commit}"}},
		{"refresh fetch", []string{"fetch", "--quiet", "--no-write-fetch-head", "--no-tags", "--", remote, head}},
		{"refresh head", []string{"rev-parse", head + "^{commit}"}},
		{"branch head", []string{"rev-parse", "refs/remotes/origin/job-default^{commit}"}},
		{"branch ancestry", []string{"merge-base", "--is-ancestor", base, head}},
		{"merge base", []string{"merge-base", base, head}},
		{"admitted tree", []string{"rev-parse", head + "^{tree}"}},
		{"checkout", []string{"checkout", "--quiet", "--detach", head}},
		{"seed head", []string{"rev-parse", "--verify", "refs/remotes/origin/job-default^{commit}"}},
		{"seed ref", []string{"update-ref", seed, head}},
		{"remove seed ref", []string{"update-ref", "-d", seed}},
		{"identity name", []string{"config", "user.name", "Coop"}},
		{"identity email", []string{"config", "user.email", "coop@localhost"}},
		{"verify head", []string{"rev-parse", "HEAD"}},
		{"verify tree", []string{"rev-parse", "HEAD^{tree}"}},
		{"verify status", []string{"-c", "core.attributesFile=/dev/null", "status", "--porcelain=v2", "--untracked-files=all", "--no-renames", "--ignore-submodules=all", "-z"}},
		{"status LFS attribute", []string{"check-attr", "-z", "--cached", "filter", "--", "large.bin"}},
		{"status LFS pointer", []string{"cat-file", "blob", head}},
		{"LFS fetch", []string{"-c", "lfs.url=" + endpoint, "-c", "remote.origin.lfsurl=" + endpoint,
			"-c", "lfs.basictransfersonly=true", "-c", "lfs.standalonetransferagent=",
			"-c", "lfs.remote.autodetect=false", "-c", "lfs.remote.searchall=false",
			"-c", "lfs.transfer.enablehrefrewrite=false", "-c", "lfs.skipdownloaderrors=false",
			"-c", "lfs.fetchrecentalways=false", "lfs", "fetch", "-I", "", "-X", "", "origin", head, base}},
		{"publication head", []string{"rev-parse", "--verify", head + "^{commit}"}},
		{"publication tree", []string{"rev-parse", "--verify", head + "^{tree}"}},
		{"publication lookup", []string{"-c", "http.followRedirects=false", "ls-remote", "--refs", remote, "refs/heads/topic"}},
		{"publication push", []string{"-c", "http.followRedirects=false", "push", "--quiet", "--no-verify", "--force-with-lease=refs/heads/topic:" + base, remote, head + ":refs/heads/topic"}},
		{"publication LFS", []string{"-c", "lfs.url=" + endpoint, "-c", "lfs.pushurl=" + endpoint,
			"-c", "lfs.basictransfersonly=true", "-c", "lfs.standalonetransferagent=",
			"-c", "lfs.remote.autodetect=false", "-c", "lfs.remote.searchall=false",
			"-c", "lfs.transfer.enablehrefrewrite=false", "lfs", "push", "--object-id", remote, "--stdin"}},
		{"committer diagnostic", []string{"-c", "user.useConfigOnly=true", "var", "GIT_COMMITTER_IDENT"}},
		{"LFS diagnostic", []string{"lfs", "env"}},
		{"credential scope diagnostic", []string{"config", "--get-urlmatch", "http.extraheader", remote}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := nonRecursiveGitArgs(tc.args)
			if err != nil {
				t.Fatal(err)
			}
			for _, setting := range []string{"submodule.recurse=false", "fetch.recurseSubmodules=false", "push.recurseSubmodules=no", "diff.submodule=short"} {
				if i := slices.Index(got, setting); i < 1 || got[i-1] != "-c" {
					t.Errorf("missing safety setting %s in %v", setting, got)
				}
			}
			verb := 0
			for tc.args[verb] == "-c" {
				verb += 2
			}
			// All caller settings survive, followed by the enforced settings.
			if !slices.Equal(got[:verb], tc.args[:verb]) {
				t.Fatalf("caller settings changed: %v", got)
			}
			command := slices.Index(got, tc.args[verb])
			want := slices.Clone(tc.args[verb:])
			switch tc.args[verb] {
			case "push":
				want = append(want, "--recurse-submodules=no")
			case "status":
				want = append(want, "--ignore-submodules=all")
			}
			if !slices.Equal(got[command:], want) {
				t.Fatalf("worker command changed: got %v, want %v", got[command:], want)
			}
		})
	}
}

func TestHostGitRejectsUnreviewedWorkerCommands(t *testing.T) {
	for _, args := range [][]string{
		{"push", "--recurse-submodules=on-demand", "origin", "HEAD"},
		{"push", "--recurse-submodules=check", "origin", "HEAD"},
		{"push", "--recurs=on-demand", "origin", "HEAD"},
		{"lfs", "push", "origin", "HEAD"}, {"lfs", "push", "--all", "origin"},
		{"lfs", "push", "--object-id", "origin"}, {"lfs", "push", "--object-id", "", "--stdin"},
		{"lfs", "push", "--object-id", "--all", "--stdin"},
		{"lfs", "push", "--object-id", "origin", "--stdin", "--all"},
		{"lfs", "env", "--extra"}, {"var", "-l"}, {"var", "GIT_EDITOR"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if _, err := nonRecursiveGitArgs(args); err == nil {
				t.Fatalf("unreviewed command accepted: %v", args)
			}
		})
	}
}

// Caller configuration cannot undo the policy, including when -- ends option parsing.
func TestHostGitPushOverridesRecursion(t *testing.T) {
	args := []string{"-c", "push.recurseSubmodules=on-demand", "push", "--quiet", "--", "origin", "HEAD:refs/heads/topic"}
	got, err := nonRecursiveGitArgs(args)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Index(got, "push.recurseSubmodules=no") <= slices.Index(got, "push.recurseSubmodules=on-demand") {
		t.Fatalf("caller config won: %v", got)
	}
	boundary := slices.Index(got, "--")
	if boundary < 1 || got[boundary-1] != "--recurse-submodules=no" {
		t.Fatalf("push recursion flag is not an option: %v", got)
	}
}
