package forkspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// trustedGitConfigKeys is the allowlist of repository-local configuration a host git command
// may see: data git needs to operate on the repository, never anything it would execute or read a
// host file from. Everything else in the agent-writable .git/config — every filter/diff/merge
// driver, include, alias, credential helper, submodule setting, hook path, pager, editor — is
// dropped, so the repository cannot name code for the host to run. Your global and system config
// are read as before; that is where filter.lfs and your signing setup live.
var trustedGitConfigKeys = []*regexp.Regexp{
	regexp.MustCompile(`^core\.(repositoryformatversion|filemode|bare|logallrefupdates|ignorecase|precomposeunicode|symlinks|autocrlf|eol|safecrlf)$`),
	regexp.MustCompile(`^extensions\.[a-z0-9]+$`),
	regexp.MustCompile(`^remote\..+\.(url|pushurl|fetch|push|mirror|prune|tagopt)$`),
	regexp.MustCompile(`^branch\..+\.(remote|pushremote|merge|rebase)$`),
	regexp.MustCompile(`^user\.(name|email)$`),
	regexp.MustCompile(`^(merge\.conflictstyle|pull\.rebase|push\.default)$`),
}

// errRefStorageUnsupported names the one extension the view cannot host: reftable keeps refs in
// a table the view's symlinked refs/ and packed-refs cannot stand in for.
var errRefStorageUnsupported = errors.New("repository uses reftable ref storage, which coop's trusted git view does not support")

// trustedGitConfig projects the repository's local config onto the allowlist. It reads a private
// COPY of the file (git config --file, which follows no include), so the result is a snapshot: a
// value the agent writes after the copy is never seen by anything that executes. The returned
// bytes are a complete config file git reads verbatim as $GIT_DIR/config.
func trustedGitConfig(ctx context.Context, localConfig, scratchDir string) ([]byte, error) {
	data, err := os.ReadFile(localConfig)
	if errors.Is(err, os.ErrNotExist) {
		data = nil
	} else if err != nil {
		return nil, err
	}
	entries, err := listGitConfig(ctx, data, scratchDir)
	if err != nil {
		return nil, err
	}
	sections := map[string][]string{}
	var order []string
	for _, entry := range entries {
		key, value := entry[0], entry[1]
		if !trustedGitConfigKey(key) {
			continue
		}
		if key == "extensions.refstorage" && !strings.EqualFold(value, "files") {
			return nil, errRefStorageUnsupported
		}
		section, name := splitGitConfigKey(key)
		if section == "" || name == "" || !safeGitConfigValue(value) {
			continue
		}
		if _, seen := sections[section]; !seen {
			order = append(order, section)
		}
		sections[section] = append(sections[section], fmt.Sprintf("\t%s = %s\n", name, quoteGitConfigValue(value)))
	}
	sort.Strings(order)
	var out bytes.Buffer
	for _, section := range order {
		out.WriteString(section + "\n")
		for _, line := range sections[section] {
			out.WriteString(line)
		}
	}
	return out.Bytes(), nil
}

func trustedGitConfigKey(key string) bool {
	lower := strings.ToLower(key)
	for _, pattern := range trustedGitConfigKeys {
		if pattern.MatchString(lower) {
			return true
		}
	}
	return false
}

// listGitConfig parses config bytes with git itself (git config --file … -z --list), so quoting,
// continuations, and case folding follow git's rules, not a reimplementation. Includes are not
// followed: --file reads exactly one file.
func listGitConfig(ctx context.Context, data []byte, scratchDir string) ([][2]string, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	path := filepath.Join(scratchDir, "config.copy")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return nil, err
	}
	defer os.Remove(path)
	cmd := exec.CommandContext(ctx, "git", "config", "--file", path, "-z", "--list")
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.Output()
	if err != nil && ctx.Err() != nil {
		return nil, errors.Join(ctx.Err(), err) // a cancelled projection is a cancellation, not a verdict on the config
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(bytes.TrimSpace(exitErr.Stderr)) > 0 {
			return nil, fmt.Errorf("read repository config: %w: %s", err, strings.TrimSpace(strings.ReplaceAll(string(exitErr.Stderr), path, "config")))
		}
		return nil, fmt.Errorf("read repository config: %w", err)
	}
	var entries [][2]string
	for _, record := range bytes.Split(out, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		key, value, _ := bytes.Cut(record, []byte{'\n'})
		entries = append(entries, [2]string{string(key), string(value)})
	}
	return entries, nil
}

// Checkout views need the host's effective built-in conversion settings, but
// must not acquire its executable drivers or external attribute files. Project
// only these data values, in system/global/local precedence, into the private view.
func includeTrustedCheckoutConfig(ctx context.Context, view *gitView) error {
	var projected bytes.Buffer
	for _, scope := range []string{"system", "global"} {
		if scope == "system" {
			switch strings.ToLower(os.Getenv("GIT_CONFIG_NOSYSTEM")) {
			case "", "0", "false", "no", "off":
			case "1", "true", "yes", "on":
				continue
			default:
				return errors.New("invalid GIT_CONFIG_NOSYSTEM for native checkout conversion")
			}
		}
		command := exec.CommandContext(ctx, "git", "-C", view.workTree, "config", "--"+scope,
			"--includes", "--null", "--type=bool-or-str", "--get-regexp", `^core\.(autocrlf|eol|safecrlf)$`)
		// Retain only the trusted host config locations, never repository locators
		// or command-line config injected through ambient GIT_CONFIG_* variables.
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, "GIT_") || strings.HasPrefix(entry, "GIT_CONFIG_GLOBAL=") || strings.HasPrefix(entry, "GIT_CONFIG_SYSTEM=") {
				command.Env = append(command.Env, entry)
			}
		}
		output, err := command.Output()
		if ctx.Err() != nil {
			return errors.Join(ctx.Err(), err)
		}
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
				continue // no matching settings, including an absent default file
			}
			return fmt.Errorf("read trusted %s checkout settings: %w", scope, err)
		}
		for _, record := range bytes.Split(output, []byte{0}) {
			if len(record) == 0 {
				continue
			}
			key, value, ok := bytes.Cut(record, []byte{'\n'})
			name := strings.TrimPrefix(strings.ToLower(string(key)), "core.")
			setting := strings.ToLower(string(value))
			valid := name == "autocrlf" && (setting == "true" || setting == "false" || setting == "input") ||
				name == "eol" && (setting == "lf" || setting == "crlf" || setting == "native") ||
				name == "safecrlf" && (setting == "true" || setting == "false" || setting == "warn")
			if !ok || !valid {
				return fmt.Errorf("invalid trusted %s native checkout setting", scope)
			}
			fmt.Fprintf(&projected, "[core]\n\t%s = %s\n", name, quoteGitConfigValue(setting))
		}
	}
	path := filepath.Join(view.dir, "config")
	local, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(projected.Bytes(), local...), 0o600)
}

// splitGitConfigKey renders "section.subsection.name" as the INI header git expects
// (`[section "subsection"]`) plus the variable name; the subsection keeps its case.
func splitGitConfigKey(key string) (header, name string) {
	dot := strings.LastIndexByte(key, '.')
	if dot <= 0 || dot == len(key)-1 {
		return "", ""
	}
	name = key[dot+1:]
	head := key[:dot]
	if i := strings.IndexByte(head, '.'); i >= 0 {
		sub := head[i+1:]
		if strings.ContainsAny(sub, "\n\x00") {
			return "", ""
		}
		return fmt.Sprintf("[%s %q]", strings.ToLower(head[:i]), sub), name
	}
	return "[" + strings.ToLower(head) + "]", name
}

func safeGitConfigValue(value string) bool {
	for _, r := range value {
		if r < 0x20 && r != '\t' {
			return false
		}
	}
	return true
}

func quoteGitConfigValue(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	value = strings.ReplaceAll(value, "\t", `\t`)
	return `"` + value + `"`
}
