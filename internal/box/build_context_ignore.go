package box

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/AndrewDryga/coop/internal/project"
	"github.com/moby/patternmatcher"
	"github.com/moby/patternmatcher/ignorefile"
)

// Filter the staged bytes as well as their digest. Ignored source is mounted at runtime; it
// cannot affect an explicit image build and must not revoke that image when an editor saves it.
func filterDockerBuildContext(ctx context.Context, repo string, entries []contextEntry) ([]contextEntry, error) {
	dockerfile := filepath.FromSlash(project.DockerfilePath(repo))
	available := make(map[string]bool, len(entries))
	for _, entry := range entries {
		available[entry.rel] = true
	}
	var active string
	var patterns []string
	for _, rel := range []string{dockerfile + ".dockerignore", ".dockerignore"} {
		data, err := readRepositoryRegularFileNoFollow(repo, rel, imageInputFileLimit)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read build exclusions %s: %w", rel, err)
		}
		if !available[rel] {
			return nil, fmt.Errorf("build exclusions %s must not be hidden or ignored by Git", rel)
		}
		patterns, err = ignorefile.ReadAll(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("read build exclusions %s: %w", rel, err)
		}
		active = rel
		break // An empty Dockerfile-specific file still overrides the root rules.
	}
	if active == "" {
		return entries, nil
	}
	matcher, err := patternmatcher.New(patterns)
	if err != nil {
		return nil, fmt.Errorf("invalid build exclusions %s: %w", active, err)
	}
	keep := make(map[string]bool)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		excluded, err := matcher.MatchesOrParentMatches(filepath.ToSlash(entry.rel))
		if err != nil {
			return nil, fmt.Errorf("match build exclusions %s: %w", active, err)
		}
		// Docker sends its control files even when their own patterns exclude them. This only
		// retains already sanitized entries: exceptions cannot restore secrets or Git output.
		if excluded && entry.rel != dockerfile && entry.rel != active {
			continue
		}
		for rel := entry.rel; rel != "."; rel = filepath.Dir(rel) {
			keep[rel] = true
		}
	}
	selected := entries[:0]
	for _, entry := range entries {
		if keep[entry.rel] {
			selected = append(selected, entry)
		}
	}
	return selected, nil
}
