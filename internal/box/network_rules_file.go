package box

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"golang.org/x/sys/unix"
)

// captureNetworkRulesFile reads one bounded regular-file descriptor. Its rules
// are the operator's grants, so it must be an owner-controlled, single-link file
// outside all supplied exposures, including aliases traversed on the way there.
// Any other file is refused by name: an agent could have written it, and no
// review exists that could approve rules for one run. The captured bytes are
// what admission uses; the path is never reopened after this read.
type networkRulesFile struct {
	rules []egress.Rule
}

func networkRulesRoots(exposed []string) []MCPSourceRoot {
	roots := make([]MCPSourceRoot, 0, len(exposed))
	for _, root := range exposed {
		roots = append(roots, MCPSourceRoot{Kind: "agent exposure", Path: root})
	}
	return roots
}

func captureNetworkRulesFile(path string, exposed []string) (*networkRulesFile, error) {
	source, err := resolveHostFileSource(path, "egress rules", networkRulesRoots(exposed))
	if err != nil {
		return nil, err
	}
	before, err := os.Lstat(source.path)
	if err != nil || !before.Mode().IsRegular() {
		return nil, fmt.Errorf("egress rules file %s must exist and be a regular file", path)
	}
	if source.overlap != nil {
		return nil, fmt.Errorf("egress rules file %s is inside %s, which the box can reach — keep the rules file outside the project", path, source.overlap.Path)
	}
	file, err := os.OpenFile(source.path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("egress rules file cannot be captured")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(before, info) || info.Size() > egress.MaxDocumentBytes {
		return nil, errors.New("egress rules file changed or exceeds its limit")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 || int(stat.Uid) != os.Getuid() || !rulesFileWritableOnlyByOwner(file, info.Mode().Perm(), stat.Gid) {
		return nil, fmt.Errorf("egress rules file %s must be yours alone: owned by you, with no other hard links, and not writable by others", path)
	}
	data, err := io.ReadAll(io.LimitReader(file, egress.MaxDocumentBytes+1))
	if err != nil || len(data) > egress.MaxDocumentBytes {
		return nil, errors.New("egress rules file cannot be captured within its limit")
	}
	rules, err := egress.DecodeRules(data)
	if err != nil {
		return nil, err
	}
	return &networkRulesFile{rules: rules}, nil
}

func (f *networkRulesFile) apply(input networkstate.Admission) (networkstate.Admission, error) {
	if f == nil {
		return input, nil
	}
	input.Operator = append(append([]egress.Input{}, input.Operator...), egress.Input{Rules: f.rules, Origin: egress.Origin{Kind: "operator", Name: "rules-file"}})
	return input, nil
}

// hostFileSource is one resolved host input: path is the canonical endpoint that
// may be reopened, and overlap names the agent-exposed root the source falls
// inside, if any. link marks a source whose last component is a symbolic link,
// which the error returned with it refuses.
type hostFileSource struct {
	path    string
	overlap *MCPSourceRoot
	link    bool
}

// An exposed source is a CLASSIFICATION, not a read failure: each caller refuses
// it in its own words. Keep malformed paths and filesystem errors distinct from
// that case.
func resolveHostFileSource(sourcePath, kind string, roots []MCPSourceRoot) (hostFileSource, error) {
	for _, component := range strings.Split(sourcePath, string(filepath.Separator)) {
		if component == ".." {
			return hostFileSource{}, fmt.Errorf("%s source %q contains a parent path component; use a canonical path without '..'", kind, sourcePath)
		}
	}
	lexicalSource, err := filepath.Abs(sourcePath)
	if err != nil {
		return hostFileSource{}, fmt.Errorf("resolve configured %s source: %w", kind, err)
	}
	lexicalSource = filepath.Clean(lexicalSource)
	source, traversed, err := resolvePathTrace(lexicalSource)
	if err != nil {
		return hostFileSource{}, fmt.Errorf("resolve configured %s source: %w", kind, err)
	}
	candidates := append([]string{lexicalSource, source}, traversed...)
	overlap, err := hostSourceOverlap(candidates, roots)
	if err != nil {
		return hostFileSource{}, err
	}
	result := hostFileSource{path: source, overlap: overlap}
	if info, err := os.Lstat(sourcePath); err == nil && info.Mode()&os.ModeSymlink != 0 {
		result.link = true
		return result, fmt.Errorf("%s source %q is a symbolic link; replace it with a private regular file and retry", kind, sourcePath)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return hostFileSource{}, fmt.Errorf("inspect configured %s source %q: %w", kind, sourcePath, err)
	}
	return result, nil
}

func hostSourceOverlap(candidates []string, roots []MCPSourceRoot) (*MCPSourceRoot, error) {
	var overlap *MCPSourceRoot
	for _, root := range roots {
		if root.Path == "" {
			continue
		}
		absoluteRoot, err := filepath.Abs(root.Path)
		if err != nil {
			return nil, fmt.Errorf("resolve %s %q: %w", root.Kind, root.Path, err)
		}
		realRoot, _, err := resolvePathTrace(absoluteRoot)
		if err != nil {
			return nil, fmt.Errorf("resolve %s %q: %w", root.Kind, root.Path, err)
		}
		for _, candidate := range candidates {
			inside, err := futurePathWithin(realRoot, candidate)
			if err != nil {
				return nil, fmt.Errorf("compare host source with %s %q: %w", root.Kind, root.Path, err)
			}
			if inside {
				overlap = &root
			}
		}
	}
	return overlap, nil
}

// rulesFileWritableOnlyByOwner allows group write only in the owner's private group. Debian and
// Ubuntu give each user a group of their own and a umask of 0002, so every file a person creates
// there is 0664; OpenSSH's Debian packaging accepts the same case. Anyone else who can write the file
// could grant a box network access, so anything these checks cannot rule out stays refused: an
// access ACL (the mode's group bits then show its mask, not the group), a group or account source
// beyond the local files, or an unreadable file. Hosts on such sources (Fedora's sss) use `chmod g-w`.
func rulesFileWritableOnlyByOwner(file *os.File, perm os.FileMode, gid uint32) bool {
	switch {
	case perm&0o002 != 0:
		return false
	case perm&0o020 == 0:
		return true
	}
	if _, err := unix.Fgetxattr(int(file.Fd()), "system.posix_acl_access", nil); !errors.Is(err, unix.ENODATA) && !errors.Is(err, unix.ENOTSUP) {
		return false
	}
	nsswitch, nsswitchErr := os.ReadFile("/etc/nsswitch.conf")
	passwd, passwdErr := os.ReadFile("/etc/passwd")
	group, groupErr := os.ReadFile("/etc/group")
	return nsswitchErr == nil && passwdErr == nil && groupErr == nil && localAccountsOnly(string(nsswitch)) &&
		privateGroup(string(passwd), string(group), os.Getuid(), gid)
}

// localAccountsOnly reports whether users and groups come only from the files privateGroup reads, so
// that a member it cannot see cannot exist. systemd's source holds only its own dynamic users.
func localAccountsOnly(nsswitch string) bool {
	seen := 0
	for line := range strings.Lines(nsswitch) {
		text, _, _ := strings.Cut(line, "#")
		database, sources, ok := strings.Cut(text, ":")
		if database = strings.TrimSpace(database); !ok || database != "passwd" && database != "group" {
			continue
		}
		seen++
		for _, source := range strings.Fields(sources) {
			if source != "files" && source != "systemd" && !strings.HasPrefix(source, "[") {
				return false
			}
		}
	}
	return seen == 2
}

// privateGroup reports whether gid is uid's primary group, named after the user, with no other
// member: nobody else lists it as their primary group and its member list names only the user. A user
// or group these files do not hold is not known to be private, and a line it cannot read could be
// anything (a NIS "+" entry, a differently written number), so any such line says no.
func privateGroup(passwd, group string, uid int, gid uint32) bool {
	name := ""
	for line := range strings.Lines(passwd) {
		fields := strings.Split(strings.TrimRight(line, "\r\n"), ":")
		if len(fields) != 7 {
			return false
		}
		lineUID, uidErr := strconv.ParseUint(fields[2], 10, 32)
		lineGID, gidErr := strconv.ParseUint(fields[3], 10, 32)
		if uidErr != nil || gidErr != nil {
			return false
		}
		if lineUID == uint64(uid) {
			if name != "" || lineGID != uint64(gid) {
				return false
			}
			name = fields[0]
		} else if lineGID == uint64(gid) {
			return false
		}
	}
	if name == "" {
		return false
	}
	found := false
	for line := range strings.Lines(group) {
		fields := strings.Split(strings.TrimRight(line, "\r\n"), ":")
		if len(fields) != 4 {
			return false
		}
		lineGID, err := strconv.ParseUint(fields[2], 10, 32)
		if err != nil {
			return false
		}
		if lineGID != uint64(gid) {
			continue
		}
		if found || fields[0] != name {
			return false
		}
		found = true
		for _, member := range strings.Split(fields[3], ",") {
			if member != "" && member != name {
				return false
			}
		}
	}
	return found
}
