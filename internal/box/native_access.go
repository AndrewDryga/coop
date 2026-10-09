package box

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/safefile"
)

// NativeAccessSnapshot is a nonrenewable host-side copy for isolated, bounded live
// tests. It is never a workload mount; normal runs use the continuously renewed broker.
type NativeAccessSnapshot struct {
	Files      map[string][]byte
	Configured bool
	ExpiresAt  time.Time
	APIKey     bool
	Ready      bool
}

// SnapshotNativeAccess reads only existing canonical authority. It never renews,
// migrates or falls back; found remains true for revoked or unusable authority.
func SnapshotNativeAccess(ctx context.Context, cfg *config.Config, provider, account string) (NativeAccessSnapshot, bool, error) {
	var out NativeAccessSnapshot
	ag, ok := agents.Get(provider)
	if !ok {
		return out, false, errors.New("unknown native account provider")
	}
	if err := ctx.Err(); err != nil {
		return out, false, err
	}
	record, found, err := readNativeAccountSnapshot(cfg, ag, account)
	if err != nil || !found || record.Revoked {
		return out, found, err
	}
	native := ag.NativeCredentials()
	state, err := native.Inspect(record.Artifacts, time.Now())
	if err != nil {
		return out, true, err
	}
	out.Configured = true
	out.ExpiresAt, out.APIKey = state.ExpiresAt, state.APIKey
	// A valid refresh-only account is configured but cannot be copied for use.
	if state.AccessToken == "" {
		return out, true, nil
	}
	out.Files = make(map[string][]byte)
	for _, artifact := range native.Artifacts {
		data, exists := record.Artifacts[artifact.Name]
		if !exists {
			continue
		}
		if artifact.AccessOnly == nil {
			return NativeAccessSnapshot{}, true, errors.New("native access export is not declared")
		}
		out.Files[artifact.Name], err = artifact.AccessOnly(data)
		if err != nil {
			return NativeAccessSnapshot{}, true, err
		}
	}
	projected, err := native.Inspect(out.Files, time.Now())
	if err != nil || projected.Refreshable || projected.Principal != state.Principal || projected.Selection != state.Selection {
		return NativeAccessSnapshot{}, true, errors.New("native access export changed identity or retained renewal authority")
	}
	out.ExpiresAt, out.APIKey, out.Ready = projected.ExpiresAt, projected.APIKey, projected.Ready
	return out, true, nil
}

// Unlike a serving read, qualification cannot create a lock or recover custody.
// Its caller also fingerprints the source before and after preparing the copy.
func readNativeAccountSnapshot(cfg *config.Config, ag agents.Agent, account string) (*accountAuthority, bool, error) {
	if !accountNameValid(account) {
		return nil, false, errors.New("invalid native account selection")
	}
	cfg = cfg.NativeAuthorityConfig()
	path := filepath.Join(cfg.ConfigDir, ag.Name(), "credentials", account)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	} else if err != nil {
		return nil, true, err
	}
	base, err := safefile.OpenRoot(cfg.ConfigDir)
	if err != nil {
		return nil, true, err
	}
	root := &accountAuthorityRoot{path: cfg.ConfigDir, base: base}
	defer root.close()
	if err := privateAccountDirectory(base); err != nil {
		return nil, true, err
	}
	parent := base
	for _, name := range []string{ag.Name(), "credentials", account} {
		dir, err := safefile.OpenDir(parent, name)
		if err != nil {
			return nil, true, err
		}
		root.pins = append(root.pins, accountDirectoryPin{parent: parent, name: name, dir: dir})
		if err := privateAccountDirectory(dir); err != nil {
			return nil, true, err
		}
		parent = dir
	}
	if pending, err := root.readRenewal(); err != nil || pending != nil {
		return nil, true, errors.Join(errAccountRenewalUncertain, err)
	}
	record, err := root.read(nativeAccountSpec(ag), account)
	if err != nil {
		return nil, true, err
	}
	if record == nil {
		return nil, true, errors.New("canonical account initialization is incomplete")
	}
	if err := root.checkNamed(); err != nil {
		return nil, true, err
	}
	return record, true, nil
}
