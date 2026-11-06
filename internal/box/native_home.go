package box

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/randid"
	"github.com/AndrewDryga/coop/internal/safefile"
)

// Ownership is outside the mounted home. A writable native index or settings file
// cannot change which repository/account the host will select on the next launch.
type nativeHomeOwner struct {
	Version  int    `json:"version"`
	Provider string `json:"provider"`
	Account  string `json:"account,omitempty"`
	Project  string `json:"project"`
	ACP      bool   `json:"acp"`
}

// PrepareNativeHome selects the complete native state domain before both host
// resume lookup and launch, creating it on first use.
func PrepareNativeHome(ctx context.Context, cfg *config.Config, provider, account, repo string, acp bool) (string, error) {
	ag, ok := agents.Get(provider)
	if !ok {
		return "", errors.New("unknown native home provider")
	}
	if _, err := NativeHomePath(cfg, provider, account, repo, acp); err != nil {
		return "", err
	}
	if !acp {
		// The lock sits beside the account's folder, so a name that leaves it would write the lock
		// wherever it points. Refuse it first.
		if name := account; name != "" && !accountNameValid(name) {
			return "", fmt.Errorf("invalid native account selection")
		}
		release, err := lockAccountHomes(ctx, HistoryAccountRoot(cfg, provider, account), false)
		if err != nil {
			return "", err
		}
		defer release()
	}
	inHome := func(home string) *config.Config { return cfg.WithNativeHomes(map[string]string{provider: home}) }
	var refresh func(string) error
	if refresher, ok := ag.(agents.DefaultsRefresher); ok {
		refresh = func(home string) error { return refresher.RefreshDefaults(inHome(home), Workdir(cfg, repo)) }
	}
	return prepareNativeHome(ctx, cfg, provider, account, repo, acp, func(home string) error {
		return ag.EnsureDefaults(inHome(home), Workdir(cfg, repo))
	}, refresh)
}

// setAsideUnpublishedHome moves a home whose setup never published to a unique name beside it. A
// home that anything used (its auth custody or the history imports of older builds exist) is refused instead.
func setAsideUnpublishedHome(root *accountAuthorityRoot) error {
	for _, used := range []string{"auth", "imports"} {
		if _, err := os.Lstat(filepath.Join(root.path, used)); !errors.Is(err, os.ErrNotExist) {
			return errors.Join(errors.New("the home was used before its setup finished; preserve it for recovery"), err)
		}
	}
	return os.Rename(filepath.Join(root.path, "home"), filepath.Join(root.path, ".home-abandoned-"+randid.Hex(8)))
}

// prepareNativeHome creates one complete native rename domain. The seed callback
// runs only for a new home, before publication; existing native settings are never
// re-seeded. A home whose setup never published is set aside and seeded again. The
// refresh callback, when there is one, runs for a published home at every launch, under the
// same lock: it fills what the agent itself dropped or a new folder never had, and never re-seeds.
func prepareNativeHome(ctx context.Context, cfg *config.Config, provider, account, repo string, acp bool,
	seed, refresh func(string) error) (string, error) {
	if cfg == nil || !accountNameValid(provider) {
		return "", errors.New("invalid native home selection")
	}
	key, project, err := HistoryKey(repo)
	if err != nil {
		return "", err
	}
	parts := []string{provider, "acp-homes", key}
	if acp {
		account = ""
	} else {
		if account == "" {
			account = config.DefaultProfile
		}
		parts = []string{provider, "native-homes", account, key}
	}
	root, err := openPrivateAccountTree(cfg.ConfigDir, parts...)
	if err != nil {
		return "", fmt.Errorf("open native home custody: %w", err)
	}
	defer root.close()
	lock, err := root.lock(ctx)
	if err != nil {
		return "", fmt.Errorf("lock native home custody: %w", err)
	}
	defer lock.Close()
	want := nativeHomeOwner{Version: 1, Provider: provider, Account: account, Project: project, ACP: acp}
	home := filepath.Join(root.path, "home")
	var owner nativeHomeOwner
	published, err := config.ReadRecord(filepath.Join(root.path, "owner.json"), &owner)
	if err != nil {
		return "", fmt.Errorf("read native home ownership: %w", err)
	}
	if published {
		if owner != want {
			return "", errors.New("native home belongs to another repository or account")
		}
		child, err := os.Open(home)
		if err != nil {
			return "", fmt.Errorf("open published native home: %w", err)
		}
		defer child.Close()
		// The box mounts this home and can widen its mode, and a sync service restores it with the
		// account's default (Dropbox writes 0755). Read bits alone come back off; anything wider,
		// or another owner, is refused.
		if _, err := config.TightenReadable(child); err != nil {
			return "", err
		}
		if err := privateAccountDirectory(child); err != nil {
			return "", err
		}
		if refresh != nil {
			if err := refresh(home); err != nil {
				return "", fmt.Errorf("refresh native home defaults at %s: %w", home, err)
			}
		}
		return home, nil
	}
	err = os.Mkdir(home, 0o700)
	if errors.Is(err, os.ErrExist) {
		// A first setup that never published left this home behind, and no box has seen it: set it
		// aside, never delete it, and set up afresh.
		if err = setAsideUnpublishedHome(root); err == nil {
			err = os.Mkdir(home, 0o700)
		}
	}
	if err != nil {
		return "", fmt.Errorf("native home initialization is incomplete at %s: %w", home, err)
	}
	if seed != nil {
		if err := seed(home); err != nil {
			return "", fmt.Errorf("initialize native home (retained at %s): %w", home, err)
		}
	}
	// The seed is provider code; never publish a home it left readable by others.
	child, err := os.Open(home)
	if err != nil {
		return "", fmt.Errorf("open initial native home: %w", err)
	}
	defer child.Close()
	if err := privateAccountDirectory(child); err != nil {
		return "", err
	}
	data, err := json.Marshal(want)
	if err != nil {
		return "", err
	}
	// All or nothing: a half-written owner record would refuse this home forever.
	if err := writeAccountPrivate(root, "owner.json", append(data, '\n')); err != nil {
		return "", fmt.Errorf("native home publication needs recovery: %w", err)
	}
	return home, nil
}

// checkNativeHistorySettled refuses a home whose history import by an older build never finished:
// this build cannot finish it, and launching would use a half-imported home. Checked under the
// run's shared home lease.
func checkNativeHistorySettled(home string) error {
	root, err := safefile.OpenRoot(filepath.Join(filepath.Dir(home), "imports"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	for {
		entries, readErr := root.ReadDir(128)
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			file, err := safefile.OpenRegular(root, entry.Name())
			if err != nil {
				return err
			}
			var receipt struct {
				Version int
				Done    bool
			}
			decodeErr := json.NewDecoder(file).Decode(&receipt)
			closeErr := file.Close()
			if decodeErr != nil || closeErr != nil || receipt.Version != 1 || !receipt.Done {
				return fmt.Errorf("the history import of %s was started by an older Coop and never finished; open this project once with a Coop build from before the pre-v11 history import was removed (see MIGRATING.md), then try again", filepath.Dir(home))
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}
