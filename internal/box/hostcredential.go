package box

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
)

const (
	hostCredentialsDir      = "host-credentials"
	hostCredentialSizeLimit = 4 << 10
)

// SaveHostCredential persists an adapter-declared API key outside the provider profile mounted
// into boxes. The selector is staged privately and only credential artifacts are
// published. Any old serving authority must complete its recoverable cutover.
func SaveHostCredential(cfg *config.Config, ag agents.Agent, profile string, secret []byte) error {
	spec := ag.HostCredential()
	if err := validateHostCredentialSpec(ag, spec); err != nil {
		return err
	}
	if err := validateHostCredentialSecret(secret); err != nil {
		return err
	}
	stage, root, err := NativeSignInStage(cfg)
	if err != nil {
		return err
	}
	stage.SetActiveProfile(ag.Name(), profile)
	home := stage.AgentDir(ag.Name())
	if err := config.EnsurePrivateDir(home); err != nil {
		return err
	}
	if err := spec.Activate(home); err != nil {
		return err
	}
	if err := config.WriteFileAtomic(filepath.Join(home, spec.File), secret); err != nil {
		return err
	}
	if err := ImportNativeSignIn(context.Background(), cfg, ag.Name(), profile, home); err != nil {
		return fmt.Errorf("credential publication failed; sign-in output retained at %s: %w", root, err)
	}
	return os.RemoveAll(root)
}

// LoadHostCredential returns one selected account's adapter-declared env credential. Missing is a
// normal false result. Present-but-unsafe state is an error so launch fails closed rather than
// falling back to a different account or the provider's unusable native store.
func LoadHostCredential(cfg *config.Config, ag agents.Agent, profile string) (envKey, value string, found bool, err error) {
	spec := ag.HostCredential()
	if !spec.Declared() {
		return "", "", false, nil
	}
	if err := validateHostCredentialSpec(ag, spec); err != nil {
		return "", "", false, err
	}
	if record, exists, err := readNativeAccount(context.Background(), cfg, ag, profile); exists {
		if err != nil {
			return "", "", false, err
		}
		if record.Revoked {
			return "", "", false, nil
		}
		state, err := ag.NativeCredentials().Inspect(record.Artifacts, time.Now())
		if err != nil {
			return "", "", false, err
		}
		if state.APIKey && state.Ready {
			return spec.EnvKey, state.AccessToken, true, nil
		}
		return "", "", false, nil
	}
	// A key that builds before v11 kept in host-credentials/<account> is refused, never read.
	return "", "", false, refuseLegacyLogin(cfg, ag, profile)
}

// RemoveHostCredential removes only the exact adapter-owned secret for an explicitly confirmed
// account deletion. The native profile is removed separately by the caller.
func RemoveHostCredential(cfg *config.Config, ag agents.Agent, profile string) error {
	spec := ag.HostCredential()
	if !spec.Declared() {
		return nil
	}
	dir, err := hostCredentialDir(cfg, ag.Name(), profile)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove private %s credential: %w", ag.DisplayName(), err)
	}
	return nil
}

// ProjectHostCredential copies the Coop-held API key one account runs on into another config's
// vault — the host-side store a session child brokers it from, never a home a box mounts — and
// returns the file it wrote, for its owner to remove. An account whose key Coop does not hold (or
// does not select) writes nothing.
func ProjectHostCredential(from, to *config.Config, ag agents.Agent, profile string) (string, error) {
	path, err := SelectedHostCredentialPath(from, ag, profile)
	if err != nil || path == "" {
		return "", err
	}
	_, value, found, err := LoadHostCredential(from, ag, profile)
	if err != nil || !found {
		return "", err
	}
	if err := SaveHostCredential(to, ag, profile, []byte(value)); err != nil {
		return "", err
	}
	return SelectedHostCredentialPath(to, ag, profile)
}

// SelectedHostCredentialPath identifies the selected account's credential input without
// modifying it. An account with no canonical record has none; one that only builds before v11
// signed in is refused.
func SelectedHostCredentialPath(cfg *config.Config, ag agents.Agent, profile string) (string, error) {
	spec := ag.HostCredential()
	if !spec.Declared() {
		return "", nil
	}
	if err := validateHostCredentialSpec(ag, spec); err != nil {
		return "", err
	}
	if record, exists, err := readNativeAccountSnapshot(cfg, ag, profile); exists {
		if err != nil {
			return "", err
		}
		if record.Revoked {
			return "", nil
		}
		state, err := ag.NativeCredentials().Inspect(record.Artifacts, time.Now())
		if err != nil || !state.APIKey {
			return "", err
		}
		return filepath.Join(cfg.NativeAuthorityConfig().ConfigDir, ag.Name(), "credentials", profile, "authority.json"), nil
	}
	return "", refuseLegacyLogin(cfg, ag, profile)
}

func hostCredentialSelected(ag agents.Agent, profileDir string, markerPresent bool) bool {
	spec := ag.HostCredential()
	return spec.Declared() && slices.Contains(ag.ActiveCredentialEnvKeys(profileDir, markerPresent), spec.EnvKey)
}

func hostCredentialDir(cfg *config.Config, agent, profile string) (string, error) {
	if profile == "" || profile == "." || profile == ".." || profile[0] == '-' || filepath.Base(profile) != profile ||
		filepath.IsAbs(profile) {
		return "", fmt.Errorf("invalid %s account name", agent)
	}
	base := filepath.Join(cfg.ConfigDir, agent, hostCredentialsDir)
	dir := filepath.Join(base, profile)
	if filepath.Dir(dir) != base {
		return "", fmt.Errorf("invalid %s account name", agent)
	}
	return dir, nil
}

func validateHostCredentialSpec(ag agents.Agent, spec agents.HostCredentialSpec) error {
	if !spec.Valid() || !slices.Contains(ag.CredentialEnvKeys(), spec.EnvKey) {
		return fmt.Errorf("%s has an invalid host credential declaration", ag.DisplayName())
	}
	return nil
}

func validateHostCredentialSecret(secret []byte) error {
	if len(secret) == 0 {
		return errors.New("API key cannot be empty")
	}
	if len(secret) > hostCredentialSizeLimit {
		return errors.New("API key is too large")
	}
	if bytes.ContainsAny(secret, "\x00\r\n") {
		return errors.New("API key cannot contain a line break")
	}
	return nil
}
