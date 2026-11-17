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
	if err := stageHostCredential(home, spec, secret); err != nil {
		// Nothing was published; only a failed publication keeps (and names) the stage.
		return errors.Join(err, RemoveNativeSignInStage(cfg, root))
	}
	if err := ImportNativeSignIn(context.Background(), cfg, ag.Name(), profile, home); err != nil {
		return fmt.Errorf("credential publication failed; sign-in output retained at %s: %w", root, err)
	}
	return RemoveNativeSignInStage(cfg, root)
}

func stageHostCredential(home string, spec agents.HostCredentialSpec, secret []byte) error {
	if err := config.EnsurePrivateDir(home); err != nil {
		return err
	}
	if err := spec.Activate(home); err != nil {
		return err
	}
	return config.WriteFileAtomic(filepath.Join(home, spec.File), secret)
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

// RemoveHostCredential removes an account's pre-v11 host-credentials/<account> folder (Coop no
// longer writes or reads it) when the account is deleted. The canonical record and the native
// profile are removed separately by the caller.
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
