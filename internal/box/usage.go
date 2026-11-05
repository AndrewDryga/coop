package box

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// Quota inspection selects the same authority as a run but does not impose the filtered API-key
// broker's separate network/support restrictions. It never returns credential values to CLI.
func UsageQuotaAuthority(cfg *config.Config, provider, account string) (agents.UsageQuotaInput, error) {
	ag, ok := agents.Get(provider)
	if !ok {
		return agents.UsageQuotaInput{}, errors.New("unknown quota provider")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	selected, exists, err := previewNativeAuthority(ctx, cfg, ag, account)
	if err != nil {
		return agents.UsageQuotaInput{}, err
	}
	if exists && (selected.canonical || selected.record == nil) {
		input := agents.UsageQuotaInput{APIKey: selected.state.APIKey}
		if selected.canonical {
			input.Current = func(ctx context.Context, deadline time.Time) (agents.NativeCredentialState, map[string][]byte, error) {
				record, err := renewNativeAccount(ctx, cfg, ag, account, deadline, deadline)
				if err != nil {
					return agents.NativeCredentialState{}, nil, err
				}
				state, err := ag.NativeCredentials().Inspect(record.Artifacts, time.Now())
				return state, record.Artifacts, err
			}
		}
		return input, nil
	}
	home := cfg.AgentProfileDir(provider, account)
	input := agents.UsageQuotaInput{}
	marker := profileMarkerPresent(ag, home)
	values, err := effectiveCredentialEnv(cfg, RunSpec{Homes: true}, ag, provider, account, marker)
	if err != nil {
		return input, errors.New("selected authentication authority is unavailable")
	}
	for _, key := range ag.ActiveCredentialEnvKeys(home, marker) {
		if strings.TrimSpace(values[key]) == "" {
			continue
		}
		if input.EnvKey != "" {
			return input, errors.New("more than one authentication authority is selected")
		}
		input.EnvKey = key
		input.APIKey = true
	}
	return input, nil
}

// Native OAuth refresh rotates the original store. Keep usage's writer out of ordinary Coop
// runs (including mounted consult peers), without copying refresh authority into another home.
type credentialLease struct {
	file *os.File
	key  string
}

func (lease *credentialLease) close() {
	_ = syscall.Flock(int(lease.file.Fd()), syscall.LOCK_UN)
	_ = lease.file.Close()
}

func credentialUseLease(ctx context.Context, cfg *config.Config, profile string, exclusive bool) (*credentialLease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	canonical, err := filepath.EvalSymlinks(profile)
	if err != nil {
		return nil, errors.New("credential home is unavailable")
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return nil, errors.New("credential home is unavailable")
	}
	dir := filepath.Join(cfg.ConfigDir, "credential-use")
	if err := config.EnsurePrivateDir(dir); err != nil {
		return nil, errors.New("credential lease is unavailable")
	}
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(canonical)))
	path := filepath.Join(dir, key+".lock")
	// A host-only lock nothing unlinks: the flock on the inode the name answers is the lease.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, errors.New("credential lease is unavailable")
	}
	mode := syscall.LOCK_SH
	if exclusive {
		mode = syscall.LOCK_EX
	}
	for {
		if ctx.Err() != nil {
			file.Close()
			return nil, errors.New("credential is busy — try again after the active run")
		}
		err = syscall.Flock(int(file.Fd()), mode|syscall.LOCK_NB)
		if err == nil {
			return &credentialLease{file: file, key: key}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			file.Close()
			return nil, errors.New("credential lease is unavailable")
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
		}
	}
}

func runCredentialUseLeases(cfg *config.Config, rt runtime.Runtime, spec *RunSpec) (func(), error) {
	var releases []func()
	release := func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
	// An import of retained history by an older build that never finished leaves the home unusable.
	if spec.Homes && !spec.Login {
		ctx := spec.Ctx
		if ctx == nil {
			ctx = context.Background()
		}
		for _, name := range credentialScope(cfg, *spec) {
			home, err := cfg.NativeHome(name)
			if err != nil {
				continue
			}
			lease, err := credentialUseLease(ctx, cfg, home, false)
			if err != nil {
				release()
				return nil, err
			}
			releases = append(releases, lease.close)
			if err := checkNativeHistorySettled(home); err != nil {
				release()
				return nil, err
			}
		}
	}
	return release, nil
}
