package box

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkstate"
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
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Nlink != 1 || int(stat.Uid) != os.Getuid() || info.Mode().Perm()&0o022 != 0 {
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
