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
	"github.com/AndrewDryga/coop/internal/safefile"
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
		},
		Complete: native.Complete}
}

// nativeSignInStagePrefix names every sign-in staging folder, directly under the configuration folder.
const nativeSignInStagePrefix = ".sign-in-"

// NativeSignInStage has no existing credentials, repository or user history.
// The caller removes it when the sign-in stops or fails, keeps it and names it when saving fails, and
// removes a successful stage only after canonical publication is confirmed. RemoveNativeSignInStage
// is how it goes.
func NativeSignInStage(cfg *config.Config) (*config.Config, string, error) {
	if cfg == nil || !filepath.IsAbs(cfg.ConfigDir) {
		return nil, "", errors.New("invalid sign-in storage root")
	}
	if err := config.EnsurePrivateDir(cfg.ConfigDir); err != nil {
		return nil, "", err
	}
	root, err := os.MkdirTemp(cfg.ConfigDir, nativeSignInStagePrefix)
	if err != nil {
		return nil, "", err
	}
	stage := cfg.Clone()
	stage.ConfigDir = root
	return stage, root, nil
}

// RemoveNativeSignInStage removes the one sign-in staging folder it is given, with everything the
// provider left in it, and nothing else. A sign-in that is stopped, cancelled by a signal or
// finished runs this on whatever path it ends by, and the staging folder holds the provider's own
// files, a half-written credential among them, so the removal checks what it is about to delete: a
// real folder named like a staging folder, directly under the configuration folder. The
// configuration folder itself, anything else in it, a path elsewhere and a link are refused, and
// the stage's siblings, another sign-in's stage among them, are never touched. A stage that is
// already gone is not an error.
func RemoveNativeSignInStage(cfg *config.Config, root string) error {
	if cfg == nil || !filepath.IsAbs(cfg.ConfigDir) || !filepath.IsAbs(root) {
		return errors.New("invalid sign-in storage root")
	}
	if filepath.Dir(root) != filepath.Clean(cfg.ConfigDir) || !strings.HasPrefix(filepath.Base(root), nativeSignInStagePrefix) {
		return fmt.Errorf("refusing to remove %s: it is not a sign-in staging folder of %s", root, cfg.ConfigDir)
	}
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("refusing to remove %s: it is not a folder", root)
	}
	return os.RemoveAll(root)
}

// ErrNoNativeSignIn says a provider's sign-in ended without writing the file that holds the login:
// it was stopped, or closed before it finished, so there is nothing to save. A sign-in that wrote a
// file Coop cannot use is a different failure, and stays one.
var ErrNoNativeSignIn = errors.New("the sign-in wrote no login")

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
		// An interrupted first setup leaves only its lock and the temporary files of a publication
		// that never landed: no account yet, so the next sign-in starts it afresh.
		unpublished, err := root.holdsOnlyUnpublished()
		if err != nil || !unpublished {
			return nil, true, errors.Join(fmt.Errorf("canonical account initialization is incomplete; preserve %s and restore a verified account backup before retrying", path), err)
		}
		return nil, false, nil
	}
	return record, true, nil
}

// holdsOnlyUnpublished reports whether the account folder holds nothing but its lock and the
// temporary files of a publication that never landed.
func (root *accountAuthorityRoot) holdsOnlyUnpublished() (bool, error) {
	entries, err := os.ReadDir(root.path)
	if err != nil {
		return false, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	for _, name := range names {
		if name != ".authority.lock" && !strings.HasPrefix(name, ".authority-") {
			return false, nil
		}
	}
	return true, nil
}

func nativeAccountReady(cfg *config.Config, ag agents.Agent, account string, now time.Time) (bool, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	record, exists, err := readNativeAccount(ctx, cfg, ag, account)
	if exists && accountBusy(err) {
		// Another process held the account longer than a status check waits: a renewal asking its
		// provider, a sign-in or a removal. That says nothing against the sign-in, so it reads as
		// ready; a launch waits for the lock and judges what that process left.
		return true, true
	}
	// A grant the provider refused still inspects as renewable, so its refusal is checked first.
	if !exists || err != nil || record.Revoked ||
		refusedIn(filepath.Join(cfg.NativeAuthorityConfig().ConfigDir, ag.Name(), "credentials", account), record) {
		return false, exists
	}
	state, err := ag.NativeCredentials().Inspect(record.Artifacts, now)
	return err == nil && state.Ready, true
}

// accountBusy reports a read that found the account's lock held by another process for as long as
// the reader would wait. Only the lock wait gives up this way; a damaged or unsafe record does not.
func accountBusy(err error) bool {
	return errors.Is(err, errAccountAuthorityBusy) || errors.Is(err, context.DeadlineExceeded)
}

// ImportNativeSignIn publishes only declared credential artifacts from a
// completed trusted sign-in staging home. It never merges projects/settings.
func ImportNativeSignIn(ctx context.Context, cfg *config.Config, provider, account, source string) error {
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
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("read native sign-in artifact %s: %w: %w", artifact.Name, ErrNoNativeSignIn, err)
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
	// Explicit fresh sign-in may supersede a renewal whose kept answer cannot be used. The
	// replacement path revalidates canonical custody and archives that answer; ordinary readers and
	// automatic renewal still refuse it.
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
	return removeNativeAuthority(ctx, cfg, provider, account)
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
		if _, _, err := root.publish(spec, account, current, &accountAuthority{Epoch: epoch, Revoked: true}, nil); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(root.path)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if name != accountRenewalName && name != accountRetryName && name != accountRefusalName && !strings.HasPrefix(name, "recovery-") && !strings.HasPrefix(name, ".private-") && !strings.HasPrefix(name, ".authority-") && !strings.HasPrefix(name, ".renewal-") {
			continue
		}
		if err := os.Remove(filepath.Join(root.path, name)); err != nil {
			return err
		}
	}
	return nil
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

// errRenewalDeferred is a renewal ahead of the deadline that waits out an exchange that failed for a
// passing reason moments ago, whichever run made it, instead of asking the provider again.
var errRenewalDeferred = errors.New("native renewal waits for the account's next retry")

// renewNativeAccount returns the account's current record, renewing its access first when that
// expires before renewBy. The renewed access only has to outlast deadline, which is never later than
// renewBy; an access that cannot be renewed stays in use until it no longer outlasts the deadline. A
// renewal while the access still outlasts nativeRenewalDeadline waits its turn after a failed
// exchange (errRenewalDeferred). No caller's deadline is further off than that, so a renewal a
// deadline needs never waits.
func renewNativeAccount(ctx context.Context, cfg *config.Config, ag agents.Agent, account string, renewBy, deadline time.Time) (*accountAuthority, error) {
	cfg = cfg.NativeAuthorityConfig()
	native := ag.NativeCredentials()
	needed := func(current *accountAuthority, retryAt time.Time) (bool, error) {
		now := time.Now()
		state, err := native.Inspect(current.Artifacts, now)
		if err != nil {
			return false, err
		}
		if !state.Ready {
			return false, errors.New("account needs host sign-in")
		}
		lasts := func(t time.Time) bool {
			return state.AccessToken != "" && (state.ExpiresAt.IsZero() || state.ExpiresAt.After(t))
		}
		renewable := state.Refreshable && native.Renew != nil
		if lasts(renewBy) || lasts(deadline) && !renewable {
			return false, nil
		}
		if !renewable {
			return false, errors.New("account needs host sign-in")
		}
		// Judge from now, under the lock: getting it may have taken 30 s since the caller set deadline.
		if lasts(now.Add(nativeRenewalDeadline)) && now.Before(retryAt) {
			return false, errRenewalDeferred
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

// PrepareNativeAccount performs host-only readiness before a remote child is started.
// Environment-only keys remain in their original host config.
func PrepareNativeAccount(ctx context.Context, cfg *config.Config, provider, account string, deadline time.Time) (bool, error) {
	ag, ok := agents.Get(provider)
	if !ok {
		return false, errors.New("unknown native provider")
	}
	_, exists, err := ensureNativeAccount(ctx, cfg.NativeAuthorityConfig(), ag, account)
	if err != nil {
		return exists, err
	}
	if !exists {
		_, selected, err := previewNativeAuthority(ctx, cfg, ag, account)
		return selected, err
	}
	_, err = renewNativeAccount(ctx, cfg.NativeAuthorityConfig(), ag, account, deadline, deadline)
	return true, err
}

// ensureNativeAccount returns the account's canonical record, or refuses a login that builds before
// v11 saved in the account's older folders and nothing converted.
func ensureNativeAccount(ctx context.Context, cfg *config.Config, ag agents.Agent, account string) (*accountAuthority, bool, error) {
	cfg = cfg.NativeAuthorityConfig()
	record, exists, err := readNativeAccount(ctx, cfg, ag, account)
	if exists || err != nil {
		return record, exists, err
	}
	return nil, false, refuseLegacyLogin(cfg, ag, account)
}

// legacyLoginPresent reports a login that builds before v11 saved: the adapter's marker or Gemini's
// OAuth grant in profiles/<account>, or a Coop-held API key in host-credentials/<account>.
func legacyLoginPresent(cfg *config.Config, ag agents.Agent, account string) (bool, error) {
	marker, _ := ag.AuthMarker()
	home := cfg.AgentProfileDir(ag.Name(), account)
	paths := []string{filepath.Join(home, marker), filepath.Join(home, "oauth_creds.json")}
	if host := ag.HostCredential(); host.Declared() {
		dir, err := hostCredentialDir(cfg, ag.Name(), account)
		if err != nil {
			return false, err
		}
		paths = append(paths, filepath.Join(dir, host.File))
	}
	for _, path := range paths {
		if _, err := os.Lstat(path); err == nil {
			return true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return false, nil
}

// refuseLegacyLogin refuses an account whose only login is one that builds before v11 saved in
// profiles/<account> or host-credentials/<account>. Nothing reads those folders any more and
// nothing deletes them. A nil result means the account has no such login.
func refuseLegacyLogin(cfg *config.Config, ag agents.Agent, account string) error {
	present, err := legacyLoginPresent(cfg, ag, account)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	return fmt.Errorf("%s account %q is still signed in the way Coop before v11 saved it, and this Coop no longer converts it; run coop login %s@%s (the old folder stays untouched), or open it once with Coop v12.1.0 or earlier", ag.DisplayName(), account, ag.Name(), account)
}
