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

// captureNetworkRulesFile reads one bounded regular-file descriptor. Operator
// authority requires an owner-controlled, single-link file outside all supplied
// exposures, including aliases traversed on the way there. Otherwise the exact
// rules are requests, never grants. The captured bytes are what admission uses;
// the path is never reopened after this read.
type networkRulesFile struct {
	rules    []egress.Rule
	operator bool
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
		return nil, errors.New("egress rules file must exist and be a regular file")
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
	data, err := io.ReadAll(io.LimitReader(file, egress.MaxDocumentBytes+1))
	if err != nil || len(data) > egress.MaxDocumentBytes {
		return nil, errors.New("egress rules file cannot be captured within its limit")
	}
	rules, err := egress.DecodeRules(data)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	operator := source.overlap == nil && ok && stat.Nlink == 1 && int(stat.Uid) == os.Getuid() && info.Mode().Perm()&0o022 == 0
	return &networkRulesFile{rules: rules, operator: operator}, nil
}

func (f *networkRulesFile) apply(input networkstate.Admission) (networkstate.Admission, error) {
	if f == nil {
		return input, nil
	}
	if f.operator {
		input.Operator = append(append([]egress.Input{}, input.Operator...), egress.Input{Rules: f.rules, Origin: egress.Origin{Kind: "operator", Name: "rules-file"}})
	} else {
		var err error
		input.Requests, err = egress.NormalizeRules(append(append([]egress.Rule{}, input.Requests...), f.rules...))
		if err != nil {
			return networkstate.Admission{}, err
		}
	}
	return input, nil
}

// hostFileSource is one resolved host input plus the proof trail the caller
// needs: path is the canonical endpoint that may be reopened, trace is every
// entry traversed on the way there, and overlap names the agent-exposed root
// the source falls inside, if any.
type hostFileSource struct {
	path    string
	trace   []string
	overlap *MCPSourceRoot
}

// An exposed source is a CLASSIFICATION, not a read failure. Rules files may
// request reviewed permissions from that lane; MCP and environment files may
// not. Keep malformed paths and filesystem errors distinct from either case.
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
	result := hostFileSource{path: source, trace: candidates, overlap: overlap}
	if info, err := os.Lstat(sourcePath); err == nil && info.Mode()&os.ModeSymlink != 0 {
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
