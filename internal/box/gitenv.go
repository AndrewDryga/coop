package box

import (
	"os"
	"os/exec"
	"strings"
)

// The box has no ambient ~/.gitconfig of its own, so without this an agent would
// commit with no author and ignore none of the user's global gitignore patterns. We
// mount a curated global config + the user's global gitignore into every Homes run.

// hostGitGlobal reads a value from the host's GLOBAL git config (~/.gitconfig), or
// "" if unset or git is unavailable.
func hostGitGlobal(args ...string) string {
	out, err := exec.Command("git", append([]string{"config", "--global"}, args...)...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// buildGitConfig renders a curated ~/.gitconfig for the box: the given identity
// (omitted when empty) plus signing turned off — the box holds no GPG/SSH key, so a
// global commit.gpgsign=true would make every agent commit fail.
func buildGitConfig(name, email string) string {
	var b strings.Builder
	if name != "" || email != "" {
		b.WriteString("[user]\n")
		if name != "" {
			b.WriteString("\tname = " + name + "\n")
		}
		if email != "" {
			b.WriteString("\temail = " + email + "\n")
		}
	}
	b.WriteString("[commit]\n\tgpgsign = false\n[tag]\n\tgpgsign = false\n")
	return b.String()
}

// gitConfigForBox is buildGitConfig with the host user's global identity.
func gitConfigForBox() string {
	return buildGitConfig(hostGitGlobal("user.name"), hostGitGlobal("user.email"))
}

// hostGlobalGitignore returns the content of the user's global gitignore
// (core.excludesfile, with ~ expanded), or "" if there is none.
func hostGlobalGitignore() string {
	path := hostGitGlobal("--path", "core.excludesfile")
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}
