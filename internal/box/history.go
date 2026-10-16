package box

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/safefile"
)

var unsafeKeyChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// NativeHomePath selects a complete home without creating it or importing data.
// ACP conversations keep the same path across account changes.
func NativeHomePath(cfg *config.Config, provider, account, repo string, acp bool) (string, error) {
	if cfg == nil || !accountNameValid(provider) {
		return "", fmt.Errorf("invalid native home selection")
	}
	key, _, err := HistoryKey(repo)
	if err != nil {
		return "", err
	}
	if acp {
		return filepath.Join(cfg.ConfigDir, provider, "acp-homes", key, "home"), nil
	}
	if account == "" {
		account = config.DefaultProfile
	}
	if !accountNameValid(account) {
		return "", fmt.Errorf("invalid native account selection")
	}
	return filepath.Join(cfg.ConfigDir, provider, "native-homes", account, key, "home"), nil
}

// HistoryKey names a repository's complete native state domain. A validated
// fork binding joins its project; path spelling alone never establishes it.
func HistoryKey(repo string) (key, project string, err error) {
	if repo == "" || !filepath.IsAbs(repo) {
		return "", "", fmt.Errorf("session history needs an absolute repository path, got %q", repo)
	}
	project, _, err = forkspace.ResolveProjectBinding(repo)
	if err != nil {
		return "", "", err
	}
	canonical, err := filepath.EvalSymlinks(project)
	if err != nil {
		return "", "", fmt.Errorf("resolve the repository for its session history: %w", err)
	}
	sum := sha256.Sum256([]byte(canonical))
	name := strings.Trim(unsafeKeyChars.ReplaceAllString(filepath.Base(canonical), "-"), "-.")
	if name == "" {
		name = "repo"
	}
	return name + "-" + hex.EncodeToString(sum[:])[:16], canonical, nil
}

// HistoryAccountRoot excludes credential-independent ACP homes. Removing an
// account must not erase editor conversations that survived account switches.
func HistoryAccountRoot(cfg *config.Config, provider, account string) string {
	if account == "" {
		account = config.DefaultProfile
	}
	return filepath.Join(cfg.ConfigDir, provider, "native-homes", account)
}

// HistoryStores enumerates complete repository homes for one account's usage.
func HistoryStores(cfg *config.Config, provider, account string) ([]string, error) {
	return nativeHistoryHomes(HistoryAccountRoot(cfg, provider, account))
}

// ACPHistoryStores has no account attribution: credentials may change while
// one editor conversation and its native history remain in the same home.
func ACPHistoryStores(cfg *config.Config, provider string) ([]string, error) {
	return nativeHistoryHomes(filepath.Join(cfg.ConfigDir, provider, "acp-homes"))
}

// ACPRepositorySessions inventories both retained and already-scoped transcripts
// without seeding dormant providers. Only recorded repository ownership counts.
func ACPRepositorySessions(cfg *config.Config, provider, repo string) (map[string]bool, error) {
	ag, ok := agents.Get(provider)
	if !ok {
		return nil, fmt.Errorf("unknown history provider")
	}
	home, err := NativeHomePath(cfg, provider, "", repo, true)
	if err != nil {
		return nil, err
	}
	_, project, err := HistoryKey(repo)
	if err != nil {
		return nil, err
	}
	ids := map[string]bool{}
	for _, source := range []string{acpSharedDir(cfg, provider), home} {
		plan, err := ag.NativeHistory(source, func(cwd string) bool {
			_, candidate, err := HistoryKey(cwd)
			return err == nil && candidate == project
		})
		if err != nil {
			return nil, err
		}
		for _, file := range plan.Files {
			if file.SessionID != "" {
				ids[file.SessionID] = true
			}
		}
	}
	return ids, nil
}

// nativeHistoryHomes lists the complete homes under path. A missing path or home is no home, and
// neither is a link or a file at <key>/home, like a link at <key>: Coop only ever makes a real
// folder there and reads no history through anything else, so reporting one would only keep usage
// partial and its cache unpruned. Any other error comes back with the homes that could be listed:
// a home it hides still holds history.
func nativeHistoryHomes(path string) ([]string, error) {
	root, err := safefile.OpenRoot(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, withPath(path, err)
	}
	defer root.Close()
	entries, err := root.ReadDir(-1)
	var homes []string
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		rel := filepath.Join(entry.Name(), "home")
		home, openErr := safefile.OpenDir(root, rel)
		if openErr != nil {
			// A file fails as ENOTDIR; POSIX lets a link fail as ENOTDIR or ELOOP.
			if !errors.Is(openErr, os.ErrNotExist) && !errors.Is(openErr, syscall.ENOTDIR) && !errors.Is(openErr, syscall.ELOOP) {
				err = errors.Join(err, withPath(filepath.Join(path, rel), openErr))
			}
			continue
		}
		_ = home.Close()
		homes = append(homes, filepath.Join(path, rel))
	}
	return homes, err
}

// withPath names the folder a bare system error is about, so a report can say what to fix.
func withPath(path string, err error) error {
	if errno, ok := err.(syscall.Errno); ok {
		return &os.PathError{Op: "open", Path: path, Err: errno}
	}
	return err
}
