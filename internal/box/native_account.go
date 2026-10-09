package box

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/safefile"
	"golang.org/x/sys/unix"
)

func nativeAccountSpec(ag agents.Agent) accountAuthoritySpec {
	native := ag.NativeCredentials()
	files := make(map[string]int64, len(native.Artifacts))
	for _, artifact := range native.Artifacts {
		files[artifact.Name] = artifact.Limit
	}
	return accountAuthoritySpec{Provider: ag.Name(), Files: files,
		Inspect: func(files map[string][]byte) (string, string, error) {
			if native.Inspect == nil {
				return "", "", errors.New("provider has no native authority inspector")
			}
			state, err := native.Inspect(files, time.Now())
			return state.Selection, state.Principal, err
		}}
}

// NativeSignInStage has no existing credentials, repository or user history.
// The caller retains failed native output for explicit recovery and removes a
// successful stage only after canonical publication is confirmed.
func NativeSignInStage(cfg *config.Config) (*config.Config, string, error) {
	if cfg == nil || !filepath.IsAbs(cfg.ConfigDir) {
		return nil, "", errors.New("invalid sign-in storage root")
	}
	if err := config.EnsurePrivateDir(cfg.ConfigDir); err != nil {
		return nil, "", err
	}
	root, err := os.MkdirTemp(cfg.ConfigDir, ".sign-in-")
	if err != nil {
		return nil, "", err
	}
	stage := cfg.Clone()
	stage.ConfigDir = root
	return stage, root, nil
}

// Readiness never falls back to old model-writable files after canonical state
// exists, including a revoked, interrupted or unreadable canonical account.
func readNativeAccount(ctx context.Context, cfg *config.Config, ag agents.Agent, account string) (*accountAuthority, bool, error) {
	cfg = cfg.NativeAuthorityConfig()
	path := filepath.Join(cfg.ConfigDir, ag.Name(), "credentials", account)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	} else if err != nil {
		return nil, true, err
	}
	root, lock, record, err := lockAccountAuthority(ctx, cfg, nativeAccountSpec(ag), account)
	if err != nil {
		return nil, true, err
	}
	defer root.close()
	defer lock.Close()
	if record == nil {
		return nil, true, fmt.Errorf("canonical account initialization is incomplete; preserve %s and restore a verified account backup before retrying", path)
	}
	return record, true, nil
}

func nativeAccountReady(cfg *config.Config, ag agents.Agent, account string, now time.Time) (bool, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	record, exists, err := readNativeAccount(ctx, cfg, ag, account)
	if !exists || err != nil || record.Revoked {
		return false, exists
	}
	state, err := ag.NativeCredentials().Inspect(record.Artifacts, now)
	return err == nil && state.Ready, true
}

// ImportNativeSignIn publishes only declared credential artifacts from a
// completed trusted sign-in staging home. It never merges projects/settings.
func ImportNativeSignIn(ctx context.Context, cfg *config.Config, provider, account, source string, runtimes ...runtime.Runtime) error {
	if len(runtimes) > 1 {
		return errors.New("sign-in requires one runtime inventory")
	}
	ag, ok := agents.Get(provider)
	if !ok {
		return errors.New("unknown account provider")
	}
	native := ag.NativeCredentials()
	root, err := safefile.OpenRoot(source)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := privateAccountDirectory(root); err != nil {
		return err
	}
	files := make(map[string][]byte)
	for _, artifact := range native.Artifacts {
		data, err := safefile.ReadRegular(root, artifact.Name, artifact.Limit)
		if errors.Is(err, os.ErrNotExist) && !artifact.Required {
			continue
		}
		if err != nil {
			return fmt.Errorf("read native sign-in artifact %s: %w", artifact.Name, err)
		}
		files[artifact.Name] = data
	}
	record, err := normalizeNativeAccount(ag, files)
	if err != nil {
		return err
	}
	var rt runtime.Runtime
	if len(runtimes) == 1 {
		rt = runtimes[0]
	}
	// A fresh login must not leave an older serving refresh authority behind.
	// Fence and retain it through the same recoverable cutover before replacing
	// the canonical epoch. Unsafe legacy inputs leave this sign-in stage intact.
	if _, _, err := ensureNativeAccount(ctx, cfg, rt, ag, account); err != nil && !errors.Is(err, errAccountRenewalUncertain) {
		return fmt.Errorf("retire previous account authority before sign-in: %w", err)
	}
	// Explicit fresh sign-in may supersede uncertain renewal. The replacement
	// path revalidates canonical custody and archives the received response;
	// ordinary readers and automatic renewal still refuse that uncertainty.
	_, _, err = replaceAccountAuthority(ctx, cfg, nativeAccountSpec(ag), account, record)
	return err
}

func normalizeNativeAccount(ag agents.Agent, files map[string][]byte) (*accountAuthority, error) {
	native := ag.NativeCredentials()
	var err error
	if native.Select != nil {
		files, err = native.Select(files)
		if err != nil {
			return nil, err
		}
	}
	for _, artifact := range native.Artifacts {
		data, exists := files[artifact.Name]
		if !exists && !artifact.Required {
			continue
		}
		if len(data) == 0 || artifact.Import == nil {
			return nil, errors.New("native sign-in inventory is incomplete")
		}
		files[artifact.Name], err = artifact.Import(data)
		if err != nil {
			return nil, err
		}
	}
	state, err := native.Inspect(files, time.Now())
	if err != nil {
		return nil, err
	}
	if !state.Ready {
		return nil, errors.New("native sign-in did not produce a usable account")
	}
	return &accountAuthority{Selection: state.Selection, Principal: state.Principal, Artifacts: files}, nil
}

// RemoveNativeAccount advances the epoch before any history is deleted. Existing
// broker snapshots can no longer be renewed for the removed identity.
func RemoveNativeAccount(ctx context.Context, cfg *config.Config, provider, account string) error {
	if err := removeNativeAuthority(ctx, cfg, provider, account); err != nil {
		return err
	}
	path := filepath.Join(cfg.ConfigDir, provider, "cutovers", account)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	root, err := openPrivateAccountTree(cfg.ConfigDir, provider, "cutovers", account)
	if err != nil {
		return err
	}
	defer root.close()
	lock, err := root.lock(ctx)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := root.checkLock(lock); err != nil {
		return err
	}
	entries, err := root.dir().ReadDir(-1)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if name != "cutover.json" && !strings.HasPrefix(name, ".private-") {
			continue
		}
		file, err := safefile.OpenRegular(root.dir(), name)
		if err != nil {
			return err
		}
		err = privateAccountFile(file)
		_ = file.Close()
		if err != nil {
			return err
		}
		if err := unix.Unlinkat(int(root.dir().Fd()), name, 0); err != nil {
			return err
		}
	}
	return errors.Join(root.dir().Sync(), root.checkLock(lock))
}

func removeNativeAuthority(ctx context.Context, cfg *config.Config, provider, account string) error {
	ag, ok := agents.Get(provider)
	if !ok {
		return errors.New("unknown account provider")
	}
	spec := nativeAccountSpec(ag)
	root, lock, current, err := lockAccountAuthorityState(ctx, cfg, spec, account)
	if err != nil {
		return err
	}
	defer root.close()
	defer lock.Close()
	if current == nil || !current.Revoked {
		epoch := uint64(1)
		if current != nil {
			epoch = current.Epoch + 1
		}
		if _, _, err := root.publish(lock, spec, account, current, &accountAuthority{Epoch: epoch, Revoked: true}, nil); err != nil {
			return err
		}
	}
	entries, err := root.dir().ReadDir(-1)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if name != accountRenewalName && name != "cutover.json" && !strings.HasPrefix(name, "recovery-") && !strings.HasPrefix(name, ".private-") && !strings.HasPrefix(name, ".authority-") && !strings.HasPrefix(name, ".renewal-") {
			continue
		}
		file, err := safefile.OpenRegular(root.dir(), name)
		if err != nil {
			return err
		}
		err = privateAccountFile(file)
		_ = file.Close()
		if err != nil {
			return err
		}
		if err := root.checkLock(lock); err != nil {
			return err
		}
		if err := unix.Unlinkat(int(root.dir().Fd()), name, 0); err != nil {
			return err
		}
	}
	return errors.Join(root.dir().Sync(), root.checkLock(lock))
}

// NativeAccountRemoved lets a confirmed removal retry finish cleanup while a
// tombstone stays absent from selectable accounts.
func NativeAccountRemoved(cfg *config.Config, provider, account string) bool {
	ag, ok := agents.Get(provider)
	if !ok {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	record, exists, err := readNativeAccount(ctx, cfg, ag, account)
	return err == nil && exists && record != nil && record.Revoked
}

func renewNativeAccount(ctx context.Context, cfg *config.Config, ag agents.Agent, account string, deadline time.Time) (*accountAuthority, error) {
	cfg = cfg.NativeAuthorityConfig()
	native := ag.NativeCredentials()
	needed := func(current *accountAuthority) (bool, error) {
		state, err := native.Inspect(current.Artifacts, time.Now())
		if err != nil {
			return false, err
		}
		if !state.Ready {
			return false, errors.New("account needs host sign-in")
		}
		if state.AccessToken != "" && (state.ExpiresAt.IsZero() || state.ExpiresAt.After(deadline)) {
			return false, nil
		}
		if !state.Refreshable || native.Renew == nil {
			return false, errors.New("account needs host sign-in")
		}
		return true, nil
	}
	record, _, err := renewAccountAuthority(ctx, cfg, nativeAccountSpec(ag), account, needed,
		func(current *accountAuthority, retain func([]byte) error) (*accountAuthority, error) {
			files, err := native.Renew(ctx, current.Artifacts, deadline, retain)
			if files == nil {
				return nil, err
			}
			current.Artifacts = files
			return current, err
		})
	return record, err
}

// PrepareNativeAccount performs host-only readiness/cutover before a remote
// child is started. Environment-only keys remain in their original host config.
func PrepareNativeAccount(ctx context.Context, cfg *config.Config, rt runtime.Runtime, provider, account string, deadline time.Time) (bool, error) {
	ag, ok := agents.Get(provider)
	if !ok {
		return false, errors.New("unknown native provider")
	}
	_, exists, err := ensureNativeAccount(ctx, cfg.NativeAuthorityConfig(), rt, ag, account)
	if err != nil {
		return exists, err
	}
	if !exists {
		_, selected, err := previewNativeAuthority(ctx, cfg, ag, account)
		return selected, err
	}
	_, err = renewNativeAccount(ctx, cfg.NativeAuthorityConfig(), ag, account, deadline)
	return true, err
}
