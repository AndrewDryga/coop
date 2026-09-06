package forkspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The projected config keeps what git needs to operate on the repository and nothing it would
// execute: drivers, includes, aliases, credential helpers, hook paths, and submodule settings are
// gone; remotes, branches, identity, and extensions survive with their exact values.
func TestTrustedGitConfigProjectsOnlyOperationalData(t *testing.T) {
	dir := t.TempDir()
	local := filepath.Join(dir, "config")
	include := filepath.Join(dir, "driver.config")
	if err := os.WriteFile(include, []byte("[filter \"x\"]\n\tclean = /evil\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	body := `[core]
	repositoryformatversion = 0
	filemode = true
	bare = false
	logallrefupdates = true
	hooksPath = /evil/hooks
	fsmonitor = /evil/fsmonitor
	pager = /evil/pager
	excludesfile = /etc/passwd
	worktree = /elsewhere
[include]
	path = ` + include + `
[includeIf "gitdir:/"]
	path = ` + include + `
[remote "origin"]
	url = /Users/someone/repo
	fetch = +refs/heads/*:refs/remotes/origin/*
	fetch = +refs/tags/*:refs/tags/*
	uploadpack = /evil/upload-pack
	receivepack = /evil/receive-pack
[branch "main"]
	remote = origin
	merge = refs/heads/main
	rebase = true
[user]
	name = "Quoted \"Name\" \\ Slash"
	email = a@example.com
	signingkey = ABCDEF
[filter "audit"]
	clean = /evil/clean
	smudge = /evil/smudge
	process = /evil/process
	required = true
[diff "audit"]
	textconv = /evil/textconv
	command = /evil/diff
[merge "audit"]
	driver = /evil/merge
[merge]
	conflictstyle = diff3
[alias]
	st = !sh -c 'evil'
[credential]
	helper = /evil/helper
[submodule "lib"]
	update = !/evil/update
	url = /somewhere
[gpg]
	program = /evil/gpg
[commit]
	gpgsign = true
[extensions]
	objectFormat = sha1
`
	if err := os.WriteFile(local, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	projected, err := trustedGitConfig(context.Background(), local, dir)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "projected")
	if err := os.WriteFile(out, projected, 0o600); err != nil {
		t.Fatal(err)
	}
	get := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"config", "--file", out}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		data, _ := cmd.Output()
		return strings.TrimSpace(string(data))
	}
	for key, want := range map[string]string{
		"remote.origin.url":       "/Users/someone/repo",
		"branch.main.merge":       "refs/heads/main",
		"branch.main.rebase":      "true",
		"user.name":               `Quoted "Name" \ Slash`,
		"user.email":              "a@example.com",
		"merge.conflictstyle":     "diff3",
		"core.filemode":           "true",
		"extensions.objectformat": "sha1",
	} {
		if got := get("--get", key); got != want {
			t.Errorf("%s = %q, want %q\n%s", key, got, want, projected)
		}
	}
	if got := get("--get-all", "remote.origin.fetch"); got != "+refs/heads/*:refs/remotes/origin/*\n+refs/tags/*:refs/tags/*" {
		t.Errorf("multi-valued fetch refspecs = %q\n%s", got, projected)
	}
	lower := strings.ToLower(string(projected))
	for _, dropped := range []string{"evil", "include", "hookspath", "fsmonitor", "pager", "excludesfile", "worktree", "uploadpack", "receivepack",
		"signingkey", "filter", "textconv", "diff \"", "driver", "alias", "credential", "submodule", "gpg", "gpgsign"} {
		if strings.Contains(lower, dropped) {
			t.Errorf("projected config still carries %q:\n%s", dropped, projected)
		}
	}
	if all := get("--list"); strings.Count(all, "\n")+1 != 14 {
		t.Errorf("projected config has %d entries, want exactly the 14 allowlisted ones:\n%s", strings.Count(all, "\n")+1, all)
	}

	// A missing local config projects to an empty file; reftable refs fail closed.
	if projected, err := trustedGitConfig(context.Background(), filepath.Join(dir, "absent"), dir); err != nil || len(projected) != 0 {
		t.Fatalf("absent config = %q, %v", projected, err)
	}
	if err := os.WriteFile(local, []byte("[extensions]\n\trefStorage = reftable\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := trustedGitConfig(context.Background(), local, dir); err == nil || !strings.Contains(err.Error(), "reftable") {
		t.Fatalf("reftable config error = %v; want a refusal", err)
	}
}
