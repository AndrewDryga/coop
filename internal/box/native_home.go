package box

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/safefile"
	"golang.org/x/sys/unix"
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
// resume lookup and launch. Legacy sources are read-only migration inputs, never
// a fallback mounted home or a source of credentials for the native client.
func PrepareNativeHome(ctx context.Context, cfg *config.Config, rt runtime.Runtime, provider, account, repo string, acp bool) (string, error) {
	source := cfg.AgentProfileDir(provider, account)
	if acp {
		source = acpSharedDir(cfg, provider)
	}
	_, project, err := HistoryKey(repo)
	if err != nil {
		return "", err
	}
	return prepareNativeHomeFromSource(ctx, cfg, rt, provider, account, repo, acp, source, func(cwd string) bool {
		_, candidate, err := HistoryKey(cwd)
		return err == nil && candidate == project
	})
}

// PreparePrivateACPHome upgrades daemon-owned ACP profiles across all credential
// accounts. The caller supplies ownership only after validating the private
// session root, fork generation/reservation, and persisted native workdir marker.
func PreparePrivateACPHome(ctx context.Context, cfg *config.Config, rt runtime.Runtime, provider, account, repo string, ownsCWD func(string) bool) (string, error) {
	if _, ok := agents.Get(provider); !ok || ownsCWD == nil {
		return "", errors.New("invalid private ACP history selection")
	}
	sources := []string{acpSharedDir(cfg, provider)}
	profiles := filepath.Join(cfg.ConfigDir, provider, "profiles")
	root, err := safefile.OpenRoot(profiles)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if root != nil {
		defer root.Close()
		entries, err := root.ReadDir(-1)
		if err != nil {
			return "", err
		}
		for _, entry := range entries {
			if !accountNameValid(entry.Name()) {
				return "", errors.New("invalid retained private ACP account")
			}
			child, err := safefile.OpenDir(root, entry.Name())
			if err != nil {
				return "", err
			}
			err = privateAccountDirectory(child)
			_ = child.Close()
			if err != nil {
				return "", err
			}
			sources = append(sources, filepath.Join(profiles, entry.Name()))
		}
	}
	var home string
	for _, source := range sources {
		home, err = prepareNativeHomeFromSource(ctx, cfg, rt, provider, account, repo, true, source, ownsCWD)
		if err != nil {
			return "", err
		}
	}
	return home, nil
}

func prepareNativeHomeFromSource(ctx context.Context, cfg *config.Config, rt runtime.Runtime, provider, account, repo string, acp bool, source string, ownsCWD func(string) bool) (string, error) {
	ag, ok := agents.Get(provider)
	if !ok {
		return "", errors.New("unknown native home provider")
	}
	inspect := func() (agents.NativeHistoryPlan, error) {
		return ag.NativeHistory(source, ownsCWD)
	}
	plan, err := inspect()
	if err != nil {
		return "", fmt.Errorf("inspect retained %s history: %w", provider, err)
	}
	selected, err := NativeHomePath(cfg, provider, account, repo, acp)
	if err != nil {
		return "", err
	}
	complete, err := nativeHistoryImported(selected, source, plan)
	if err != nil {
		return "", err
	}
	if !complete {
		lease, err := credentialUseLease(ctx, cfg, source, true)
		if err != nil {
			return "", err
		}
		defer lease.close()
		if err := checkLegacyMountPaths(ctx, rt, true, source); err != nil {
			return "", err
		}
		// A stable hash is not proof that a paused writer has exited. Rescan
		// only after both the launch lease and runtime inventory are fenced.
		plan, err = inspect()
		if err != nil {
			return "", err
		}
	}
	home, err := prepareNativeHome(ctx, cfg, provider, account, repo, acp, func(home string) error {
		if defaults := ag.NativeCredentials().Defaults; defaults != nil {
			files, err := defaults(cfg.NativeAuthorityConfig().AgentProfileDir(provider, account))
			if err != nil {
				return err
			}
			for name, data := range files {
				if !accountNameValid(name) {
					return errors.New("invalid native defaults filename")
				}
				if err := config.WriteFileAtomicMode(filepath.Join(home, name), data, 0600); err != nil {
					return err
				}
			}
		}
		return ag.EnsureDefaults(cfg.WithNativeHomes(map[string]string{provider: home}), Workdir(cfg, repo))
	})
	if err != nil {
		return "", err
	}
	if complete {
		return home, nil
	}
	if !complete {
		homeLease, err := credentialUseLease(ctx, cfg, home, true)
		if err != nil {
			return "", err
		}
		defer homeLease.close()
		if err := checkLegacyMountPaths(ctx, rt, true, home); err != nil {
			return "", err
		}
	}
	if err := importNativeHistory(ctx, home, source, plan); err != nil {
		return "", err
	}
	return home, nil
}

// prepareNativeHome creates one complete native rename domain. The seed callback
// runs only for a new home, before publication; existing native settings are never
// re-seeded. An interrupted seed is retained and refused, not silently overwritten.
func prepareNativeHome(ctx context.Context, cfg *config.Config, provider, account, repo string, acp bool,
	seed func(string) error) (string, error) {
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
	home := filepath.Join(append([]string{cfg.ConfigDir}, append(parts, "home")...)...)
	file, err := safefile.OpenRegular(root.dir(), "owner.json")
	if err == nil {
		defer file.Close()
		if err := privateAccountFile(file); err != nil {
			return "", err
		}
		data, err := io.ReadAll(io.LimitReader(file, (16<<10)+1))
		if err != nil || len(data) > 16<<10 {
			return "", errors.New("native home ownership exceeds its bound or cannot be read")
		}
		var owner nativeHomeOwner
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&owner) != nil || decoder.Decode(new(any)) != io.EOF || owner != want {
			return "", errors.New("native home belongs to another repository or account")
		}
		child, err := safefile.OpenDir(root.dir(), "home")
		if err != nil {
			return "", fmt.Errorf("open published native home: %w", err)
		}
		defer child.Close()
		if err := privateAccountDirectory(child); err != nil {
			return "", err
		}
		if err := root.checkLock(lock); err != nil {
			return "", fmt.Errorf("confirm published native home custody: %w", err)
		}
		return home, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := unix.Mkdirat(int(root.dir().Fd()), "home", 0o700); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return "", fmt.Errorf("native home initialization is incomplete at %s — preserve it for recovery", home)
		}
		return "", err
	}
	child, err := safefile.OpenDir(root.dir(), "home")
	if err != nil {
		return "", fmt.Errorf("open initial native home: %w", err)
	}
	defer child.Close()
	if seed != nil {
		if err := seed(home); err != nil {
			return "", fmt.Errorf("initialize native home (retained at %s): %w", home, err)
		}
	}
	if err := root.checkLock(lock); err != nil {
		return "", fmt.Errorf("confirm initial native home custody: %w", err)
	}
	current, err := safefile.OpenDir(root.dir(), "home")
	if err != nil {
		return "", err
	}
	same := sameAccountInode(child, current)
	_ = current.Close()
	if !same {
		return "", errors.New("native home changed during initialization")
	}
	if err := privateAccountDirectory(child); err != nil {
		return "", err
	}
	data, err := json.Marshal(want)
	if err != nil {
		return "", err
	}
	fd, err := unix.Openat(int(root.dir().Fd()), "owner.json", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return "", err
	}
	record := os.NewFile(uintptr(fd), "owner.json")
	_, writeErr := record.Write(append(data, '\n'))
	if writeErr == nil {
		writeErr = record.Sync()
	}
	if err := errors.Join(writeErr, record.Close(), root.dir().Sync()); err != nil {
		return "", fmt.Errorf("native home publication needs recovery: %w", err)
	}
	if err := root.checkLock(lock); err != nil {
		return "", err
	}
	return home, nil
}
