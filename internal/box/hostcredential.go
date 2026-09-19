package box

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
)

const (
	hostCredentialsDir      = "host-credentials"
	hostCredentialSizeLimit = 4 << 10
)

// SaveHostCredential persists an adapter-declared API key outside the provider profile mounted
// into boxes. The selected profile still receives its non-secret auth selector, then the key lands
// atomically behind owner-only ancestors. Existing provider credential files are never touched.
func SaveHostCredential(cfg *config.Config, ag agents.Agent, profile string, secret []byte) error {
	spec := ag.HostCredential()
	if err := validateHostCredentialSpec(ag, spec); err != nil {
		return err
	}
	if err := validateHostCredentialSecret(secret); err != nil {
		return err
	}
	dir, err := hostCredentialDir(cfg, ag.Name(), profile)
	if err != nil {
		return err
	}
	if err := EnsureProfilesDir(cfg, ag.Name()); err != nil {
		return fmt.Errorf("prepare %s account: %w", ag.DisplayName(), err)
	}
	profileDir := cfg.AgentProfileDir(ag.Name(), profile)
	if err := config.EnsurePrivateDir(profileDir); err != nil {
		return fmt.Errorf("prepare %s account: %w", ag.DisplayName(), err)
	}
	for _, path := range []string{
		cfg.ConfigDir,
		filepath.Join(cfg.ConfigDir, ag.Name()),
		filepath.Join(cfg.ConfigDir, ag.Name(), hostCredentialsDir),
		dir,
	} {
		if err := config.EnsurePrivateDir(path); err != nil {
			return fmt.Errorf("prepare private %s credential storage: %w", ag.DisplayName(), err)
		}
	}
	if err := spec.Activate(profileDir); err != nil {
		return err
	}
	if err := config.WriteFileAtomic(filepath.Join(dir, spec.File), secret); err != nil {
		return fmt.Errorf("save %s API key: %w", ag.DisplayName(), err)
	}
	return nil
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
	dir, err := hostCredentialDir(cfg, ag.Name(), profile)
	if err != nil {
		return "", "", false, err
	}
	before, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("inspect %s credential storage: %w", ag.DisplayName(), err)
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 || before.Mode().Perm()&0o077 != 0 {
		return "", "", false, fmt.Errorf("%s credential storage must be a private real directory", ag.DisplayName())
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", "", false, fmt.Errorf("open %s credential storage: %w", ag.DisplayName(), err)
	}
	defer root.Close()
	after, err := root.Stat(".")
	if err != nil || !os.SameFile(before, after) {
		return "", "", false, fmt.Errorf("%s credential storage changed while opening", ag.DisplayName())
	}
	info, err := root.Lstat(spec.File)
	if errors.Is(err, os.ErrNotExist) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("inspect %s API key: %w", ag.DisplayName(), err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return "", "", false, fmt.Errorf("%s API key must be an owner-only regular file", ag.DisplayName())
	}
	if info.Size() <= 0 || info.Size() > hostCredentialSizeLimit {
		return "", "", false, fmt.Errorf("%s API key has an invalid size", ag.DisplayName())
	}
	f, err := root.Open(spec.File)
	if err != nil {
		return "", "", false, fmt.Errorf("open %s API key: %w", ag.DisplayName(), err)
	}
	opened, statErr := f.Stat()
	if statErr != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() {
		_ = f.Close()
		return "", "", false, fmt.Errorf("%s API key changed while opening", ag.DisplayName())
	}
	data, readErr := io.ReadAll(io.LimitReader(f, hostCredentialSizeLimit+1))
	closeErr := f.Close()
	if readErr != nil || closeErr != nil {
		return "", "", false, fmt.Errorf("read %s API key", ag.DisplayName())
	}
	if err := validateHostCredentialSecret(data); err != nil {
		return "", "", false, fmt.Errorf("stored %s API key is invalid", ag.DisplayName())
	}
	return spec.EnvKey, string(data), true, nil
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
	profileDir := from.AgentProfileDir(ag.Name(), profile)
	if !hostCredentialSelected(ag, profileDir, profileMarkerPresent(ag, profileDir)) {
		return "", nil
	}
	_, value, found, err := LoadHostCredential(from, ag, profile)
	if err != nil || !found {
		return "", err
	}
	dir, err := hostCredentialDir(to, ag.Name(), profile)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, ag.HostCredential().File), SaveHostCredential(to, ag, profile, []byte(value))
}

func hostCredentialMtime(cfg *config.Config, ag agents.Agent, profile string) (os.FileInfo, bool) {
	spec := ag.HostCredential()
	if !spec.Declared() {
		return nil, false
	}
	dir, err := hostCredentialDir(cfg, ag.Name(), profile)
	if err != nil {
		return nil, false
	}
	info, err := os.Lstat(filepath.Join(dir, spec.File))
	return info, err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm()&0o077 == 0
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
