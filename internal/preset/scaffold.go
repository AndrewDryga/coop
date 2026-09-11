package preset

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Scaffold publishes a validated starter preset and its prompts as one create-only
// bundle. Even an empty or incomplete existing destination belongs to the user.
func Scaffold(repo, name string) (string, error) {
	return scaffold(repo, name, writeScaffoldBundle, publishScaffoldBundle)
}

func scaffold(repo, name string, write func(*scaffoldBundle, string) error, publish func(*os.Root, *os.Root, string) error) (string, error) {
	if !ValidName(name) {
		return "", fmt.Errorf("invalid preset name %q — a preset is a folder name under %s/ (lowercase, no '/', '..', or leading '-')", name, Dir)
	}
	root, err := os.OpenRoot(repo)
	if err != nil {
		return "", err
	}
	defer root.Close()
	agent, err := openScaffoldDir(root, ".agent", 0o755)
	if err != nil {
		return "", err
	}
	defer agent.Close()
	parent, err := openScaffoldDir(agent, "presets", 0o755)
	if err != nil {
		return "", err
	}
	defer parent.Close()
	if _, err := parent.Lstat(name); err == nil {
		return "", scaffoldExists(name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	stageName := ".coop-preset-" + rand.Text()
	if err := parent.Mkdir(stageName, 0o700); err != nil {
		return "", err
	}
	stage, err := openScaffoldDir(parent, stageName, 0o700)
	if err != nil {
		return "", err
	}
	defer stage.Close()
	defer func() {
		if scaffoldDirMatches(parent, stageName, stage) {
			_ = parent.Remove(stageName)
		}
	}()
	// A fixed inner name keeps List blind to staging, even for a preset named
	// "preset.yaml". Interrupted wrappers are harmless and never block a retry.
	bundleRoot, err := openScaffoldDir(stage, "bundle", 0o755)
	if err != nil {
		return "", err
	}
	bundle := &scaffoldBundle{Root: bundleRoot}
	defer bundle.Close()
	defer func() {
		owned := scaffoldDirMatches(stage, "bundle", bundle.Root)
		bundle.cleanup(owned)
		if owned {
			_ = stage.Remove("bundle")
		}
	}()
	if err := write(bundle, name); err != nil {
		return "", err
	}
	data, err := bundle.ReadFile("preset.yaml")
	if err != nil {
		return "", err
	}
	path := filepath.Join(repo, Dir, name, "preset.yaml")
	if _, err := loadPreset(name, filepath.Dir(path), data, bundle.ReadFile); err != nil {
		return "", fmt.Errorf("validate starter preset: %w", err)
	}
	if !scaffoldDirMatches(root, ".agent", agent) || !scaffoldDirMatches(agent, "presets", parent) {
		return "", errors.New("preset parent changed during initialization — retry after the directory stops changing")
	}
	for _, dir := range []*os.Root{bundle.Root, stage, parent, agent, root} {
		if err := syncScaffoldDir(dir); err != nil {
			return "", err
		}
	}
	if !scaffoldDirMatches(parent, stageName, stage) || !scaffoldDirMatches(stage, "bundle", bundle.Root) {
		return "", errors.New("preset staging changed during initialization — existing entries were preserved; retry with a new stage")
	}
	if err := publish(stage, parent, name); err != nil {
		return "", err
	}
	if !scaffoldDirMatches(root, ".agent", agent) || !scaffoldDirMatches(agent, "presets", parent) || !scaffoldDirMatches(parent, name, bundle.Root) {
		return "", fmt.Errorf("preset %q was published but its path changed — inspect the moved directory before retrying", name)
	}
	return path, nil
}

func scaffoldExists(name string) error {
	return fmt.Errorf("preset %q already exists (%s) — existing files were preserved; edit it or choose another name", name, filepath.Join(Dir, name))
}

// Opening one component at a time rejects links and pins the directory identity;
// later operations never traverse a mutable parent path again.
func openScaffoldDir(parent *os.Root, name string, mode os.FileMode) (*os.Root, error) {
	if err := parent.Mkdir(name, mode); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	info, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("preset directory %s is not a real directory — symlinks are not supported during initialization", name)
	}
	dir, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err := dir.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		_ = dir.Close()
		return nil, fmt.Errorf("preset directory %s changed while opening", name)
	}
	return dir, nil
}

func scaffoldDirMatches(parent *os.Root, name string, dir *os.Root) bool {
	current, err := parent.Lstat(name)
	if err != nil || !current.IsDir() {
		return false
	}
	opened, err := dir.Stat(".")
	return err == nil && os.SameFile(current, opened)
}

type scaffoldEntry struct {
	parent *os.Root
	name   string
	info   os.FileInfo
}

type scaffoldBundle struct {
	*os.Root
	roles   *os.Root
	created []scaffoldEntry
}

// Cleanup never follows the lexical roles path and never removes a substituted
// inode. Unknown entries leave invisible residue instead of widening authority.
func (b *scaffoldBundle) cleanup(owned bool) {
	if b.roles != nil {
		defer b.roles.Close()
	}
	if !owned {
		return
	}
	for i := len(b.created) - 1; i >= 0; i-- {
		entry := b.created[i]
		if current, err := entry.parent.Lstat(entry.name); err == nil && os.SameFile(current, entry.info) {
			_ = entry.parent.Remove(entry.name)
		}
	}
}

func writeScaffoldBundle(bundle *scaffoldBundle, name string) error {
	if err := bundle.Mkdir("roles", 0o755); err != nil {
		return err
	}
	roles, err := bundle.OpenRoot("roles")
	if err != nil {
		return err
	}
	bundle.roles = roles
	info, err := roles.Stat(".")
	if err != nil {
		return err
	}
	bundle.created = append(bundle.created, scaffoldEntry{bundle.Root, "roles", info})
	files := append([]templateFile{{"preset.yaml", fmt.Sprintf(Template, name)}}, templateFiles...)
	for _, item := range files {
		parent, rel := bundle.Root, item.rel
		if strings.HasPrefix(rel, "roles/") {
			parent, rel = roles, strings.TrimPrefix(rel, "roles/")
		}
		file, err := parent.OpenFile(filepath.FromSlash(rel), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		info, err := file.Stat()
		if err != nil {
			_ = file.Close()
			return err
		}
		bundle.created = append(bundle.created, scaffoldEntry{parent, rel, info})
		_, writeErr := file.WriteString(item.content)
		err = errors.Join(writeErr, file.Sync(), file.Close())
		if err != nil {
			return err
		}
	}
	return syncScaffoldDir(roles)
}

func syncScaffoldDir(root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func publishScaffoldBundle(stage, parent *os.Root, name string) error {
	return publishScaffoldBundleSync(stage, parent, name, (*os.File).Sync)
}

func publishScaffoldBundleSync(stage, parent *os.Root, name string, sync func(*os.File) error) error {
	from, err := stage.Open(".")
	if err != nil {
		return err
	}
	defer from.Close()
	to, err := parent.Open(".")
	if err != nil {
		return err
	}
	defer to.Close()
	if err := renameScaffoldBundle(int(from.Fd()), int(to.Fd()), name); err != nil {
		if errors.Is(err, os.ErrExist) {
			return scaffoldExists(name)
		}
		return fmt.Errorf("publish preset %q: %w", name, err)
	}
	if err := errors.Join(sync(to), sync(from)); err != nil {
		return fmt.Errorf("preset %q was published but directory sync failed — inspect it before retrying: %w", name, err)
	}
	return nil
}
