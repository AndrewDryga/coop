package eval

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type suitePart struct {
	caseIndex int
	name      string
	source    *openedSuiteTree // nil only for an agent case with no files
}

// StageSuite freezes only the paths a suite names, before any provider work. The returned suite
// points into an owner-private directory; its candidate inputs and hidden verifiers occupy separate
// case directories, and the caller owns removing or retaining the staged directory. A source that
// changes while it is copied is refused instead of mixing two versions into one run.
func StageSuite(ctx context.Context, root string, source *Suite) (_ *Suite, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if source == nil {
		return nil, fmt.Errorf("freeze suite: no suite")
	}
	if err := source.validate(); err != nil {
		return nil, err
	}
	if err := ensurePrivateRoot(root); err != nil {
		return nil, err
	}
	sourceDir, err := filepath.EvalSymlinks(source.Dir)
	if err != nil {
		return nil, fmt.Errorf("resolve suite directory: %w", err)
	}
	base, err := os.OpenRoot(sourceDir)
	if err != nil {
		return nil, fmt.Errorf("open suite directory: %w", err)
	}
	defer base.Close()
	rootInfo, err := base.Stat(".")
	if err != nil {
		return nil, err
	}
	if source.dirIdentity == nil || !os.SameFile(source.dirIdentity, rootInfo) {
		return nil, fmt.Errorf("suite directory changed since load; retry with a stable suite")
	}
	manifest, manifestInfo, err := readManifestFromRoot(base, filepath.Base(source.Path))
	if err != nil {
		return nil, err
	}
	if source.manifestIdentity == nil || !os.SameFile(source.manifestIdentity, manifestInfo) ||
		source.manifestHash != newHasher().bytes("manifest", manifest).sum() {
		return nil, fmt.Errorf("suite manifest changed since load; retry with a stable suite")
	}
	var parts []suitePart
	defer func() {
		for _, part := range parts {
			if part.source != nil {
				_ = part.source.root.Close()
			}
		}
	}()
	for i, c := range source.Cases {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, part := range []struct{ name, rel string }{
			{"verifier", c.Verifier}, {"files", c.Files}, {"fixture", c.Fixture}, {"tasks", c.Tasks},
		} {
			if part.rel == "" && (part.name != "files" || source.IsLoop()) {
				continue
			}
			item := suitePart{caseIndex: i, name: part.name}
			if part.rel != "" {
				item.source, err = openSuiteTree(base, part.rel)
				if err != nil {
					return nil, fmt.Errorf("case %q %s: %w", c.ID, part.name, err)
				}
			}
			parts = append(parts, item)
		}
	}
	// Lexical overlap at Load catches obvious authoring mistakes. Opened inode chains also catch
	// aliases on case-insensitive filesystems before any verifier can enter a candidate input.
	for _, input := range parts {
		if input.name == "verifier" || input.source == nil {
			continue
		}
		for _, hidden := range parts {
			if hidden.name == "verifier" && suiteTreesOverlap(input.source, hidden.source) {
				return nil, fmt.Errorf("case %q %s physically overlaps case %q verifier; every grader must stay outside every candidate input", source.Cases[input.caseIndex].ID, input.name, source.Cases[hidden.caseIndex].ID)
			}
		}
	}
	dir, err := os.MkdirTemp(root, "suite-")
	if err != nil {
		return nil, fmt.Errorf("create frozen suite: %w", err)
	}
	defer func() {
		if err != nil {
			_ = RemoveStagedSuite(dir)
		}
	}()

	staged := *source
	staged.Dir = dir
	staged.Path = filepath.Join(dir, "suite.yaml")
	staged.Cases = append([]Case(nil), source.Cases...)
	if staged.IsLoop() {
		// The effective loop recipe is a configuration, not a workload. The CLI writes the
		// already-frozen bytes here and uses this path for every loop trial.
		staged.LoopConfig = "./loop.yaml"
	}
	content := newHasher()
	content.text("schema", "eval.suite-content.v1")
	remaining := int64(SnapshotLimit)
	remainingEntries := stageMaxEntries
	lastCase := -1
	for _, part := range parts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c := &staged.Cases[part.caseIndex]
		if part.caseIndex != lastCase {
			content.text("case", c.ID)
			lastCase = part.caseIndex
		}
		destRel := filepath.Join("cases", c.ID, part.name)
		dest := filepath.Join(dir, destRel)
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			return nil, err
		}
		var digest Fingerprint
		if part.source == nil {
			// A no-files agent case starts empty, never from the suite directory.
			if err := os.Mkdir(dest, 0o700); err != nil {
				return nil, err
			}
			opened, openErr := os.OpenRoot(dest)
			if openErr != nil {
				return nil, openErr
			}
			digest, err = openedTreeDigest(ctx, opened)
			_ = opened.Close()
		} else {
			digest, err = stageOpenedTree(ctx, part.source.root, dest, &remaining, &remainingEntries)
		}
		if err != nil {
			return nil, fmt.Errorf("case %q %s: %w", c.ID, part.name, err)
		}
		switch part.name {
		case "verifier":
			c.Verifier = "./" + filepath.ToSlash(destRel)
		case "files":
			c.Files = "./" + filepath.ToSlash(destRel)
		case "fixture":
			c.Fixture = "./" + filepath.ToSlash(destRel)
		case "tasks":
			c.Tasks = "./" + filepath.ToSlash(destRel)
		}
		content.text(part.name, string(digest))
	}
	for _, part := range parts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if part.source != nil {
			if err := part.source.stillNamed(base); err != nil {
				return nil, err
			}
		}
	}
	if err := stageRuntimeProfiles(ctx, root, source, &staged, &remaining, &remainingEntries); err != nil {
		return nil, err
	}
	staged.ContentDigest = content.sum()
	if err := staged.validate(); err != nil {
		return nil, fmt.Errorf("frozen suite: %w", err)
	}
	manifest, err = yaml.Marshal(&staged)
	if err != nil {
		return nil, fmt.Errorf("encode frozen suite: %w", err)
	}
	if err := os.WriteFile(staged.Path, manifest, 0o600); err != nil {
		return nil, fmt.Errorf("write frozen suite: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &staged, nil
}

// RemoveStagedSuite removes only a temporary suite-* tree. It restores owner access to copied
// read-only directories first; otherwise a dry-run or failed freeze could leave private inputs
// behind on filesystems that require directory write permission for removal.
func RemoveStagedSuite(dir string) error {
	if !strings.HasPrefix(filepath.Base(dir), "suite-") {
		return fmt.Errorf("refusing to remove non-staging directory %q", dir)
	}
	info, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		return nil // a real run moved the tree into its record
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to remove non-directory staging path %q", dir)
	}
	if err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return os.Chmod(path, 0o700)
		}
		return nil
	}); err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

func stageOpenedTree(ctx context.Context, source *os.Root, dest string, remainingBytes *int64, remainingEntries *int) (Fingerprint, error) {
	return stageOpenedTreeWithPolicy(ctx, source, dest, remainingBytes, remainingEntries, false)
}

func stageOpenedTreeWithPolicy(ctx context.Context, source *os.Root, dest string, remainingBytes *int64, remainingEntries *int, profile bool) (Fingerprint, error) {
	before, err := openedTreeDigestWithPolicy(ctx, source, profile)
	if err != nil {
		return "", err
	}
	if err := copyOpenedSuiteTree(ctx, source, dest, remainingBytes, remainingEntries, profile); err != nil {
		return "", err
	}
	after, err := openedTreeDigestWithPolicy(ctx, source, profile)
	if err != nil {
		return "", err
	}
	staged, err := os.OpenRoot(dest)
	if err != nil {
		return "", err
	}
	copied, err := openedTreeDigestWithPolicy(ctx, staged, profile)
	_ = staged.Close()
	if err != nil {
		return "", err
	}
	if before != after || before != copied {
		return "", fmt.Errorf("source changed while freezing; retry with a stable suite")
	}
	return copied, nil
}
