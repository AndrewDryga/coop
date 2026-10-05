package box

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
)

// Session history is kept per repository. Every box mounts the account's whole provider home, and
// that home held the history of every project the account ran in, so an agent working in one repo
// could read the others'. A provider that declares a HistoryLayout gets, for each repository, a
// store outside its profile, <ConfigDir>/<agent>/history/<account>/<key>/, mounted over each
// declared path inside the home. The credential and settings stay shared.

// historyLayout is the provider's declared layout; a variable so tests can give one to a provider.
var historyLayout = func(agent string) agents.HistoryLayout {
	if ag, ok := agents.Get(agent); ok {
		if keeper, ok := ag.(agents.HistoryKeeper); ok {
			return keeper.HistoryLayout()
		}
	}
	return agents.HistoryLayout{}
}

func layoutEmpty(layout agents.HistoryLayout) bool {
	return len(layout.Dirs) == 0 && len(layout.Appends) == 0
}

// historyAccountRoot holds one account's per-repository stores.
func historyAccountRoot(cfg *config.Config, agent, account string) string {
	if account == "" {
		account = config.DefaultProfile
	}
	return filepath.Join(cfg.ConfigDir, agent, "history", account)
}

var unsafeKeyChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// HistoryKey names a repository's store: its folder name and a hash of its canonical path. A fork
// maps to its project through the validated fork binding, so the two share one history.
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

// storeRecord is written into every store, naming the project it belongs to, so a key collision or
// a moved store is refused instead of mixing two repositories' history.
type storeRecord struct {
	Project string `json:"project"`
}

const storeRecordName = ".coop-store.json"

// PrepareHistory returns the store for (agent, account, repo), creating it as the user if needed,
// and makes sure every overlay target exists in the profile so a runtime never creates one as
// root. It returns "" when the provider declares no layout. It runs before any host lookup and
// before the mount, from both places.
func PrepareHistory(cfg *config.Config, agent, account, repo string) (string, error) {
	layout := historyLayout(agent)
	if layoutEmpty(layout) {
		return "", nil
	}
	key, project, err := HistoryKey(repo)
	if err != nil {
		return "", err
	}
	root := historyAccountRoot(cfg, agent, account)
	store := filepath.Join(root, key)
	for _, dir := range []string{filepath.Join(cfg.ConfigDir, agent, "history"), root, store} {
		if err := ensurePrivateDir(dir); err != nil {
			return "", fmt.Errorf("prepare %s session history: %w", agent, err)
		}
	}
	if err := claimStore(store, project); err != nil {
		return "", err
	}
	profile := cfg.AgentProfileDir(agent, account)
	for _, dir := range layout.Dirs {
		for _, base := range []string{store, profile} {
			if err := ensurePrivateDir(filepath.Join(base, filepath.FromSlash(dir))); err != nil {
				return "", fmt.Errorf("prepare %s session history: %w", agent, err)
			}
		}
	}
	for _, file := range layout.Appends {
		for _, base := range []string{store, profile} {
			if err := ensurePrivateFile(filepath.Join(base, filepath.FromSlash(file))); err != nil {
				return "", fmt.Errorf("prepare %s session history: %w", agent, err)
			}
		}
	}
	return store, nil
}

// claimStore records the project in a new store and refuses a store that names another one.
func claimStore(store, project string) error {
	path := filepath.Join(store, storeRecordName)
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		record, _ := json.Marshal(storeRecord{Project: project})
		return os.WriteFile(path, append(record, '\n'), 0o600)
	case err != nil:
		return err
	}
	var record storeRecord
	if err := json.Unmarshal(data, &record); err != nil || record.Project != project {
		return fmt.Errorf("session history store %s belongs to another repository (%q, not %q) — move it aside to continue", store, record.Project, project)
	}
	return nil
}

// ensurePrivateDir creates a 0700 directory, refusing a symlink where it should be: a box can
// write inside a store, and a link it planted must never redirect what the host creates.
func ensurePrivateDir(path string) error {
	if info, err := os.Lstat(path); err == nil {
		if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is not a directory", path)
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return nil
}

// ensurePrivateFile creates an empty 0600 file if none is there, refusing anything but a regular
// file: a file overlay needs a real file on both sides of the mount.
func ensurePrivateFile(path string) error {
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", path)
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return f.Close()
}

// historyOverlays is what a store adds to the box: each declared path of the provider's home, from
// the store. In an ACP box the lead's ACP directories come from the ACP store instead, since one
// mount point can't have two sources.
func historyOverlays(cfg *config.Config, spec RunSpec, agent, store string) []mountedWritable {
	layout := historyLayout(agent)
	var acpDirs []string
	if spec.ShareACPSessions && agent == runPrimary(spec) {
		if ag, ok := agents.Get(agent); ok {
			acpDirs = ag.ACPSessionDirs()
		}
	}
	var out []mountedWritable
	for _, dir := range layout.Dirs {
		if !containsString(acpDirs, dir) {
			out = append(out, mountedWritable{Host: filepath.Join(store, filepath.FromSlash(dir)), Box: cfg.HomeInBox + "/." + agent + "/" + dir,
				Kind: agent + " session history", history: true})
		}
	}
	for _, file := range layout.Appends {
		out = append(out, mountedWritable{Host: filepath.Join(store, filepath.FromSlash(file)), Box: cfg.HomeInBox + "/." + agent + "/" + file,
			Kind: agent + " session history", history: true, file: true})
	}
	return out
}

func containsString(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

// prepareHistoryStores gives each scoped agent its store for this run. A login box gets empty
// stores of its own under the run's artifacts (passed only for a login), removed with them: it has
// no repository, and the sign-in CLI must not read anyone's history. A remote session's private
// profile already holds only that session's history, keyed by /workspace, so it keeps it as is.
func prepareHistoryStores(cfg *config.Config, spec *RunSpec, artifacts string) error {
	if remoteSessionStateRoot(cfg, *spec) != "" {
		return nil
	}
	for _, agent := range credentialScope(cfg, *spec) {
		layout := historyLayout(agent)
		if layoutEmpty(layout) {
			continue
		}
		var store string
		switch {
		case spec.Login && artifacts == "":
			continue
		case spec.Login:
			dir, err := os.MkdirTemp(artifacts, "coop-history-"+agent+"-")
			if err != nil {
				return err
			}
			for _, d := range layout.Dirs {
				if err := ensurePrivateDir(filepath.Join(dir, filepath.FromSlash(d))); err != nil {
					return err
				}
			}
			for _, f := range layout.Appends {
				if err := ensurePrivateFile(filepath.Join(dir, filepath.FromSlash(f))); err != nil {
					return err
				}
			}
			store = dir
		case spec.Repo == "":
			continue
		default:
			prepared, err := PrepareHistory(cfg, agent, cfg.ActiveProfile(agent), spec.Repo)
			if err != nil {
				return err
			}
			store = prepared
		}
		if spec.historyStores == nil {
			spec.historyStores = map[string]string{}
		}
		spec.historyStores[agent] = store
	}
	return nil
}

// HistoryStores lists an account's per-repository stores, for readers that total history (coop
// usage). Links are skipped: a store is a directory Coop created.
func HistoryStores(cfg *config.Config, agent, account string) []string {
	root := historyAccountRoot(cfg, agent, account)
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var stores []string
	for _, entry := range entries {
		if entry.IsDir() && entry.Type()&fs.ModeSymlink == 0 {
			stores = append(stores, filepath.Join(root, entry.Name()))
		}
	}
	return stores
}

// HistoryAccountRoot is where an account's stores live, for deleting them with the account.
func HistoryAccountRoot(cfg *config.Config, agent, account string) string {
	return historyAccountRoot(cfg, agent, account)
}
