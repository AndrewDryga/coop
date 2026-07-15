// Package liveprovider isolates the minimum credential state needed by opt-in live provider tests.
// It never recursively copies an agent home and never returns source paths or credential contents.
package liveprovider

import (
	"bufio"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/testutil/procharness"
)

const maxCredentialBytes = 4 << 20

// Selection is one source credential account to isolate. SourceDefault says the source env file
// belongs to this account; the destination always marks one selected account per provider default.
type Selection struct {
	Provider      string
	Account       string
	SourceDefault bool
}

// Prepared is an isolated credential config plus the opaque source-integrity baseline that made it.
type Prepared struct {
	ConfigDir    string
	accounts     map[string]string
	configured   map[string]bool
	envBacked    map[string]bool
	profileDirs  map[string]string
	key          [32]byte
	inputs       []sourceInput
	baseline     [][32]byte
	revokeMu     sync.Mutex
	revokePath   string
	revokeActive bool
}

var removeRevokedCredentialTree = os.RemoveAll

type sourceInput struct {
	provider string
	ordinal  int
	root     string
	path     string
}

type fileState struct {
	exists  bool
	data    []byte
	info    os.FileInfo
	mode    os.FileMode
	size    int64
	mtime   int64
	nlink   uint64
	dev     uint64
	ino     uint64
	pathNow os.FileInfo
}

// SelectionForTarget resolves one concrete account without changing the source config. A live
// compatibility prompt deliberately supports one account only; account ladders belong to the
// deterministic rotation suites.
func SelectionForTarget(cfg *config.Config, target agents.Target) (Selection, error) {
	if len(target.Accounts) > 1 {
		return Selection{}, fmt.Errorf("%s live target selects more than one account", target.Provider)
	}
	account := target.Account()
	if account == "" {
		account = cfg.DefaultProfileOf(target.Provider)
	}
	if err := validateSelection(target.Provider, account); err != nil {
		return Selection{}, err
	}
	return Selection{
		Provider: target.Provider, Account: account,
		SourceDefault: account == cfg.DefaultProfileOf(target.Provider),
	}, nil
}

// SelectionsForTargets expands account ladders into the exact unique credential accounts a live
// process may reach. A bare target selects only that provider's marked default.
func SelectionsForTargets(cfg *config.Config, targets []agents.Target) ([]Selection, error) {
	selections := make([]Selection, 0, len(targets))
	seen := map[string]bool{}
	for _, target := range targets {
		accounts := target.Accounts
		if len(accounts) == 0 {
			accounts = []string{cfg.DefaultProfileOf(target.Provider)}
		}
		for _, account := range accounts {
			selection, err := SelectionForTarget(cfg, agents.Target{
				Provider: target.Provider, Accounts: []string{account},
			})
			if err != nil {
				return nil, err
			}
			key := selectionKey(selection.Provider, selection.Account)
			if !seen[key] {
				seen[key] = true
				selections = append(selections, selection)
			}
		}
	}
	return selections, nil
}

// DefaultSelections returns exactly one selected credential account per registered provider. ACP
// live conformance switches providers, but it does not need authority from unrelated named accounts.
func DefaultSelections(cfg *config.Config) ([]Selection, error) {
	selections := make([]Selection, 0, len(agents.Names()))
	for _, provider := range agents.Names() {
		def := cfg.DefaultProfileOf(provider)
		if err := validateSelection(provider, def); err != nil {
			return nil, err
		}
		selections = append(selections, Selection{
			Provider: provider, Account: def, SourceDefault: true,
		})
	}
	return selections, nil
}

func validateSelection(provider, account string) error {
	if !agents.Valid(provider) {
		return fmt.Errorf("unknown live provider %q", provider)
	}
	parsed, err := agents.ParseTarget(provider + "@" + account)
	if err != nil || parsed.Account() != account {
		return fmt.Errorf("%s live target has an unsafe account name", provider)
	}
	return nil
}

// Prepare copies only selected adapter-declared auth material into destination. Destination must
// not exist. Construction happens in a sibling staging directory and becomes visible by rename only
// after source stability and destination inode checks pass.
func Prepare(sourceDir, destination string, selections []Selection) (*Prepared, error) {
	ordered, defaults, err := normalizeSelections(selections)
	if err != nil {
		return nil, err
	}
	if len(ordered) == 0 {
		return nil, errors.New("no live credential selections")
	}
	p := &Prepared{
		ConfigDir: destination, accounts: defaults, configured: map[string]bool{},
		envBacked: map[string]bool{}, profileDirs: map[string]string{},
	}
	if _, err := rand.Read(p.key[:]); err != nil {
		return nil, errors.New("create source-integrity key")
	}
	if err := p.ensureRevocationPathLocked(); err != nil {
		return nil, err
	}
	p.inputs, err = sourceInputs(sourceDir, ordered)
	if err != nil {
		return nil, err
	}
	p.baseline, err = p.snapshot()
	if err != nil {
		return nil, err
	}

	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, errors.New("create isolated credential parent")
	}
	if _, err := os.Lstat(destination); err == nil {
		return nil, errors.New("isolated credential destination already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("inspect isolated credential destination")
	}
	stage, err := os.MkdirTemp(parent, ".coop-live-credentials-")
	if err != nil {
		return nil, errors.New("create isolated credential staging directory")
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(stage)
		}
	}()
	if err := os.Chmod(stage, 0o700); err != nil {
		return nil, errors.New("secure isolated credential staging directory")
	}

	primaryPresent := map[string]bool{}
	activeEnvKeys := map[string][]string{}
	allowedEnvKeys := map[string][]string{}
	for _, selection := range ordered {
		ag, live, err := liveCredentialsFor(selection.Provider)
		if err != nil {
			return nil, err
		}
		profileDir := filepath.Join(stage, selection.Provider, "profiles", selection.Account)
		if err := mkdirPrivate(profileDir); err != nil {
			return nil, errors.New("create isolated credential account directory")
		}
		key := selectionKey(selection.Provider, selection.Account)
		for ordinal, artifact := range live.Artifacts {
			sourcePath := filepath.Join(sourceDir, selection.Provider, "profiles", selection.Account, artifact.Name)
			state, err := readSource(sourceDir, sourcePath, selection.Provider, ordinal, nil)
			if err != nil {
				return nil, err
			}
			if !state.exists {
				continue
			}
			if artifact.Primary {
				primaryPresent[key] = true
			}
			data, err := projectCredential(selection.Provider, ordinal, artifact, state.data)
			if err != nil {
				return nil, err
			}
			if data == nil {
				continue
			}
			if err := writeCopy(filepath.Join(profileDir, artifact.Name), data, state.info); err != nil {
				return nil, credentialError(selection.Provider, ordinal, err.Error())
			}
		}
		if selection.SourceDefault {
			activeEnvKeys[key] = ag.ActiveCredentialEnvKeys(profileDir, primaryPresent[key])
			allowedEnvKeys[selection.Provider] = activeEnvKeys[key]
		}
		p.profileDirs[key] = filepath.Join(destination, selection.Provider, "profiles", selection.Account)
	}
	envLines, envProviders, err := selectedEnv(sourceDir, allowedEnvKeys)
	if err != nil {
		return nil, err
	}
	for _, selection := range ordered {
		key := selectionKey(selection.Provider, selection.Account)
		envBacked := selection.SourceDefault && envProviders[selection.Provider]
		fileBacked := primaryPresent[key] && len(activeEnvKeys[key]) == 0
		p.configured[key] = fileBacked || envBacked
		p.envBacked[key] = envBacked
	}
	if len(envLines) > 0 {
		if err := writePrivate(filepath.Join(stage, "env"), []byte(strings.Join(envLines, "\n")+"\n")); err != nil {
			return nil, errors.New("write isolated credential environment")
		}
	}
	if err := writeDefaults(filepath.Join(stage, "defaults"), defaults); err != nil {
		return nil, err
	}
	afterCopy, err := p.snapshot()
	if err != nil {
		return nil, err
	}
	if !equalSnapshots(p.baseline, afterCopy) {
		return nil, errors.New("source credentials changed while copying")
	}
	if err := os.Rename(stage, destination); err != nil {
		return nil, errors.New("publish isolated credential directory")
	}
	keep = true
	return p, nil
}

// RevocationPath is the private, parent-known tombstone shared with the tagged timeout path.
func (p *Prepared) RevocationPath() (string, error) {
	if p == nil || p.ConfigDir == "" {
		return "", errors.New("missing isolated credential directory")
	}
	p.revokeMu.Lock()
	defer p.revokeMu.Unlock()
	if err := p.ensureRevocationPathLocked(); err != nil {
		return "", err
	}
	return p.revokePath, nil
}

func (p *Prepared) ensureRevocationPathLocked() error {
	if p.revokePath == "" {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return errors.New("create credential revocation name")
		}
		p.revokePath = filepath.Join(
			filepath.Dir(p.ConfigDir),
			".coop-live-revoked-"+fmt.Sprintf("%x", nonce[:]),
		)
		if _, err := os.Lstat(p.revokePath); err == nil || !errors.Is(err, os.ErrNotExist) {
			p.revokePath = ""
			return errors.New("reserve credential revocation name")
		}
	}
	return nil
}

// Revoke atomically removes the published credential path, then deletes the renamed private tree.
// Source fingerprints remain usable because they describe the original vault, never ConfigDir.
// A failed tree removal is retryable by this process even when the tagged child performed the move.
func (p *Prepared) Revoke() error {
	if p == nil || p.ConfigDir == "" {
		return errors.New("missing isolated credential directory")
	}
	p.revokeMu.Lock()
	defer p.revokeMu.Unlock()
	if err := p.ensureRevocationPathLocked(); err != nil {
		return err
	}
	if !p.revokeActive {
		if err := os.Rename(p.ConfigDir, p.revokePath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				info, statErr := os.Lstat(p.revokePath)
				if errors.Is(statErr, os.ErrNotExist) {
					return nil
				}
				if statErr != nil {
					return errors.New("inspect revoked credential directory")
				}
				stat, ok := info.Sys().(*syscall.Stat_t)
				if !info.IsDir() || info.Mode().Perm()&0o077 != 0 || !ok || int(stat.Uid) != os.Getuid() {
					return errors.New("invalid revoked credential directory")
				}
				p.revokeActive = true
			} else {
				return errors.New("revoke isolated credential directory")
			}
		} else {
			p.revokeActive = true
		}
	}
	if err := removeRevokedCredentialTree(p.revokePath); err != nil {
		return errors.New("remove revoked credential directory")
	}
	p.revokeActive = false
	return nil
}

func normalizeSelections(selections []Selection) ([]Selection, map[string]string, error) {
	ordered := append([]Selection(nil), selections...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Provider != ordered[j].Provider {
			return ordered[i].Provider < ordered[j].Provider
		}
		return ordered[i].Account < ordered[j].Account
	})
	defaults := map[string]string{}
	seen := map[string]bool{}
	for _, selection := range ordered {
		if err := validateSelection(selection.Provider, selection.Account); err != nil {
			return nil, nil, err
		}
		key := selectionKey(selection.Provider, selection.Account)
		if seen[key] {
			return nil, nil, fmt.Errorf("%s live credentials select one account twice", selection.Provider)
		}
		seen[key] = true
		if defaults[selection.Provider] == "" || selection.SourceDefault {
			defaults[selection.Provider] = selection.Account
		}
	}
	return ordered, defaults, nil
}

func sourceInputs(sourceDir string, selections []Selection) ([]sourceInput, error) {
	inputs := make([]sourceInput, 0, len(selections)*2+1)
	envNeeded := false
	for _, selection := range selections {
		_, live, err := liveCredentialsFor(selection.Provider)
		if err != nil {
			return nil, err
		}
		for ordinal, artifact := range live.Artifacts {
			inputs = append(inputs, sourceInput{
				provider: selection.Provider, ordinal: ordinal, root: sourceDir,
				path: filepath.Join(sourceDir, selection.Provider, "profiles", selection.Account, artifact.Name),
			})
		}
		envNeeded = envNeeded || selection.SourceDefault
	}
	if envNeeded {
		inputs = append(inputs, sourceInput{provider: "environment", ordinal: 0, root: sourceDir, path: filepath.Join(sourceDir, "env")})
	}
	return inputs, nil
}

func selectedEnv(sourceDir string, allowedByProvider map[string][]string) ([]string, map[string]bool, error) {
	allowed := map[string]string{}
	for provider, keys := range allowedByProvider {
		for _, key := range keys {
			if owner := allowed[key]; owner != "" && owner != provider {
				return nil, nil, errors.New("live credential environment has ambiguous ownership")
			}
			allowed[key] = provider
		}
	}
	configured := map[string]bool{}
	if len(allowed) == 0 {
		return nil, configured, nil
	}
	state, err := readSource(sourceDir, filepath.Join(sourceDir, "env"), "environment", 0, nil)
	if err != nil || !state.exists {
		return nil, configured, err
	}
	values := map[string]string{}
	scanner := bufio.NewScanner(strings.NewReader(string(state.data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, explicit := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		_, ok := allowed[key]
		if !explicit || !ok {
			continue
		}
		if strings.TrimSpace(value) == "" {
			delete(values, key)
			continue
		}
		values[key] = key + "=" + value
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, credentialError("environment", 0, "cannot parse")
	}
	selected := make([]string, 0, len(allowedByProvider))
	for provider, keys := range allowedByProvider {
		selectedKey := ""
		for _, key := range keys {
			if values[key] == "" {
				continue
			}
			if selectedKey != "" {
				return nil, nil, credentialError(provider, 0, "has ambiguous active environment credentials")
			}
			selectedKey = key
		}
		if selectedKey != "" {
			selected = append(selected, selectedKey)
			configured[provider] = true
		}
	}
	sort.Strings(selected)
	lines := make([]string, 0, len(selected))
	for _, key := range selected {
		lines = append(lines, values[key])
	}
	return lines, configured, nil
}

func (p *Prepared) snapshot() ([][32]byte, error) {
	digests := make([][32]byte, 0, len(p.inputs))
	for _, input := range p.inputs {
		state, err := readSource(input.root, input.path, input.provider, input.ordinal, nil)
		if err != nil {
			return nil, err
		}
		h := hmac.New(sha256.New, p.key[:])
		writeFingerprintField(h, input.provider)
		writeFingerprintInt(h, int64(input.ordinal))
		if state.exists {
			writeFingerprintInt(h, 1)
			writeFingerprintInt(h, int64(state.mode))
			writeFingerprintInt(h, state.size)
			writeFingerprintInt(h, state.mtime)
			writeFingerprintInt(h, int64(state.nlink))
			writeFingerprintInt(h, int64(state.dev))
			writeFingerprintInt(h, int64(state.ino))
			_, _ = h.Write(state.data)
		}
		var digest [32]byte
		copy(digest[:], h.Sum(nil))
		digests = append(digests, digest)
	}
	return digests, nil
}

// VerifySources fails when any selected source credential changed after Prepare. The error reveals
// no source location, account, artifact name, token, or digest.
func (p *Prepared) VerifySources() error {
	current, err := p.snapshot()
	if err != nil || !equalSnapshots(p.baseline, current) {
		return errors.New("source credentials changed during live provider test")
	}
	return nil
}

// CredentialPresent reports whether the isolated account has a primary file or an explicit env
// assignment belonging to that account in the source config.
func (p *Prepared) CredentialPresent(provider, account string) bool {
	return p.configured[selectionKey(provider, account)]
}

// SafeThrough reports whether using this isolated credential through deadline cannot require a
// refresh of copied remote state. Explicit env/API-key credentials are safe; file credentials must
// prove an access token remains valid through the deadline in their adapter.
func (p *Prepared) SafeThrough(provider, account string, deadline time.Time) bool {
	return p.PreflightReason(provider, account, deadline) == ""
}

// PreflightReason returns the stable prerequisite skip reason for an isolated credential.
func (p *Prepared) PreflightReason(provider, account string, deadline time.Time) string {
	key := selectionKey(provider, account)
	if !p.configured[key] {
		return ReasonMissingCredential
	}
	if p.envBacked[key] {
		return ""
	}
	_, live, err := liveCredentialsFor(provider)
	if err != nil {
		return ReasonUnsafeCredential
	}
	return portabilityReason(live.Portability(p.profileDirs[key], deadline))
}

func portabilityReason(status agents.CredentialPortability) string {
	switch status {
	case agents.CredentialPortable:
		return ""
	case agents.CredentialRefreshRequired:
		return ReasonCredentialRefresh
	case agents.CredentialNotPortable:
		return ReasonCredentialNotPortable
	default:
		return ReasonUnsafeCredential
	}
}

func liveCredentialsFor(provider string) (agents.Agent, agents.LiveCredentialSpec, error) {
	ag, ok := agents.Get(provider)
	if !ok {
		return nil, agents.LiveCredentialSpec{}, fmt.Errorf("%s live credential contract is unavailable", provider)
	}
	live := ag.LiveCredentials()
	if len(live.Artifacts) == 0 || live.Portability == nil || len(live.AuthSignals) == 0 {
		return nil, agents.LiveCredentialSpec{}, fmt.Errorf("%s live credential contract is unavailable", provider)
	}
	return ag, live, nil
}

func projectCredential(provider string, ordinal int, artifact agents.CredentialArtifact, data []byte) ([]byte, error) {
	if artifact.Project == nil {
		return nil, credentialError(provider, ordinal, "projection failed")
	}
	projected, err := artifact.Project(data)
	if err != nil {
		return nil, credentialError(provider, ordinal, "projection failed")
	}
	return projected, nil
}

// Account returns the account marked default in the isolated config for provider.
func (p *Prepared) Account(provider string) string { return p.accounts[provider] }

func equalSnapshots(a, b [][32]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !hmac.Equal(a[i][:], b[i][:]) {
			return false
		}
	}
	return true
}

func readSource(root, path, provider string, ordinal int, afterOpen func()) (fileState, error) {
	canonicalRoot, canonicalPath, err := canonicalSourcePath(root, path)
	if err != nil {
		return fileState{}, credentialError(provider, ordinal, "escapes its credential root")
	}
	f, err := procharness.OpenRegularFile(canonicalRoot, canonicalPath, os.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		return fileState{}, nil
	}
	if err != nil {
		return fileState{}, credentialError(provider, ordinal, "is not a regular single-link file")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fileState{}, credentialError(provider, ordinal, "cannot be inspected")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fileState{}, credentialError(provider, ordinal, "has unsafe permissions")
	}
	if info.Size() > maxCredentialBytes {
		return fileState{}, credentialError(provider, ordinal, "exceeds the size limit")
	}
	if afterOpen != nil {
		afterOpen()
	}
	data, err := io.ReadAll(io.LimitReader(f, maxCredentialBytes+1))
	if err != nil {
		return fileState{}, credentialError(provider, ordinal, "cannot be read")
	}
	if len(data) > maxCredentialBytes {
		return fileState{}, credentialError(provider, ordinal, "exceeds the size limit")
	}
	after, err := f.Stat()
	if err != nil || !os.SameFile(info, after) || info.Mode() != after.Mode() || info.Size() != after.Size() || !info.ModTime().Equal(after.ModTime()) {
		return fileState{}, credentialError(provider, ordinal, "changed while reading")
	}
	if _, err := procharness.CanonicalUnderRoot(canonicalRoot, canonicalPath); err != nil {
		return fileState{}, credentialError(provider, ordinal, "changed while reading")
	}
	pathNow, err := os.Lstat(canonicalPath)
	if err != nil || pathNow.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, pathNow) {
		return fileState{}, credentialError(provider, ordinal, "changed while reading")
	}
	nlink, ok := linkCount(after)
	if !ok || nlink != 1 {
		return fileState{}, credentialError(provider, ordinal, "is not a regular single-link file")
	}
	dev, ino, ok := fileIdentity(after)
	if !ok {
		return fileState{}, credentialError(provider, ordinal, "has no stable file identity")
	}
	return fileState{
		exists: true, data: data, info: after, mode: after.Mode(), size: after.Size(),
		mtime: after.ModTime().UnixNano(), nlink: nlink, dev: dev, ino: ino, pathNow: pathNow,
	}, nil
}

// canonicalSourcePath preserves the caller's declared root relationship while normalizing host
// aliases such as macOS /var -> /private/var. Symlinks below root remain visible to the no-follow
// walk and are rejected by OpenRegularFile.
func canonicalSourcePath(root, path string) (string, string, error) {
	cleanRoot, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return "", "", err
	}
	cleanPath, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", "", err
	}
	rel, err := filepath.Rel(cleanRoot, cleanPath)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", errors.New("source path is outside root")
	}
	canonicalRoot, err := filepath.EvalSymlinks(cleanRoot)
	if err != nil {
		return "", "", err
	}
	return canonicalRoot, filepath.Join(canonicalRoot, rel), nil
}

func credentialError(provider string, ordinal int, reason string) error {
	return fmt.Errorf("%s credential artifact %d %s", provider, ordinal+1, reason)
}

// CredentialDetailCode converts a redacted copier error to a stable operator diagnostic.
func CredentialDetailCode(err error) string {
	message := err.Error()
	switch {
	case strings.Contains(message, "projection failed"):
		return "credential_projection"
	case strings.Contains(message, "unsafe permissions"):
		return "credential_permissions"
	case strings.Contains(message, "size limit"):
		return "credential_size"
	case strings.Contains(message, "escapes"):
		return "credential_path"
	case strings.Contains(message, "changed"):
		return "credential_replaced"
	case strings.Contains(message, "regular single-link"):
		return "credential_file_type"
	default:
		return "credential_isolation"
	}
}

func mkdirPrivate(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}

func writeCopy(path string, data []byte, source os.FileInfo) error {
	if err := writePrivate(path, data); err != nil {
		return errors.New("cannot be copied")
	}
	destination, err := os.Lstat(path)
	if err != nil || destination.Mode().Perm() != 0o600 || !destination.Mode().IsRegular() || os.SameFile(source, destination) {
		return errors.New("did not produce a distinct private file")
	}
	if links, ok := linkCount(destination); !ok || links != 1 {
		return errors.New("did not produce a distinct private file")
	}
	return nil
}

func writePrivate(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	ok = true
	return nil
}

func writeDefaults(path string, defaults map[string]string) error {
	providers := make([]string, 0, len(defaults))
	for provider := range defaults {
		providers = append(providers, provider)
	}
	sort.Strings(providers)
	var body strings.Builder
	for _, provider := range providers {
		body.WriteString(provider + "=" + defaults[provider] + "\n")
	}
	if err := writePrivate(path, []byte(body.String())); err != nil {
		return errors.New("write isolated credential defaults")
	}
	return nil
}

func linkCount(info os.FileInfo) (uint64, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(stat.Nlink), true
}

func fileIdentity(info os.FileInfo) (uint64, uint64, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return uint64(stat.Dev), uint64(stat.Ino), true
}

func selectionKey(provider, account string) string { return provider + "\x00" + account }

func writeFingerprintField(w io.Writer, value string) {
	writeFingerprintInt(w, int64(len(value)))
	_, _ = io.WriteString(w, value)
}

func writeFingerprintInt(w io.Writer, value int64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(value))
	_, _ = w.Write(encoded[:])
}
